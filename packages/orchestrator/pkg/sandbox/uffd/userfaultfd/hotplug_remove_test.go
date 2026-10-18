//go:build linux

package userfaultfd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// stateHandler is a handler with a mapping and tracker but no userfaultfd:
// enough to drive the REMOVE bookkeeping without a guest.
func stateHandler(regions []memory.Region) *Userfaultfd {
	return &Userfaultfd{
		pageSize:    header.PageSize,
		pageTracker: block.NewTracker(),
		ma:          memory.NewMapping(regions),
		logger:      logger.NewNopLogger(),
	}
}

// The unplug path after a UFFD restore: one REMOVE per 128 MiB slot, inside the
// hotplug region, which sits after boot memory in the memory file but at an
// unrelated host address. The removed pages become Removed (a later fault
// serves zeros, never their old content, and the diff exports them empty),
// the hook sees the memory-file ranges, and boot memory is untouched.
func TestApplyRemoves_HotplugRegion(t *testing.T) {
	t.Parallel()

	const page = header.PageSize
	base, total := uintptr(16*page), uintptr(32*page)
	bootAddr, hotplugAddr := uintptr(0x10_0000), uintptr(0x90_0000)
	u := stateHandler([]memory.Region{
		{BaseHostVirtAddr: bootAddr, Size: base, Offset: 0, PageSize: page},
		{BaseHostVirtAddr: hotplugAddr, Size: total, Offset: base, PageSize: page},
	})

	// Pages the guest had touched: some boot memory and some hotplug memory.
	u.pageTracker.SetRange(0, 4, block.Dirty)
	u.pageTracker.SetRange(16, 24, block.Dirty)

	var seen []block.Range
	u.SetRemovedRangeHook(func(_ context.Context, r block.Range) { seen = append(seen, r) })

	// Unplug hotplug pages 2..6 (memory-file pages 18..22).
	rm := newUffdRemove(hotplugAddr+2*page, hotplugAddr+6*page)
	u.applyRemovesLocked(t.Context(), []*UffdRemove{&rm})

	require.Equal(t, []block.Range{{Start: int64(base + 2*page), Size: 4 * page}}, seen)

	dirty, empty := u.pageTracker.Export()
	assert.Equal(t, []uint32{0, 1, 2, 3, 16, 17, 22, 23}, dirty.ToArray(), "unplugged pages are no longer dirty")
	assert.Equal(t, []uint32{18, 19, 20, 21}, empty.ToArray(), "they export as empty")

	for idx := uint32(18); idx < 22; idx++ {
		assert.Equal(t, block.Removed, u.pageTracker.Get(idx), "a later fault at %d is served as zeros", idx)
	}
}

// A REMOVE spanning two adjacent regions is recorded in both, at each region's
// own memory-file offset — not extrapolated from its start.
func TestApplyRemoves_SpansRegions(t *testing.T) {
	t.Parallel()

	const page = header.PageSize
	u := stateHandler([]memory.Region{
		{BaseHostVirtAddr: 0x1000_0000, Size: 4 * page, Offset: 0, PageSize: page},
		{BaseHostVirtAddr: 0x1000_0000 + 4*page, Size: 4 * page, Offset: 16 * page, PageSize: page},
	})

	var seen []block.Range
	u.SetRemovedRangeHook(func(_ context.Context, r block.Range) { seen = append(seen, r) })

	rm := newUffdRemove(0x1000_0000+3*page, 0x1000_0000+5*page)
	u.applyRemovesLocked(t.Context(), []*UffdRemove{&rm})

	assert.Equal(t, []block.Range{{Start: 3 * page, Size: page}, {Start: 16 * page, Size: page}}, seen)
	assert.Equal(t, block.Removed, u.pageTracker.Get(3))
	assert.Equal(t, block.Removed, u.pageTracker.Get(16))
	assert.Equal(t, block.NotPresent, u.pageTracker.Get(4), "the gap between the regions' offsets is untouched")
}
