package team

import (
	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/constants"
	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

// LimitMaxCPU validates a template build's CPU hot resize maximum against its
// vCPU count and the team tier. It returns nil for a fixed vCPU count (no
// maximum, or one equal to cpuCount), which is what is stored. The
// template-manager re-validates it (Firecracker's ceiling, SMT parity).
func LimitMaxCPU(limits *types.TeamLimits, cpuCount int64, maxCPUCount *int32) (*int64, *api.APIError) {
	if maxCPUCount == nil {
		return nil, nil
	}

	maxCPU := int64(*maxCPUCount)

	switch {
	case maxCPU == cpuCount:
		return nil, nil
	case maxCPU < cpuCount:
		return nil, badRequest("maxCpuCount (%d) can't be lower than cpuCount (%d)", maxCPU, cpuCount)
	case maxCPU > constants.MaxCPUHotplugVCPUs:
		return nil, badRequest("maxCpuCount can't be higher than %d", constants.MaxCPUHotplugVCPUs)
	case maxCPU > limits.MaxVcpu:
		return nil, badRequest("maxCpuCount can't be higher than %d (if you need to increase this limit, please contact support)", limits.MaxVcpu)
	}

	return &maxCPU, nil
}
