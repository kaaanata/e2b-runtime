package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func (a *APIStore) GetVolumesVolumeID(c *gin.Context, volumeID api.VolumeID) {
	volume, team, ok := a.getVolume(c, volumeID)
	if !ok {
		return
	}

	if volume.Backend == volumeBackendBlock {
		// Block volumes have no content API: their files are reached through
		// a sandbox that mounts them, so no content token is signed.
		c.JSON(http.StatusOK, volumeAndTokenToAPI(volume, "", nil))

		return
	}

	domain, domainErr := a.volumeContentDomain(team)
	if domainErr != nil {
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "Cluster not found")
		telemetry.ReportError(c.Request.Context(), "cluster not found", domainErr)

		return
	}

	token, err := generateVolumeContentToken(a.config.VolumesToken, volume, team, a.volumeTokenAudience(domain))
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "failed to sign token")
		telemetry.ReportCriticalError(c.Request.Context(), "failed to sign token", err)

		return
	}

	c.JSON(http.StatusOK, volumeAndTokenToAPI(volume, token, domain))
}

// volumeAndTokenToAPI renders a volume with its content token. Block volumes
// get an empty token and contentApi=unsupported.
func volumeAndTokenToAPI(volume queries.Volume, token string, domain *string) api.VolumeAndToken {
	v := volumeToAPI(volume)

	contentAPI := api.Supported
	if volume.Backend == volumeBackendBlock {
		contentAPI = api.Unsupported
		token = ""
		domain = nil
	}

	return api.VolumeAndToken{
		VolumeID:    v.VolumeID,
		Name:        v.Name,
		Token:       token,
		Domain:      domain,
		Backend:     v.Backend,
		State:       v.State,
		CapacityGiB: v.CapacityGiB,
		OwnerNode:   v.OwnerNode,
		ContentApi:  &contentAPI,
	}
}
