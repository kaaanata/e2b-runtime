package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/apierrors"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// memoryHotplugBlockMB is the granularity of hotplugged memory; a request
// must keep the hotplugged part (requested - boot memory) a multiple of it.
const memoryHotplugBlockMB = int64(2)

// validateSandboxMemory checks a requested guest memory for a sandbox whose
// template boots with ramMB and may grow to maxRamMB (nil: fixed size, no
// hotplug device), against the team tier. requested is what the caller asks
// for after defaults were applied.
func validateSandboxMemory(limits *authtypes.TeamLimits, ramMB int64, maxRamMB *int64, requested int64) *api.APIError {
	if maxRamMB == nil {
		if requested == ramMB {
			return nil
		}

		return &api.APIError{
			Code:      http.StatusConflict,
			ErrorCode: orchestrator.ErrCodeMemoryNotHotResizable,
			ClientMsg: fmt.Sprintf("The template's memory is fixed at %d MiB; build it with maxMemoryMB to resize sandboxes", ramMB),
			Err:       fmt.Errorf("memory %d MiB requested for a template fixed at %d MiB", requested, ramMB),
		}
	}

	bad := func(format string, args ...any) *api.APIError {
		msg := fmt.Sprintf(format, args...)

		return &api.APIError{Code: http.StatusBadRequest, ClientMsg: msg, Err: fmt.Errorf("%s", msg)}
	}

	switch {
	case requested < ramMB:
		return bad("memoryMB (%d) can't be lower than the template's memoryMB (%d)", requested, ramMB)
	case requested > *maxRamMB:
		return bad("memoryMB (%d) can't be higher than the template's maxMemoryMB (%d)", requested, *maxRamMB)
	case limits != nil && requested > limits.MaxRamMb:
		return bad("memoryMB can't be higher than %d MiB (if you need to increase this limit, please contact support)", limits.MaxRamMb)
	case (requested-ramMB)%memoryHotplugBlockMB != 0:
		return bad("memoryMB - the template's memoryMB (%d) must be a multiple of %d MiB", requested-ramMB, memoryHotplugBlockMB)
	}

	return nil
}

// requestedOrDefault returns the requested memory, or def when none was given.
func requestedOrDefault(requested *api.MemoryMB, def int64) int64 {
	if requested == nil {
		return def
	}

	return int64(*requested)
}

// snapshotMemoryDefault is the size a paused sandbox resumes with when the
// request names none: what it had when paused, else the boot memory.
func snapshotMemoryDefault(ramMB int64, requestedRamMB *int64) int64 {
	if requestedRamMB != nil && *requestedRamMB > 0 {
		return *requestedRamMB
	}

	return ramMB
}

// memoryResizeOrchestrator is what PatchSandboxesSandboxID needs from the
// orchestrator; tests substitute it (see APIStore.resizeBackendOverride).
type memoryResizeOrchestrator interface {
	GetSandbox(ctx context.Context, teamID uuid.UUID, sandboxID string) (sandbox.Sandbox, error)
	ResizeSandboxMemory(ctx context.Context, teamID uuid.UUID, sandboxID string, memoryMB int64) (sandbox.Sandbox, *api.APIError)
	ResizeSandboxCPU(ctx context.Context, teamID uuid.UUID, sandboxID string, vcpus int64) (sandbox.Sandbox, *api.APIError)
}

func (a *APIStore) resizeBackend() memoryResizeOrchestrator {
	if a.resizeBackendOverride != nil {
		return a.resizeBackendOverride
	}

	return a.orchestrator
}

// PatchSandboxesSandboxID changes a running sandbox in place: its guest
// memory, hotplugged into the running guest for a template built with
// maxMemoryMB, and/or its vCPU count for a template built with maxCpuCount.
// Both are validated before either is applied; memory goes first.
func (a *APIStore) PatchSandboxesSandboxID(c *gin.Context, id string) {
	ctx := c.Request.Context()

	teamInfo := auth.MustGetTeamInfo(c)
	team := teamInfo.Team

	sandboxID, err := utils.ShortID(id)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid sandbox ID")

		return
	}

	telemetry.ReportEvent(ctx, "update sandbox", telemetry.WithSandboxID(sandboxID))

	body, err := ginutils.ParseBody[api.SandboxUpdate](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	if body.MemoryMB == nil && body.CpuCount == nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Nothing to update: memoryMB or cpuCount is required")

		return
	}

	backend := a.resizeBackend()

	sbx, err := backend.GetSandbox(ctx, team.ID, sandboxID)
	if err != nil || sbx.TeamID != team.ID {
		a.sendAPIStoreError(c, http.StatusNotFound, utils.SandboxNotFoundMsg(sandboxID))

		return
	}

	if sbx.State != sandbox.StateRunning {
		a.sendAPIStoreError(c, http.StatusConflict, utils.SandboxChangingStateMsg(sandboxID, sbx.State))

		return
	}

	if body.MemoryMB != nil {
		if apiErr := validateSandboxMemory(teamInfo.Limits, sbx.RamMB, sbx.MaxMemoryMB, int64(*body.MemoryMB)); apiErr != nil {
			apierrors.SendAPIError(c, apiErr)

			return
		}
	}

	if body.CpuCount != nil {
		if apiErr := validateSandboxCPU(teamInfo.Limits, sbx.VCpu, sbx.MaxCPUCount, int64(*body.CpuCount)); apiErr != nil {
			apierrors.SendAPIError(c, apiErr)

			return
		}
	}

	updated := sbx
	if body.MemoryMB != nil {
		var apiErr *api.APIError
		updated, apiErr = backend.ResizeSandboxMemory(ctx, team.ID, sandboxID, int64(*body.MemoryMB))
		if apiErr != nil {
			telemetry.ReportErrorByCode(ctx, apiErr.Code, "error resizing sandbox memory", apiErr.Err, telemetry.WithSandboxID(sandboxID))
			apierrors.SendAPIError(c, apiErr)

			return
		}
	}

	if body.CpuCount != nil {
		var apiErr *api.APIError
		updated, apiErr = backend.ResizeSandboxCPU(ctx, team.ID, sandboxID, int64(*body.CpuCount))
		if apiErr != nil {
			telemetry.ReportErrorByCode(ctx, apiErr.Code, "error resizing sandbox vCPUs", apiErr.Err, telemetry.WithSandboxID(sandboxID))
			apierrors.SendAPIError(c, apiErr)

			return
		}
	}

	domain, apiErr := a.sandboxDomain(teamInfo)
	if apiErr != nil {
		apierrors.SendAPIError(c, apiErr)

		return
	}

	c.JSON(http.StatusOK, runningSandboxDetail(updated, domain))
}

// sandboxDomain is the base domain the team's sandboxes are reachable at.
func (a *APIStore) sandboxDomain(team *authtypes.Team) (*string, *api.APIError) {
	if team.ClusterID == nil || a.clusters == nil {
		return nil, nil
	}

	cluster, ok := a.clusters.GetClusterById(*team.ClusterID)
	if !ok {
		return nil, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: fmt.Sprintf("cluster with id %s not found", *team.ClusterID),
			Err:       fmt.Errorf("cluster with ID '%s' not found", *team.ClusterID),
		}
	}

	return cluster.SandboxDomain, nil
}

// runningSandboxDetail renders a running sandbox's record as the API detail.
func runningSandboxDetail(sbx sandbox.Sandbox, domain *string) api.SandboxDetail {
	state := api.Running
	if sbx.State == sandbox.StatePausing {
		state = api.Paused
	}

	detail := api.SandboxDetail{
		ClientID:            sbx.ClientID,
		TemplateID:          sbx.BaseTemplateID,
		Alias:               sbx.Alias,
		SandboxID:           sbx.SandboxID,
		StartedAt:           sbx.StartTime,
		CpuCount:            api.CPUCount(sbx.RequestedVCPUs()),
		MemoryMB:            api.MemoryMB(sbx.RequestedMemoryMB()),
		DiskSizeMB:          api.DiskSizeMB(sbx.TotalDiskSizeMB),
		EndAt:               sbx.EndTime,
		State:               state,
		EnvdVersion:         sbx.EnvdVersion,
		EnvdAccessToken:     sbx.EnvdAccessToken,
		AllowInternetAccess: sbx.AllowInternetAccess,
		Domain:              domain,
		Network:             dbNetworkConfigToAPI(sbx.Network),
		Lifecycle:           sandboxLifecycleToAPI(sbx.AutoPause, sbx.AutoResume),
		VolumeMounts:        convertFromDBMountsToAPIMounts(sbx.VolumeMounts),
	}
	setRunningMemory(&detail.CurrentMemoryMB, &detail.MaxMemoryMB, sbx)
	setRunningCPU(&detail.CurrentCpuCount, &detail.MaxCpuCount, sbx)

	if sbx.Metadata != nil {
		meta := api.SandboxMetadata(sbx.Metadata)
		detail.Metadata = &meta
	}

	return detail
}

// setRunningMemory fills the hotplug fields of a running sandbox's API view:
// only a hot-resizable sandbox reports them.
func setRunningMemory(current **api.MemoryMB, maxMB **api.MaxMemoryMB, sbx sandbox.Sandbox) {
	if !sbx.HotResizable() {
		return
	}

	*maxMB = new(api.MaxMemoryMB(*sbx.MaxMemoryMB))
	if sbx.CurrentMemoryMB > 0 {
		*current = new(api.MemoryMB(sbx.CurrentMemoryMB))
	}
}

// pausedMemory renders the memory fields of a paused sandbox from its
// snapshot build row.
func pausedMemory(ramMB int64, maxRamMB, requestedRamMB *int64) (memoryMB api.MemoryMB, maxMemoryMB *api.MaxMemoryMB) {
	memoryMB = api.MemoryMB(snapshotMemoryDefault(ramMB, requestedRamMB))
	if maxRamMB != nil {
		maxMemoryMB = new(api.MaxMemoryMB(*maxRamMB))
	}

	return memoryMB, maxMemoryMB
}
