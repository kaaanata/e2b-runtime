//go:build linux

package uffd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const (
	testBase  = int64(1 << 30)
	testTotal = int64(2 << 30)
)

func hotplugRegions() []memory.Region {
	return []memory.Region{
		{BaseHostVirtAddr: 0x7f00_0000_0000, Size: uintptr(testBase), Offset: 0, PageSize: header.HugepageSize},
		{BaseHostVirtAddr: 0x7e00_0000_0000, Size: uintptr(testTotal), Offset: uintptr(testBase), PageSize: header.HugepageSize},
	}
}

func TestValidateHandshake(t *testing.T) {
	t.Parallel()

	hotplug := &block.Range{Start: testBase, Size: testTotal}

	require.NoError(t, validateHandshake(hotplugRegions(), testBase+testTotal, hotplug), "the measured fork handshake")
	require.NoError(t, validateHandshake(hotplugRegions(), -1, hotplug), "no memfd sent: only the regions are checked")
	require.NoError(t, validateHandshake(hotplugRegions()[:1], testBase, nil), "no hotplug expected: nothing is checked")

	require.Error(t, validateHandshake(hotplugRegions()[:1], testBase, hotplug), "the hotplug region is missing")
	require.Error(t, validateHandshake(hotplugRegions(), testBase, hotplug), "memfd sized to boot memory only")

	shifted := hotplugRegions()
	shifted[1].Offset += header.HugepageSize
	require.Error(t, validateHandshake(shifted, testBase+testTotal, hotplug), "region at a different offset")

	extra := append(hotplugRegions(), memory.Region{BaseHostVirtAddr: 0x1000, Size: header.HugepageSize, Offset: uintptr(testBase + testTotal), PageSize: header.HugepageSize})
	require.Error(t, validateHandshake(extra, testBase+testTotal, hotplug), "regions beyond the layout")
}

func TestClipRange(t *testing.T) {
	t.Parallel()

	bounds := block.Range{Start: 100, Size: 100}

	got, ok := clipRange(block.Range{Start: 150, Size: 10}, bounds)
	assert.True(t, ok)
	assert.Equal(t, block.Range{Start: 150, Size: 10}, got)

	got, ok = clipRange(block.Range{Start: 50, Size: 100}, bounds)
	assert.True(t, ok)
	assert.Equal(t, block.Range{Start: 100, Size: 50}, got, "only the in-region part")

	_, ok = clipRange(block.Range{Start: 0, Size: 100}, bounds)
	assert.False(t, ok, "boot memory is left alone")
}

func newPatternMemfd(t *testing.T, size int64) *block.Memfd {
	t.Helper()

	fd, err := unix.MemfdCreate("hotplug-test", 0)
	require.NoError(t, err)
	require.NoError(t, unix.Ftruncate(fd, size))

	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = 0xa5
	}
	_, err = unix.Pwrite(fd, pattern, 0)
	require.NoError(t, err)

	m, err := block.NewFromFd(fd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	return m
}

// An unplug must release the hotplug part of the removed range, and only it.
func TestHotplugRemovedHook_PunchesOnlyTheHotplugRegion(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	base, total := 4*page, 8*page
	m := newPatternMemfd(t, base+total)
	hook := hotplugRemovedHook(func() *block.Memfd { return m }, block.Range{Start: base, Size: total}, logger.NewNopLogger())

	// A balloon REMOVE in boot memory and an unplug straddling into the
	// hotplug region.
	hook(t.Context(), block.Range{Start: 0, Size: page})
	hook(t.Context(), block.Range{Start: base - page, Size: 3 * page})

	got := make([]byte, base+total)
	_, err := m.ReadAt(got, 0)
	require.NoError(t, err)

	for i := range base {
		require.Equal(t, byte(0xa5), got[i], "boot memory byte %d must stay", i)
	}
	assert.Equal(t, make([]byte, 2*page), got[base:base+2*page], "the unplugged hotplug pages read as zeros")
	for i := base + 2*page; i < base+total; i++ {
		require.Equal(t, byte(0xa5), got[i], "still-plugged hotplug byte %d must stay", i)
	}

	// A handed-off memfd (snapshot export) is skipped.
	none := hotplugRemovedHook(func() *block.Memfd { return nil }, block.Range{Start: base, Size: total}, logger.NewNopLogger())
	none(t.Context(), block.Range{Start: base, Size: page})
}
