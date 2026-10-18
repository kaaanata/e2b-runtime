package handlers

import (
	"fmt"
	"net/http"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
)

// validateSandboxCPU checks a requested vCPU count for a sandbox whose
// template boots vcpu CPUs online and may grow to maxVCPU (nil: fixed count,
// no CPU hot resize), against the team tier. requested is what the caller
// asks for after defaults were applied.
func validateSandboxCPU(limits *authtypes.TeamLimits, vcpu int64, maxVCPU *int64, requested int64) *api.APIError {
	if maxVCPU == nil {
		if requested == vcpu {
			return nil
		}

		return &api.APIError{
			Code:      http.StatusConflict,
			ErrorCode: orchestrator.ErrCodeCPUNotHotResizable,
			ClientMsg: fmt.Sprintf("The template's vCPU count is fixed at %d; build it with maxCpuCount to resize sandboxes", vcpu),
			Err:       fmt.Errorf("%d vCPUs requested for a template fixed at %d", requested, vcpu),
		}
	}

	bad := func(format string, args ...any) *api.APIError {
		msg := fmt.Sprintf(format, args...)

		return &api.APIError{Code: http.StatusBadRequest, ClientMsg: msg, Err: fmt.Errorf("%s", msg)}
	}

	switch {
	case requested < vcpu:
		return bad("cpuCount (%d) can't be lower than the template's cpuCount (%d)", requested, vcpu)
	case requested > *maxVCPU:
		return bad("cpuCount (%d) can't be higher than the template's maxCpuCount (%d)", requested, *maxVCPU)
	case limits != nil && requested > limits.MaxVcpu:
		return bad("cpuCount can't be higher than %d (if you need to increase this limit, please contact support)", limits.MaxVcpu)
	}

	return nil
}

// cpuRequestedOrDefault returns the requested vCPU count, or def when none
// was given.
func cpuRequestedOrDefault(requested *api.CPUCount, def int64) int64 {
	if requested == nil {
		return def
	}

	return int64(*requested)
}

// snapshotCPUDefault is the count a paused sandbox resumes with when the
// request names none: what it was granted when paused, else the template's.
func snapshotCPUDefault(vcpu int64, requestedVCPU *int64) int64 {
	if requestedVCPU != nil && *requestedVCPU > 0 {
		return *requestedVCPU
	}

	return vcpu
}

// setRunningCPU fills the CPU hot resize fields of a running sandbox's API
// view: only a CPU-hot-resizable sandbox reports them.
func setRunningCPU(current **api.CPUCount, maxCPU **api.MaxCPUCount, sbx sandbox.Sandbox) {
	if !sbx.CPUHotResizable() {
		return
	}

	*maxCPU = new(api.MaxCPUCount(*sbx.MaxCPUCount))
	if sbx.CurrentCPUCount > 0 {
		*current = new(api.CPUCount(sbx.CurrentCPUCount))
	}
}

// pausedCPU renders the vCPU fields of a paused sandbox from its snapshot
// build row.
func pausedCPU(vcpu int64, maxVCPU, requestedVCPU *int64) (cpuCount api.CPUCount, maxCPUCount *api.MaxCPUCount) {
	cpuCount = api.CPUCount(snapshotCPUDefault(vcpu, requestedVCPU))
	if maxVCPU != nil {
		maxCPUCount = new(api.MaxCPUCount(*maxVCPU))
	}

	return cpuCount, maxCPUCount
}
