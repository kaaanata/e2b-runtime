//go:build linux

package uffd

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// ExpectMemoryHotplug declares that the snapshot this backend serves has a
// virtio-mem hotplug region at memory-file range [offset, offset+size). The
// handshake is then checked against it, and REMOVEs inside the region (the
// guest unplugging memory) punch holes in the memfd so the host gets the
// memory back. Must be called before Start.
func (u *Uffd) ExpectMemoryHotplug(offset, size int64) {
	u.hotplug = &block.Range{Start: offset, Size: size}
}

// validateHandshake checks the regions and memfd Firecracker sent against
// the expected hotplug region. Firecracker registers every region of the
// snapshot whole — boot memory at offset 0 and the hotplug region right after
// it, plugged or not — and sizes the memfd to cover them all (measured on the
// fork FC v1.14-0.2.0: 2 regions, memfd = base + total). Any other shape
// means the faults, removes and exports below would be translated wrongly.
func validateHandshake(regions []memory.Region, memfdSize int64, hotplug *block.Range) error {
	if hotplug == nil {
		return nil
	}

	found := false
	end := int64(0)
	for _, r := range regions {
		end = max(end, int64(r.Offset+r.Size))
		if int64(r.Offset) == hotplug.Start && int64(r.Size) == hotplug.Size {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("memory hotplug region [%d, %d) not in the UFFD handshake regions %+v", hotplug.Start, hotplug.End(), regions)
	}

	if end != hotplug.End() {
		return fmt.Errorf("UFFD handshake regions end at %d, the memory hotplug layout at %d", end, hotplug.End())
	}

	if memfdSize >= 0 && memfdSize != hotplug.End() {
		return fmt.Errorf("memfd is %d bytes, the memory hotplug layout needs %d", memfdSize, hotplug.End())
	}

	return nil
}

// clipRange is the part of r inside bounds.
func clipRange(r, bounds block.Range) (block.Range, bool) {
	start := max(r.Start, bounds.Start)
	end := min(r.End(), bounds.End())
	if start >= end {
		return block.Range{}, false
	}

	return block.Range{Start: start, Size: end - start}, true
}

// hotplugReportInterval is how often a sandbox with hotplug activity logs a
// summary of the memory it released (only when something changed).
const hotplugReportInterval = 10 * time.Second

// HotplugReleaseStats are the cumulative counters of a hotplugRelease.
type HotplugReleaseStats struct {
	// Removes is the number of removed memory-file ranges reported to the
	// hook (one per region a UFFD_EVENT_REMOVE touched), in any region.
	Removes uint64
	// HotplugBytes is the part of those ranges inside the hotplug region —
	// memory the guest unplugged.
	HotplugBytes int64
	// BootBytes is the part outside it: boot memory dropped by balloon
	// free-page reporting, which is deliberately not released here.
	BootBytes int64
	// PunchedBytes is what was actually punched out of the memfd.
	PunchedBytes int64
	// SkippedBytes were unplugged while the memfd was handed off for a
	// snapshot export (the VM is paused then; nothing to release).
	SkippedBytes int64
	// PunchFailures counts failed fallocate(PUNCH_HOLE) calls.
	PunchFailures uint64
}

// hotplugRelease punches the hotplug part of every removed range out of the
// memfd. Without it an unplug on the UFFD + memfd path frees nothing on the
// host (shared memfd pages survive MADV_DONTNEED; measured flat hugepage use
// and RSS after unplug) until the next pause. Boot memory is left alone: its
// REMOVEs come from balloon free-page reporting, whose host-side release is a
// separate decision. A failed punch only costs memory; the tracker has
// already recorded the range Removed, so a later fault still serves zeros.
//
// Only memory the guest had touched is backed, so an unplug releases exactly
// the touched pages of the unplugged blocks. The guest's struct pages
// (memmap) for hotplugged memory live in boot memory and are not part of the
// hotplug region: growing by N GiB costs ~N×16 MiB of boot memory once, and a
// shrink does not give that back (the pages are only freed inside the guest).
type hotplugRelease struct {
	memfd  func() *block.Memfd
	region block.Range
	logger logger.Logger

	removes       atomic.Uint64
	hotplugBytes  atomic.Int64
	bootBytes     atomic.Int64
	punchedBytes  atomic.Int64
	skippedBytes  atomic.Int64
	punchFailures atomic.Uint64

	// reported is the snapshot of the last summary log, so the reporter
	// only logs when something changed. Owned by the reporter goroutine.
	reported HotplugReleaseStats
}

func newHotplugRelease(memfd func() *block.Memfd, region block.Range, lg logger.Logger) *hotplugRelease {
	return &hotplugRelease{memfd: memfd, region: region, logger: lg}
}

// onRemoved is the userfaultfd.RemovedRangeHook.
func (h *hotplugRelease) onRemoved(ctx context.Context, r block.Range) {
	h.removes.Add(1)

	clipped, ok := clipRange(r, h.region)
	h.bootBytes.Add(r.Size - clipped.Size)
	if !ok {
		return
	}
	h.hotplugBytes.Add(clipped.Size)

	m := h.memfd()
	if m == nil {
		// Handed off for a snapshot export; the VM is paused then and the
		// export reads the removed range as empty anyway.
		h.skippedBytes.Add(clipped.Size)

		return
	}

	if err := m.PunchHole(clipped.Start, clipped.Size); err != nil {
		// Warn once; the summary carries the count of later failures.
		if h.punchFailures.Add(1) == 1 {
			h.logger.Warn(ctx, "memory hotplug: could not release unplugged memory",
				zap.Int64("offset", clipped.Start), zap.Int64("size", clipped.Size), zap.Error(err))
		}

		return
	}
	h.punchedBytes.Add(clipped.Size)

	h.logger.Debug(ctx, "memory hotplug: released unplugged memory",
		zap.Int64("offset", clipped.Start), zap.Int64("size", clipped.Size))
}

// Stats returns the cumulative counters.
func (h *hotplugRelease) Stats() HotplugReleaseStats {
	return HotplugReleaseStats{
		Removes:       h.removes.Load(),
		HotplugBytes:  h.hotplugBytes.Load(),
		BootBytes:     h.bootBytes.Load(),
		PunchedBytes:  h.punchedBytes.Load(),
		SkippedBytes:  h.skippedBytes.Load(),
		PunchFailures: h.punchFailures.Load(),
	}
}

// report logs a summary if the counters moved since the last one. Not safe
// for concurrent use (one reporter per release).
func (h *hotplugRelease) report(ctx context.Context, msg string) {
	s := h.Stats()
	if s == h.reported {
		return
	}

	h.logger.Info(ctx, msg,
		zap.Uint64("removes", s.Removes),
		zap.Int64("unplugged_bytes", s.HotplugBytes),
		zap.Int64("punched_bytes", s.PunchedBytes),
		zap.Int64("skipped_bytes", s.SkippedBytes),
		zap.Int64("boot_removed_bytes", s.BootBytes),
		zap.Uint64("punch_failures", s.PunchFailures),
		zap.Int64("hotplug_offset", h.region.Start),
		zap.Int64("hotplug_size", h.region.Size))
	h.reported = s
}

// reportLoop logs a summary every interval while there is activity, and a
// final one when done closes.
func (h *hotplugRelease) reportLoop(ctx context.Context, interval time.Duration, done <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			h.report(ctx, "memory hotplug: released unplugged memory (cumulative)")
		case <-done:
			h.report(ctx, "memory hotplug: released unplugged memory (final)")

			return
		}
	}
}

// HotplugReleaseStats reports what the backend released for unplugged
// memory. ok is false when the backend releases nothing (no hotplug region,
// no memfd in the handshake, or Firecracker has not connected yet).
func (u *Uffd) HotplugReleaseStats() (HotplugReleaseStats, bool) {
	r := u.hotplugRelease.Load()
	if r == nil {
		return HotplugReleaseStats{}, false
	}

	return r.Stats(), true
}
