package cgroups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryLimit(t *testing.T) {
	t.Parallel()

	const gib = uint64(1) << 30
	assert.Equal(t, 8*gib-128<<20, MemoryLimit(8*gib), "large guests keep 128 MiB free")
	assert.Equal(t, uint64(896<<20), MemoryLimit(1<<30), "1 GiB keeps 128 MiB (= 1/8) free")
	assert.Equal(t, uint64(448<<20), MemoryLimit(512<<20), "small guests keep 1/8 free")
}

func TestReadMemTotal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	require.NoError(t, os.WriteFile(path, []byte("MemFree:  100 kB\nMemTotal:       33312576 kB\nBuffers: 1 kB\n"), 0o600))

	got, err := ReadMemTotal(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(33312576)*1024, got)

	require.NoError(t, os.WriteFile(path, []byte("MemFree: 1 kB\n"), 0o600))
	_, err = ReadMemTotal(path)
	require.Error(t, err, "no MemTotal line")

	_, err = ReadMemTotal(filepath.Join(dir, "missing"))
	require.Error(t, err)
}

type fakeLimiter struct {
	mu     sync.Mutex
	limits map[ProcessType][]uint64
	fail   bool
}

func (f *fakeLimiter) SetMemoryLimit(pt ProcessType, limit uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail {
		return errors.New("refused")
	}
	if f.limits == nil {
		f.limits = map[ProcessType][]uint64{}
	}
	f.limits[pt] = append(f.limits[pt], limit)

	return nil
}

func (f *fakeLimiter) set(pt ProcessType) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]uint64(nil), f.limits[pt]...)
}

func (f *fakeLimiter) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

// The limits follow MemTotal up (a plug) and down (an unplug), only when it
// changes, and a refused update is retried.
func TestFollowMemTotal(t *testing.T) {
	t.Parallel()

	const gib = uint64(1) << 30
	var mu sync.Mutex
	memTotal := 8 * gib
	read := func() (uint64, error) {
		mu.Lock()
		defer mu.Unlock()

		return memTotal, nil
	}
	setMemTotal := func(v uint64) {
		mu.Lock()
		defer mu.Unlock()
		memTotal = v
	}

	var logMu sync.Mutex
	var logs []string
	logf := func(format string, _ ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, format)
	}

	lim := &fakeLimiter{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		FollowMemTotal(ctx, lim, []ProcessType{ProcessTypePTY, ProcessTypeUser}, 8*gib, time.Millisecond, read, logf)
	}()

	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, lim.set(ProcessTypeUser), "unchanged MemTotal: nothing written")

	setMemTotal(32 * gib)
	require.Eventually(t, func() bool { return len(lim.set(ProcessTypeUser)) == 1 }, time.Second, time.Millisecond)
	assert.Equal(t, []uint64{MemoryLimit(32 * gib)}, lim.set(ProcessTypeUser))
	assert.Equal(t, []uint64{MemoryLimit(32 * gib)}, lim.set(ProcessTypePTY))

	lim.setFail(true)
	setMemTotal(8 * gib)
	time.Sleep(20 * time.Millisecond)
	lim.setFail(false)
	require.Eventually(t, func() bool { return len(lim.set(ProcessTypeUser)) == 2 }, time.Second, time.Millisecond)
	assert.Equal(t, MemoryLimit(8*gib), lim.set(ProcessTypeUser)[1], "retried after the failure")

	cancel()
	<-done

	logMu.Lock()
	defer logMu.Unlock()
	failures := 0
	for _, l := range logs {
		if l == "memory limit: cannot follow MemTotal %d -> %d (limit %d): %v" {
			failures++
		}
	}
	assert.Equal(t, 1, failures, "a run of failures is logged once")
}
