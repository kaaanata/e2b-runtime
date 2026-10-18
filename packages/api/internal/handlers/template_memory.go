package handlers

import "github.com/e2b-dev/infra/packages/api/internal/api"

// maxMemoryToAPI renders env_builds.max_ram_mb: the memory hotplug maximum of
// a build, nil for a fixed-size build (or one that did not record it).
func maxMemoryToAPI(maxRamMB *int64) *api.MaxMemoryMB {
	if maxRamMB == nil {
		return nil
	}

	return new(api.MaxMemoryMB(*maxRamMB))
}
