package handlers

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/e2b-dev/infra/packages/api/internal/api"
)

// volumeResizes validates the volumeMounts of a resume or connect request
// and returns the requested capacities (volume name -> bytes). Whether each
// name is a block volume of the snapshot, and whether it grows, is checked
// against the snapshot when the resume is planned.
func (a *APIStore) volumeResizes(mounts *[]api.ResumeVolumeMount) (map[string]int64, *api.APIError) {
	if mounts == nil || len(*mounts) == 0 {
		return nil, nil
	}

	out := make(map[string]int64, len(*mounts))

	for _, m := range *mounts {
		if m.Name == "" {
			return nil, &api.APIError{Code: http.StatusBadRequest, ClientMsg: "volumeMounts: name is required"}
		}

		if _, dup := out[m.Name]; dup {
			return nil, &api.APIError{Code: http.StatusBadRequest, ClientMsg: fmt.Sprintf("volumeMounts: volume '%s' is listed twice", m.Name)}
		}

		if !slices.Contains(a.config.BlockVolumeCapacityTiersGiB, m.CapacityGiB) {
			return nil, &api.APIError{Code: http.StatusBadRequest, ClientMsg: fmt.Sprintf("volumeMounts: capacityGiB of volume '%s' must be one of %v", m.Name, a.config.BlockVolumeCapacityTiersGiB)}
		}

		out[m.Name] = m.CapacityGiB << 30
	}

	return out, nil
}

// errResizeRunning refuses volumeMounts on a sandbox that is running: a
// volume is only resized while its sandbox is paused.
func errResizeRunning(sandboxID string) *api.APIError {
	return &api.APIError{
		Code:      http.StatusConflict,
		ErrorCode: errCodeVolumeResizeInvalid,
		ClientMsg: fmt.Sprintf("Sandbox '%s' is running; its volumes can only be resized while it is paused", sandboxID),
	}
}
