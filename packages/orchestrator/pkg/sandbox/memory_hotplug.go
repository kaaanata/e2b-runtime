//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

// ErrMemoryNotHotResizable reports a memory size request for a sandbox whose
// template has no virtio-mem device: its memory is fixed at the boot size.
var ErrMemoryNotHotResizable = errors.New("sandbox memory is not hot-resizable: its template was built without maxMemoryMB")

// ErrMemoryResizeBusy reports a resize attempted while an in-place checkpoint
// of the sandbox is in flight; the caller retries after it.
var ErrMemoryResizeBusy = errors.New("sandbox memory cannot be resized while a checkpoint is in progress")

// DefaultMemoryHotplugTimeout bounds how long a resize waits for the guest to
// reach the requested size. Every grow measured on the fork FC completed in
// well under a second; a shrink under pinned memory stalls (and recovers tens
// of seconds after the memory is freed), so after the timeout the achieved
// size is reported and the sandbox keeps running.
const DefaultMemoryHotplugTimeout = 30 * time.Second

// memoryHotplugPollInterval is how often the plugged size is read while a
// resize waits for the guest.
const memoryHotplugPollInterval = 100 * time.Millisecond

// MemoryAccountingMode selects what a node counts as a sandbox's memory.
type MemoryAccountingMode string

const (
	// MemoryAccountingRequested counts boot memory plus the larger of the
	// requested and the plugged hotplug memory: a request already promised and
	// memory still plugged both occupy the host. The default: Firecracker
	// reserves nothing up front (MAP_NORESERVE), so admission against the
	// hugepage pool is the only thing that keeps a faulting guest from SIGBUS.
	MemoryAccountingRequested MemoryAccountingMode = "requested"
	// MemoryAccountingBoot counts only the boot memory, as before hotplug.
	MemoryAccountingBoot MemoryAccountingMode = "boot"
)

// MemoryAccountingEnv is the environment variable selecting the
// MemoryAccountingMode; unset or unknown values mean MemoryAccountingRequested.
const MemoryAccountingEnv = "MEMORY_HOTPLUG_ACCOUNTING"

var memoryAccounting = parseMemoryAccounting(env.GetEnv(MemoryAccountingEnv, string(MemoryAccountingRequested)))

func parseMemoryAccounting(v string) MemoryAccountingMode {
	if MemoryAccountingMode(v) == MemoryAccountingBoot {
		return MemoryAccountingBoot
	}

	return MemoryAccountingRequested
}

// MemoryResizeResult is the outcome of a resize, in MiB.
type MemoryResizeResult struct {
	// RequestedMB is the request as recorded (block-aligned).
	RequestedMB int64
	// MemoryMB is the guest memory when the call returned: boot plus plugged.
	MemoryMB int64
	// MaxMB is the template's maximum.
	MaxMB int64
	// Complete reports whether MemoryMB reached RequestedMB before the timeout.
	Complete bool
}

// memfileSizeBytes is the size of the memory file a sandbox of this config
// boots with: boot memory, plus the whole hotplug region when the template
// has a virtio-mem device (its memory file covers the region, holes read as
// zeros).
func (c *Config) memfileSizeBytes() int64 {
	if c.MemoryHotplug != nil {
		return c.MemoryHotplug.MemfileSizeBytes()
	}

	return units.MBToBytes(c.RamMB)
}

// MemfileSizeBytes is memfileSizeBytes for callers that create the empty
// memory file of a cold boot (template builds).
func (c *Config) MemfileSizeBytes() int64 {
	return c.memfileSizeBytes()
}

// RequestedMemoryMB is the guest memory requested for the sandbox: boot
// memory plus hotplug. RamMB without a device or before any request.
func (c *Config) RequestedMemoryMB() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.memoryMB == 0 {
		return c.RamMB
	}

	return c.memoryMB
}

// CurrentMemoryMB is the guest memory as last observed: boot memory plus the
// plugged hotplug memory.
func (c *Config) CurrentMemoryMB() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.RamMB + c.pluggedMB
}

// MaxMemoryMB is the largest size the sandbox can be resized to (RamMB
// without a device).
func (c *Config) MaxMemoryMB() int64 {
	if c.MemoryHotplug == nil {
		return c.RamMB
	}

	return c.MemoryHotplug.MaxMiB()
}

// AccountedMemoryMB is the guest memory the node accounts for this sandbox,
// per MemoryAccountingEnv.
func (c *Config) AccountedMemoryMB() int64 {
	if c.MemoryHotplug == nil || memoryAccounting == MemoryAccountingBoot {
		return c.RamMB
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	requested := c.memoryMB
	if requested == 0 {
		requested = c.RamMB
	}

	return c.MemoryHotplug.AccountedMiB(requested-c.RamMB, c.pluggedMB)
}

func (c *Config) setMemory(requestedMB, pluggedMB int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.memoryMB = requestedMB
	c.pluggedMB = pluggedMB
}

// adoptMemoryHotplug reconciles the config with the snapshot it starts from.
// The snapshot is authoritative for the device (it carries it in its device
// table and memory file); a config that names one must name the same one,
// and the boot memory must be the device's base. A request of 0 takes the
// size recorded in the snapshot, so an older caller never changes it; any
// other request must be within the device's range.
func (c *Config) adoptMemoryHotplug(meta metadata.Template) error {
	requested := c.RequestedMemoryMB()

	if meta.MemoryHotplug == nil {
		if c.MemoryHotplug != nil {
			return fmt.Errorf("%w: the snapshot has no memory hotplug device", ErrMemoryNotHotResizable)
		}

		if requested != c.RamMB {
			return fmt.Errorf("%w: requested %d MiB, the template is fixed at %d MiB", ErrMemoryNotHotResizable, requested, c.RamMB)
		}

		return nil
	}

	l := meta.MemoryHotplug
	if c.MemoryHotplug != nil && !c.MemoryHotplug.Equal(l) {
		return fmt.Errorf("memory hotplug layout %+v does not match the snapshot's %+v", *c.MemoryHotplug, *l)
	}

	if c.RamMB != l.BaseMiB {
		return fmt.Errorf("boot memory %d MiB does not match the snapshot's %d MiB", c.RamMB, l.BaseMiB)
	}

	c.mu.Lock()
	if c.memoryMB == 0 {
		c.memoryMB = l.BaseMiB + meta.MemoryHotplugPluggedMiB
	}
	c.pluggedMB = meta.MemoryHotplugPluggedMiB
	c.mu.Unlock()

	if _, err := l.RequestedHotplugMiB(c.RequestedMemoryMB()); err != nil {
		return err
	}

	c.MemoryHotplug = l.Clone()

	return nil
}

// resumeHotplugRequest returns the hotplug size a memory resume must request
// from the paused VM, or nil when the snapshot already restores the
// requested size.
func (c *Config) resumeHotplugRequest(meta metadata.Template) (*int64, error) {
	if c.MemoryHotplug == nil {
		return nil, nil
	}

	want, err := c.MemoryHotplug.RequestedHotplugMiB(c.RequestedMemoryMB())
	if err != nil {
		return nil, err
	}

	if want == meta.MemoryHotplugPluggedMiB {
		return nil, nil
	}

	return &want, nil
}

// VerifyMemoryHotplug checks that the guest activated the sandbox's virtio-mem
// device, by requesting the size the template build runs at (0 plugged).
// Firecracker refuses the request when the guest driver never acknowledged
// the device, which is what a guest kernel without CONFIG_VIRTIO_MEM does; a
// template build must fail then instead of producing a template whose
// sandboxes cannot be resized. A no-op for a sandbox without a device.
func (s *Sandbox) VerifyMemoryHotplug(ctx context.Context) error {
	if s.Config.MemoryHotplug == nil {
		return nil
	}

	if err := s.process.PatchMemoryHotplug(ctx, 0); err != nil {
		return fmt.Errorf("memory hotplug: the guest kernel must have CONFIG_VIRTIO_MEM to build a template with maxMemoryMB: %w", err)
	}

	return nil
}

// settleRequestedMemory brings a just-started sandbox to its requested size:
// requests it from the device (unless alreadyRequested, the resume path
// having done so on the paused VM) and waits for the guest, bounded by
// DefaultMemoryHotplugTimeout. An incomplete plug is reported, not fatal: the
// guest keeps plugging, and accounting covers the request.
func (s *Sandbox) settleRequestedMemory(ctx context.Context, alreadyRequested bool) error {
	l := s.Config.MemoryHotplug
	if l == nil {
		return nil
	}

	want, err := l.RequestedHotplugMiB(s.Config.RequestedMemoryMB())
	if err != nil {
		return err
	}

	ctx, span := tracer.Start(ctx, "settle-requested-memory", trace.WithAttributes(
		attribute.Int64("memory_hotplug.requested_mib", want),
		attribute.Bool("memory_hotplug.already_requested", alreadyRequested),
	))
	defer span.End()

	if !alreadyRequested {
		status, err := s.process.MemoryHotplugStatus(ctx)
		if err != nil {
			return err
		}

		s.Config.setMemory(s.Config.RequestedMemoryMB(), status.PluggedMiB)
		if status.PluggedMiB == want && status.RequestedMiB == want {
			return nil
		}

		if err := s.process.PatchMemoryHotplug(ctx, want); err != nil {
			return err
		}
	}

	plugged, complete, err := s.waitMemoryPlugged(ctx, want, DefaultMemoryHotplugTimeout)
	if err != nil {
		return err
	}

	s.Config.setMemory(s.Config.RequestedMemoryMB(), plugged)
	if !complete {
		s.log().Warn(ctx, "memory hotplug: guest did not reach the requested size at start",
			zap.Int64("requested_mib", want), zap.Int64("plugged_mib", plugged))
		telemetry.ReportEvent(ctx, "memory hotplug incomplete at start",
			attribute.Int64("memory_hotplug.plugged_mib", plugged))
	}

	return nil
}

// waitMemoryPlugged polls the device until plugged == want, the timeout
// elapses or ctx ends. It returns the last plugged size.
func (s *Sandbox) waitMemoryPlugged(ctx context.Context, want int64, timeout time.Duration) (plugged int64, complete bool, err error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(memoryHotplugPollInterval)
	defer ticker.Stop()

	for {
		status, err := s.process.MemoryHotplugStatus(ctx)
		if err != nil {
			return 0, false, err
		}
		plugged = status.PluggedMiB

		if plugged == want {
			return plugged, true, nil
		}

		if time.Now().After(deadline) {
			return plugged, false, nil
		}

		select {
		case <-ctx.Done():
			return plugged, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ResizeMemory changes the guest memory of the running sandbox to
// requestedMB. It validates the request against the template's device,
// refuses while an in-place checkpoint runs, waits for any pending CoW memory
// capture, excludes pauses for its duration, PATCHes the device and waits up
// to timeout (<= 0: DefaultMemoryHotplugTimeout) for the guest. The result
// reports the achieved size; an incomplete shrink is not an error and never
// stops the sandbox.
func (s *Sandbox) ResizeMemory(ctx context.Context, requestedMB int64, timeout time.Duration) (MemoryResizeResult, error) {
	l := s.Config.MemoryHotplug
	if l == nil {
		return MemoryResizeResult{}, ErrMemoryNotHotResizable
	}

	want, err := l.RequestedHotplugMiB(requestedMB)
	if err != nil {
		return MemoryResizeResult{}, err
	}

	if timeout <= 0 {
		timeout = DefaultMemoryHotplugTimeout
	}

	ctx, span := tracer.Start(ctx, "resize-memory", trace.WithAttributes(
		attribute.Int64("memory_hotplug.requested_mb", requestedMB),
		attribute.Int64("memory_hotplug.requested_hotplug_mib", want),
	))
	defer span.End()

	s.memResizeMu.Lock()
	defer s.memResizeMu.Unlock()

	if s.inPlaceCheckpointInFlight.Load() {
		return MemoryResizeResult{}, ErrMemoryResizeBusy
	}

	// A CoW export window from a previous in-place checkpoint reads pre-images
	// of armed pages; an unplug's REMOVE on one of them cancels the export.
	if err := s.waitForMemorySeal(ctx); err != nil {
		return MemoryResizeResult{}, fmt.Errorf("previous memory capture did not complete: %w", err)
	}

	if err := s.process.PatchMemoryHotplug(ctx, want); err != nil {
		return MemoryResizeResult{}, err
	}

	// Recorded before waiting: accounting covers the request from now on,
	// and a caller that gives up still sees the request on the node.
	s.Config.setMemory(l.BaseMiB+want, s.Config.CurrentMemoryMB()-l.BaseMiB)

	plugged, complete, err := s.waitMemoryPlugged(ctx, want, timeout)
	if err != nil {
		return MemoryResizeResult{}, err
	}

	s.Config.setMemory(l.BaseMiB+want, plugged)

	res := MemoryResizeResult{
		RequestedMB: l.BaseMiB + want,
		MemoryMB:    l.BaseMiB + plugged,
		MaxMB:       l.MaxMiB(),
		Complete:    complete,
	}
	span.SetAttributes(
		attribute.Int64("memory_hotplug.plugged_mib", plugged),
		attribute.Bool("memory_hotplug.complete", complete),
	)
	if !complete {
		telemetry.ReportEvent(ctx, "memory hotplug resize incomplete",
			attribute.Int64("memory_hotplug.plugged_mib", plugged))
	}

	return res, nil
}

// stampMemoryHotplugPlugged records in the snapshot's metadata how much
// hotplug memory is plugged, read from the paused VM. 0 for a sandbox without
// a device or a filesystem-only snapshot.
func (s *Sandbox) stampMemoryHotplugPlugged(ctx context.Context, m metadata.Template, filesystemOnly bool) (metadata.Template, error) {
	if s.Config.MemoryHotplug == nil || filesystemOnly {
		return m.MarkMemoryHotplugPlugged(0), nil
	}

	status, err := s.process.MemoryHotplugStatus(ctx)
	if err != nil {
		return m, fmt.Errorf("memory hotplug status at pause: %w", err)
	}

	s.Config.setMemory(s.Config.RequestedMemoryMB(), status.PluggedMiB)

	return m.MarkMemoryHotplugPlugged(status.PluggedMiB), nil
}
