//go:build linux

package uffd

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
func TestHotplugRelease_PunchesOnlyTheHotplugRegion(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	base, total := 4*page, 8*page
	m := newPatternMemfd(t, base+total)
	release := newHotplugRelease(func() *block.Memfd { return m }, block.Range{Start: base, Size: total}, logger.NewNopLogger())

	// A balloon REMOVE in boot memory and an unplug straddling into the
	// hotplug region.
	release.onRemoved(t.Context(), block.Range{Start: 0, Size: page})
	release.onRemoved(t.Context(), block.Range{Start: base - page, Size: 3 * page})

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

	assert.Equal(t, HotplugReleaseStats{
		Removes:      2,
		HotplugBytes: 2 * page,
		BootBytes:    2 * page,
		PunchedBytes: 2 * page,
	}, release.Stats())
}

// A handed-off memfd (snapshot export) is skipped and counted.
func TestHotplugRelease_SkipsAHandedOffMemfd(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	release := newHotplugRelease(func() *block.Memfd { return nil }, block.Range{Start: 4 * page, Size: 8 * page}, logger.NewNopLogger())
	release.onRemoved(t.Context(), block.Range{Start: 4 * page, Size: page})

	assert.Equal(t, HotplugReleaseStats{Removes: 1, HotplugBytes: page, SkippedBytes: page}, release.Stats())
}

// A punch the kernel refuses is counted, warned once, and does not stop
// later punches.
func TestHotplugRelease_CountsPunchFailures(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	base, total := 4*page, 8*page
	m := newPatternMemfd(t, base+total)
	core, logs := observer.New(zapcore.DebugLevel)
	release := newHotplugRelease(func() *block.Memfd { return m }, block.Range{Start: base, Size: total}, logger.NewTracedLoggerFromCore(core))

	// Out of the memfd's bounds: the memfd is smaller than the region claims.
	shortMemfd := newPatternMemfd(t, base)
	short := newHotplugRelease(func() *block.Memfd { return shortMemfd }, block.Range{Start: base, Size: total}, logger.NewTracedLoggerFromCore(core))
	short.onRemoved(t.Context(), block.Range{Start: base, Size: page})
	short.onRemoved(t.Context(), block.Range{Start: base + page, Size: page})
	assert.Equal(t, uint64(2), short.Stats().PunchFailures)
	assert.Zero(t, short.Stats().PunchedBytes)
	assert.Equal(t, 1, logs.FilterMessage("memory hotplug: could not release unplugged memory").Len(), "warned once")

	release.onRemoved(t.Context(), block.Range{Start: base, Size: page})
	assert.Equal(t, page, release.Stats().PunchedBytes)
}

// The reporter logs a summary only when the counters moved, and a final one
// at teardown.
func TestHotplugRelease_ReportsOnlyChanges(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	base, total := 4*page, 8*page
	m := newPatternMemfd(t, base+total)
	core, logs := observer.New(zapcore.InfoLevel)
	release := newHotplugRelease(func() *block.Memfd { return m }, block.Range{Start: base, Size: total}, logger.NewTracedLoggerFromCore(core))

	release.report(t.Context(), "summary")
	assert.Zero(t, logs.Len(), "nothing happened: no summary")

	release.onRemoved(t.Context(), block.Range{Start: base, Size: 2 * page})
	release.report(t.Context(), "summary")
	release.report(t.Context(), "summary")
	require.Equal(t, 1, logs.Len(), "one summary per change")
	fields := logs.All()[0].ContextMap()
	assert.Equal(t, 2*page, fields["punched_bytes"])
	assert.Equal(t, uint64(1), fields["removes"])

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		release.reportLoop(t.Context(), time.Hour, done)
	}()
	release.onRemoved(t.Context(), block.Range{Start: base + 2*page, Size: page})
	close(done)
	<-finished
	require.Equal(t, 2, logs.Len(), "final summary at teardown")
	assert.Equal(t, 3*page, logs.All()[1].ContextMap()["punched_bytes"])
}
