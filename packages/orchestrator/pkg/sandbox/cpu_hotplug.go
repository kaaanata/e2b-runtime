//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/cgroup"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// ErrCPUNotHotResizable reports a vCPU count request for a sandbox whose
// template was built without maxCpuCount: its vCPU count is fixed.
var ErrCPUNotHotResizable = errors.New("sandbox vCPU count is not hot-resizable: its template was built without maxCpuCount")

// errCPUHotplugEnvd reports an envd without the /cpus endpoint (older than
// the release that added it), so the guest's online CPUs cannot be changed;
// the cgroup quota still applies.
var errCPUHotplugEnvd = errors.New("the sandbox's envd does not support CPU hot resize (no /cpus endpoint); rebuild the template")

// cpuHotplugEnvdTimeout bounds one /cpus call. Onlining measured ~60 ms per
// CPU on the fork FC (4 -> 31 CPUs in 1.3-1.9 s), offlining ~30 ms.
const cpuHotplugEnvdTimeout = 30 * time.Second

// CPUResizeResult is the outcome of a vCPU resize.
type CPUResizeResult struct {
	// VCPUs is the count granted: the cgroup quota, applied before return.
	VCPUs int64
	// OnlineVCPUs is what the guest had online when the call returned.
	OnlineVCPUs int64
	// MaxVCPUs is the template's maximum.
	MaxVCPUs int64
	// Complete reports whether OnlineVCPUs == VCPUs.
	Complete bool
}

// WithRequestedVCPUs records the vCPU count a start asks for; 0 keeps the
// default (the template's count, or for a resume the count granted at pause,
// see adoptCPUHotplug). Call before the sandbox starts.
func (c *Config) WithRequestedVCPUs(vcpus int64) *Config {
	c.vcpuCount = vcpus

	return c
}

// RequestedVCPUs is the vCPU count granted to the sandbox (its cgroup
// quota): Vcpu for a fixed-size template or before any request.
func (c *Config) RequestedVCPUs() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.vcpuCount == 0 {
		return c.Vcpu
	}

	return c.vcpuCount
}

// OnlineVCPUs is the number of CPUs the guest had online when envd last
// reported it; the requested count before the first report.
func (c *Config) OnlineVCPUs() int64 {
	c.mu.RLock()
	online := c.onlineVCPUs
	c.mu.RUnlock()

	if online == 0 {
		return c.RequestedVCPUs()
	}

	return online
}

// MaxVCPUs is the largest vCPU count the sandbox can be resized to (Vcpu
// without CPU hotplug).
func (c *Config) MaxVCPUs() int64 {
	if c.CPUHotplug == nil {
		return c.Vcpu
	}

	return c.CPUHotplug.MaxVCPUs
}

// AccountedVCPUs is the vCPU count the node accounts for the sandbox: the
// granted count. The cgroup quota caps the VM there, whatever the guest has
// online, so the count present-but-offline vCPUs could reach is not counted.
func (c *Config) AccountedVCPUs() int64 {
	return c.RequestedVCPUs()
}

func (c *Config) setVCPUs(requested, online int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.vcpuCount = requested
	if online > 0 {
		c.onlineVCPUs = online
	}
}

// adoptCPUHotplug reconciles the config with the snapshot it starts from, as
// adoptMemoryHotplug does for memory: the snapshot's layout is authoritative,
// Vcpu must be its base, a request of 0 takes the count granted at pause (or
// the base), and any other request must be within the layout.
func (c *Config) adoptCPUHotplug(meta metadata.Template) error {
	requested := c.RequestedVCPUs()

	if meta.CPUHotplug == nil {
		if c.CPUHotplug != nil {
			return fmt.Errorf("%w: the snapshot has no CPU hotplug layout", ErrCPUNotHotResizable)
		}

		if requested != c.Vcpu {
			return fmt.Errorf("%w: requested %d vCPUs, the template is fixed at %d", ErrCPUNotHotResizable, requested, c.Vcpu)
		}

		return nil
	}

	l := meta.CPUHotplug
	if c.CPUHotplug != nil && !c.CPUHotplug.Equal(l) {
		return fmt.Errorf("CPU hotplug layout %+v does not match the snapshot's %+v", *c.CPUHotplug, *l)
	}

	if c.Vcpu != l.BaseVCPUs {
		return fmt.Errorf("vCPU count %d does not match the snapshot's %d", c.Vcpu, l.BaseVCPUs)
	}

	c.mu.Lock()
	if c.vcpuCount == 0 {
		c.vcpuCount = l.BaseVCPUs
		if meta.CPUHotplugVCPUs > 0 {
			c.vcpuCount = meta.CPUHotplugVCPUs
		}
	}
	c.mu.Unlock()

	if err := l.CheckRequestedVCPUs(c.RequestedVCPUs()); err != nil {
		return err
	}

	c.CPUHotplug = l.Clone()

	return nil
}

// applyCPUQuota sets the cgroup CPU quota of a CPU-hotplug sandbox to its
// granted count, before Firecracker starts (the cgroup is entered at clone),
// so the guest never runs above it. A no-op without CPU hotplug. Without a
// real cgroup the start is refused: nothing else would hold the guest to the
// granted count.
func (c *Config) applyCPUQuota(h *cgroup.CgroupHandle) error {
	if c.CPUHotplug == nil {
		return nil
	}

	if err := h.SetCPUQuota(c.RequestedVCPUs()); err != nil {
		return fmt.Errorf("CPU hotplug: cannot enforce %d vCPUs: %w", c.RequestedVCPUs(), err)
	}

	return nil
}

// callEnvdCPUs calls envd's /cpus: POST with online set, GET without.
func (s *Sandbox) callEnvdCPUs(ctx context.Context, online *int64) (envd.CPUsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cpuHotplugEnvdTimeout)
	defer cancel()

	method := http.MethodGet
	var body any
	if online != nil {
		method = http.MethodPost
		body = envd.CPUsRequest{Online: *online}
	}

	resp, err := s.doEnvdRequest(ctx, method, "cpus", "", body)
	if err != nil {
		return envd.CPUsResult{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return envd.CPUsResult{}, errCPUHotplugEnvd
	default:
		data, _ := io.ReadAll(resp.Body)

		return envd.CPUsResult{}, fmt.Errorf("cpus returned %d: %s", resp.StatusCode, utils.Truncate(string(data), 200))
	}

	var res envd.CPUsResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return envd.CPUsResult{}, fmt.Errorf("decode cpus result: %w", err)
	}

	return res, nil
}

// VerifyCPUHotplug checks, in a template build, that the guest has every vCPU
// of the VM present and hotpluggable (the guest kernel has CONFIG_HOTPLUG_CPU
// and was booted with maxcpus=, not nr_cpus=), then brings it to exactly the
// template's CPUs online and checks that took: the layer snapshot must hold
// the base online. Something in the guest may have onlined more at boot (a
// udev rule onlining CPUs on "add", as Ubuntu's 40-vm-hotadd.rules does
// during coldplug). A template whose sandboxes cannot be resized must fail
// its build. A no-op without CPU hotplug.
func (s *Sandbox) VerifyCPUHotplug(ctx context.Context) error {
	l := s.Config.CPUHotplug
	if l == nil {
		return nil
	}

	res, err := s.callEnvdCPUs(ctx, nil)
	if err != nil {
		return fmt.Errorf("CPU hotplug: %w", err)
	}

	if res.Present != l.MaxVCPUs || !res.Hotpluggable {
		return fmt.Errorf("CPU hotplug: the guest has %d of %d CPUs present, hotpluggable=%v: the guest kernel must have CONFIG_HOTPLUG_CPU to build a template with maxCpuCount",
			res.Present, l.MaxVCPUs, res.Hotpluggable)
	}

	online, complete, err := s.setGuestCPUs(ctx, l.BaseVCPUs)
	if err != nil {
		return fmt.Errorf("CPU hotplug: %w", err)
	}

	if !complete {
		return fmt.Errorf("CPU hotplug: the guest has %d CPUs online and could not be brought to the template's %d", online, l.BaseVCPUs)
	}

	if res.Online != l.BaseVCPUs {
		s.log().Info(ctx, "CPU hotplug: the guest had more CPUs online than it booted with (a udev rule?); brought back to the base",
			zap.Int64("online_before", res.Online), zap.Int64("base", l.BaseVCPUs))
	}

	return nil
}

// setGuestCPUs asks envd to bring exactly n CPUs online and records the
// result. The cgroup quota has already been moved by the caller.
func (s *Sandbox) setGuestCPUs(ctx context.Context, n int64) (online int64, complete bool, err error) {
	res, err := s.callEnvdCPUs(ctx, &n)
	if err != nil {
		return 0, false, err
	}

	if len(res.Errors) > 0 {
		s.log().Warn(ctx, "CPU hotplug: some guest CPUs could not be changed",
			zap.Int64("requested", n), zap.Int64("online", res.Online), zap.Strings("errors", res.Errors))
	}

	s.Config.setVCPUs(n, res.Online)

	return res.Online, res.Online == n, nil
}

// settleRequestedCPUs brings a just-started CPU-hotplug sandbox's guest to
// its granted count once envd is up. The quota was applied before the VM ran,
// so a failure here only leaves the guest's view (nproc, the scheduler) off:
// it is reported, not fatal.
func (s *Sandbox) settleRequestedCPUs(ctx context.Context) {
	if s.Config.CPUHotplug == nil {
		return
	}

	want := s.Config.RequestedVCPUs()

	ctx, span := tracer.Start(ctx, "settle-requested-cpus", trace.WithAttributes(
		attribute.Int64("cpu_hotplug.requested_vcpus", want),
	))
	defer span.End()

	online, complete, err := s.setGuestCPUs(ctx, want)
	if err != nil {
		s.log().Warn(ctx, "CPU hotplug: could not set the guest's online CPUs at start; the cgroup quota still applies",
			zap.Int64("requested", want), zap.Error(err))
		telemetry.ReportError(ctx, "CPU hotplug settle failed", err)

		return
	}

	span.SetAttributes(attribute.Int64("cpu_hotplug.online_vcpus", online))
	if !complete {
		s.log().Warn(ctx, "CPU hotplug: guest did not reach the requested CPU count at start",
			zap.Int64("requested", want), zap.Int64("online", online))
	}
}

// ResizeCPU changes the vCPU count of the running sandbox to vcpus: it moves
// the cgroup quota first (the enforcement, effective immediately in both
// directions) and then asks the guest to bring CPUs online or offline. It
// excludes pauses for its duration. The result reports what the guest has
// online; a guest that could not reach the count still runs, capped at the
// quota.
func (s *Sandbox) ResizeCPU(ctx context.Context, vcpus int64) (CPUResizeResult, error) {
	l := s.Config.CPUHotplug
	if l == nil {
		return CPUResizeResult{}, ErrCPUNotHotResizable
	}

	if err := l.CheckRequestedVCPUs(vcpus); err != nil {
		return CPUResizeResult{}, err
	}

	ctx, span := tracer.Start(ctx, "resize-cpu", trace.WithAttributes(
		attribute.Int64("cpu_hotplug.requested_vcpus", vcpus),
	))
	defer span.End()

	s.cpuResizeMu.Lock()
	defer s.cpuResizeMu.Unlock()

	if err := s.cgroupHandle.SetCPUQuota(vcpus); err != nil {
		return CPUResizeResult{}, fmt.Errorf("set CPU quota: %w", err)
	}

	// Recorded once the quota holds: accounting follows the enforcement.
	s.Config.setVCPUs(vcpus, 0)

	online, complete, err := s.setGuestCPUs(ctx, vcpus)
	if err != nil {
		// The quota (the grant) already holds and is recorded; report the
		// guest's last known count as incomplete rather than fail a resize
		// whose enforcement took effect.
		s.log().Warn(ctx, "CPU hotplug: the CPU quota moved but the guest's CPUs could not be changed",
			zap.Int64("requested", vcpus), zap.Error(err))
		telemetry.ReportError(ctx, "CPU hotplug resize: guest CPUs unchanged", err)
		online, complete = s.Config.OnlineVCPUs(), false
	}

	span.SetAttributes(
		attribute.Int64("cpu_hotplug.online_vcpus", online),
		attribute.Bool("cpu_hotplug.complete", complete),
	)

	return CPUResizeResult{
		VCPUs:       vcpus,
		OnlineVCPUs: online,
		MaxVCPUs:    l.MaxVCPUs,
		Complete:    complete,
	}, nil
}

// stampCPUHotplugVCPUs records in the snapshot's metadata the vCPU count the
// sandbox was granted. 0 without CPU hotplug.
func (s *Sandbox) stampCPUHotplugVCPUs(m metadata.Template) metadata.Template {
	if s.Config.CPUHotplug == nil {
		return m.MarkCPUHotplugVCPUs(0)
	}

	return m.MarkCPUHotplugVCPUs(s.Config.RequestedVCPUs())
}
