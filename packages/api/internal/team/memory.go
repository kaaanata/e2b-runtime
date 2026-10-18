package team

import (
	"fmt"
	"net/http"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/constants"
	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

func badRequest(format string, args ...any) *api.APIError {
	msg := fmt.Sprintf(format, args...)

	return &api.APIError{Err: fmt.Errorf("%s", msg), ClientMsg: msg, Code: http.StatusBadRequest}
}

// LimitMaxMemory validates a template build's memory hotplug maximum against
// its boot memory ramMB and the team tier. It returns nil for a fixed-size
// build (no maximum, or one equal to ramMB), which is what is stored.
func LimitMaxMemory(limits *types.TeamLimits, ramMB int64, maxMemoryMB *int32) (*int64, *api.APIError) {
	if maxMemoryMB == nil {
		return nil, nil
	}

	maxMB := int64(*maxMemoryMB)

	switch {
	case maxMB == ramMB:
		return nil, nil
	case maxMB < ramMB:
		return nil, badRequest("maxMemoryMB (%d MiB) can't be lower than memoryMB (%d MiB)", maxMB, ramMB)
	case maxMB > limits.MaxRamMb:
		return nil, badRequest("maxMemoryMB can't be higher than %d MiB (if you need to increase this limit, please contact support)", limits.MaxRamMb)
	case (maxMB-ramMB)%constants.MemoryHotplugSlotMB != 0:
		return nil, badRequest("maxMemoryMB - memoryMB (%d MiB) must be a multiple of %d MiB", maxMB-ramMB, constants.MemoryHotplugSlotMB)
	}

	return &maxMB, nil
}
