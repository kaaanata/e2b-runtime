package metadata

import (
	"errors"
	"fmt"
	"slices"
)

// CapabilityCPUHotplugV1 is the required capability of a template whose VMs
// boot with more vCPUs than they bring online (see Template.CPUHotplug). A
// reader without this code refuses such a template at decode instead of
// restoring a snapshot whose guest could bring every present vCPU online with
// no host-side limit (it would neither apply the cgroup CPU quota nor boot a
// cold start with maxcpus=), and accounting it at the boot count.
const CapabilityCPUHotplugV1 = "cpu-hotplug-v1"

// MaxFirecrackerVCPUs is Firecracker's vcpu_count ceiling.
const MaxFirecrackerVCPUs int64 = 32

// CPUHotplugLayout describes how a template's VMs are sized for CPU hot
// resize. Firecracker has no vCPU hotplug, so the VM is created with MaxVCPUs
// vCPUs and the guest kernel boots with maxcpus=BaseVCPUs: the rest are
// present but offline (their vCPU threads sit in KVM waiting for a SIPI and
// use no host CPU). A resize brings guest CPUs online or offline through envd
// and moves the host cgroup quota of the Firecracker process, which is what
// enforces the count: a guest that onlines more CPUs itself still gets no more
// than the quota.
//
// It is lineage state: the vCPU count is part of the snapshot (every vCPU's
// state is in it), so every later snapshot of the lineage restores MaxVCPUs.
type CPUHotplugLayout struct {
	// BaseVCPUs is the template's cpuCount: the CPUs online at boot and in the
	// template snapshot, and the smallest count a sandbox can run with.
	BaseVCPUs int64 `json:"base_vcpus"`
	// MaxVCPUs is the vCPU count of the VM (Firecracker vcpu_count).
	MaxVCPUs int64 `json:"max_vcpus"`
}

// ErrCPUHotplugLayout is matched by every layout validation error.
var ErrCPUHotplugLayout = errors.New("invalid CPU hotplug layout")

// ErrCPUCountOutOfRange is matched by CheckRequestedVCPUs's errors.
var ErrCPUCountOutOfRange = errors.New("CPU count out of range")

// NewCPUHotplugLayout returns the layout of a template with baseVCPUs online
// at boot that may grow to maxVCPUs. It returns nil (no hotplug, the classic
// fixed-size VM) when maxVCPUs is not above baseVCPUs. smt is the machine
// config's SMT setting: Firecracker then requires an even vcpu_count (or 1).
func NewCPUHotplugLayout(baseVCPUs, maxVCPUs int64, smt bool) (*CPUHotplugLayout, error) {
	if maxVCPUs < baseVCPUs {
		return nil, fmt.Errorf("%w: maximum CPU count %d is below the CPU count %d", ErrCPUHotplugLayout, maxVCPUs, baseVCPUs)
	}

	if maxVCPUs == baseVCPUs {
		return nil, nil
	}

	l := &CPUHotplugLayout{BaseVCPUs: baseVCPUs, MaxVCPUs: maxVCPUs}
	if err := l.Validate(); err != nil {
		return nil, err
	}

	if smt && maxVCPUs%2 != 0 {
		return nil, fmt.Errorf("%w: maximum CPU count %d must be even (the VM has SMT, Firecracker accepts only 1 or an even vcpu_count)", ErrCPUHotplugLayout, maxVCPUs)
	}

	return l, nil
}

// Validate checks the layout's bounds.
func (l *CPUHotplugLayout) Validate() error {
	switch {
	case l.BaseVCPUs <= 0:
		return fmt.Errorf("%w: CPU count %d", ErrCPUHotplugLayout, l.BaseVCPUs)
	case l.MaxVCPUs <= l.BaseVCPUs:
		return fmt.Errorf("%w: maximum CPU count %d must be above the CPU count %d", ErrCPUHotplugLayout, l.MaxVCPUs, l.BaseVCPUs)
	case l.MaxVCPUs > MaxFirecrackerVCPUs:
		return fmt.Errorf("%w: maximum CPU count %d is above Firecracker's %d", ErrCPUHotplugLayout, l.MaxVCPUs, MaxFirecrackerVCPUs)
	}

	return nil
}

// CheckRequestedVCPUs reports whether a sandbox may run with vcpus CPUs.
func (l *CPUHotplugLayout) CheckRequestedVCPUs(vcpus int64) error {
	if vcpus < l.BaseVCPUs || vcpus > l.MaxVCPUs {
		return fmt.Errorf("%w: %d is outside [%d, %d]", ErrCPUCountOutOfRange, vcpus, l.BaseVCPUs, l.MaxVCPUs)
	}

	return nil
}

// Equal reports whether two layouts (either may be nil) are the same.
func (l *CPUHotplugLayout) Equal(o *CPUHotplugLayout) bool {
	if l == nil || o == nil {
		return l == nil && o == nil
	}

	return *l == *o
}

// Clone returns a copy of the layout (nil for nil).
func (l *CPUHotplugLayout) Clone() *CPUHotplugLayout {
	if l == nil {
		return nil
	}

	c := *l

	return &c
}

// WithCPUHotplug stamps the CPU hotplug layout of the build that produced
// this metadata, together with the capability a reader must have to handle
// it. A nil layout clears both. Other required capabilities are kept.
func (t Template) WithCPUHotplug(l *CPUHotplugLayout) Template {
	t.CPUHotplug = l.Clone()
	t.RequiredCapabilities = slices.DeleteFunc(slices.Clone(t.RequiredCapabilities), func(c string) bool {
		return c == CapabilityCPUHotplugV1
	})

	if l != nil {
		t.RequiredCapabilities = append(t.RequiredCapabilities, CapabilityCPUHotplugV1)
	}

	if len(t.RequiredCapabilities) == 0 {
		t.RequiredCapabilities = nil
	}

	return t
}

// MarkCPUHotplugVCPUs records the CPU count the sandbox was granted when this
// snapshot was taken (0 for a template without CPU hotplug or a
// filesystem-only snapshot). A resume without an explicit count restores it.
// Per snapshot, like MemoryHotplugPluggedMiB: only WithPrefetch carries it.
func (t Template) MarkCPUHotplugVCPUs(vcpus int64) Template {
	t.CPUHotplugVCPUs = vcpus

	return t
}
