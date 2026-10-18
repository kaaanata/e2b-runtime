//go:build linux

package cgroups

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCgroup2Manager_SetMemoryLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	user := filepath.Join(dir, "user")
	noMemory := filepath.Join(dir, "socats")
	require.NoError(t, os.MkdirAll(user, 0o755))
	require.NoError(t, os.MkdirAll(noMemory, 0o755))
	for _, f := range []string{"memory.max", "memory.high"} {
		require.NoError(t, os.WriteFile(filepath.Join(user, f), []byte("8208056320\n"), 0o600))
	}

	m := Cgroup2Manager{cgroupPaths: map[ProcessType]string{ProcessTypeUser: user, ProcessTypeSocat: noMemory}}

	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(user, f))
		require.NoError(t, err)

		return string(b)
	}

	for _, limit := range []uint64{33176797184, 4 << 30} { // raise, then lower
		require.NoError(t, m.SetMemoryLimit(ProcessTypeUser, limit))
		assert.Equal(t, strconv.FormatUint(limit, 10), read("memory.max"))
		assert.Equal(t, strconv.FormatUint(limit, 10), read("memory.high"))
	}

	require.NoError(t, m.SetMemoryLimit(ProcessTypeSocat, 1<<30), "no memory controller: left alone")
	require.Error(t, m.SetMemoryLimit(ProcessTypePTY, 1<<30), "unknown cgroup")
}
