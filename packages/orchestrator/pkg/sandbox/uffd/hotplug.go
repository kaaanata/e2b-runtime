//go:build linux

package uffd

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/userfaultfd"
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

// hotplugRemovedHook punches the hotplug part of every removed range out of
// the memfd. Without it an unplug on the UFFD + memfd path frees nothing on
// the host (shared memfd pages survive MADV_DONTNEED; measured flat hugepage
// use and RSS after unplug) until the next pause. Boot memory is left alone:
// its REMOVEs come from balloon free-page reporting, whose host-side release
// is a separate decision. A failed punch only costs memory; the tracker has
// already recorded the range Removed, so a later fault still serves zeros.
func hotplugRemovedHook(memfd func() *block.Memfd, hotplug block.Range, lg logger.Logger) userfaultfd.RemovedRangeHook {
	return func(ctx context.Context, r block.Range) {
		clipped, ok := clipRange(r, hotplug)
		if !ok {
			return
		}

		m := memfd()
		if m == nil {
			// Handed off for a snapshot export; the VM is paused then and the
			// export reads the removed range as empty anyway.
			return
		}

		if err := m.PunchHole(clipped.Start, clipped.Size); err != nil {
			lg.Warn(ctx, "memory hotplug: could not release unplugged memory", zap.Error(err))
		}
	}
}
