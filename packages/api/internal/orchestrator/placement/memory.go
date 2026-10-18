package placement

import "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"

// requestedMemoryMiB is the memory a sandbox occupies on its node for
// placement and the optimistic allocation: the requested size (memory
// hotplug) when the request names one, else the boot memory. It mirrors the
// node's own accounting, which counts max(requested, plugged).
func requestedMemoryMiB(cfg *orchestrator.SandboxConfig) int64 {
	if cfg.GetRequestedMemoryMb() > 0 {
		return cfg.GetRequestedMemoryMb()
	}

	return cfg.GetRamMb()
}

// requestedVCPUs is the vCPU count a sandbox occupies on its node for
// placement and the optimistic allocation: the requested count (CPU hot
// resize) when the request names one, else the template's count. It mirrors
// the node's accounting, which counts the granted count (the cgroup quota).
func requestedVCPUs(cfg *orchestrator.SandboxConfig) int64 {
	if cfg.GetRequestedVcpu() > 0 {
		return cfg.GetRequestedVcpu()
	}

	return cfg.GetVcpu()
}
