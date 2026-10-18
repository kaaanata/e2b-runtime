package cgroups

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProcMeminfo is the guest's meminfo, the MemTotal source in production.
const ProcMeminfo = "/proc/meminfo"

// MemoryLimitFollowInterval is how often envd re-reads MemTotal to keep the
// cgroup memory limits in step with the guest's memory.
const MemoryLimitFollowInterval = time.Second

// maxMemoryReserved caps the memory envd keeps outside the limited cgroups.
const maxMemoryReserved = 128 << 20

// MemoryLimit is the memory.max (and memory.high) of the limited cgroups for
// a guest with memTotal bytes of memory: keep 1/8 of the memory free, but no
// more than 128 MiB.
func MemoryLimit(memTotal uint64) uint64 {
	return memTotal - min(memTotal/8, maxMemoryReserved)
}

// ReadMemTotal returns MemTotal, in bytes, from a /proc/meminfo-format file.
func ReadMemTotal(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}

		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal %q: %w", fields[1], err)
		}
		if len(fields) > 2 && fields[2] != "kB" {
			return 0, fmt.Errorf("unexpected MemTotal unit %q", fields[2])
		}

		return kib << 10, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}

	return 0, errors.New("no MemTotal in " + path)
}

// MemoryLimiter sets a cgroup's memory limit.
type MemoryLimiter interface {
	SetMemoryLimit(procType ProcessType, limit uint64) error
}

// FollowMemTotal keeps the memory limit of procTypes at MemoryLimit(MemTotal)
// as the guest's memory changes, until ctx ends. The limits are computed once
// at startup, but a guest's memory is not fixed: virtio-mem hot-resize plugs
// and unplugs memory while envd runs, and a snapshot is resumed at another
// size without restarting envd. A limit left at the boot size would cap the
// workload at the boot memory however much is plugged (and OOM-kill it at
// that size).
//
// applied is the MemTotal the limits were last set for. A failed update is
// retried on the next tick and logged once until it succeeds again.
func FollowMemTotal(
	ctx context.Context,
	limiter MemoryLimiter,
	procTypes []ProcessType,
	applied uint64,
	interval time.Duration,
	readMemTotal func() (uint64, error),
	logf func(format string, args ...any),
) {
	t := time.NewTicker(interval)
	defer t.Stop()

	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		memTotal, err := readMemTotal()
		if err != nil {
			if !failing {
				logf("memory limit: cannot read MemTotal: %v", err)
			}
			failing = true

			continue
		}
		if memTotal == applied {
			continue
		}

		limit := MemoryLimit(memTotal)
		var errs []error
		for _, pt := range procTypes {
			if err := limiter.SetMemoryLimit(pt, limit); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", pt, err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			if !failing {
				logf("memory limit: cannot follow MemTotal %d -> %d (limit %d): %v", applied, memTotal, limit, err)
			}
			failing = true

			continue
		}

		logf("memory limit: MemTotal %d -> %d, memory.max/memory.high set to %d", applied, memTotal, limit)
		applied = memTotal
		failing = false
	}
}
