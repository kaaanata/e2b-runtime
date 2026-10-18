//go:build linux

package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCPUMax(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]float64{
		"max 100000\n":    0,
		"400000 100000\n": 4,
		"50000 100000":    0.5,
	} {
		got, err := parseCPUMax(in)
		require.NoError(t, err, in)
		assert.InDelta(t, want, got, 1e-9, in)
	}

	for _, bad := range []string{"", "max", "x 100000", "100 0"} {
		_, err := parseCPUMax(bad)
		require.Error(t, err, bad)
	}
}

func TestSetCPUQuota_WritesCPUMax(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	h := &CgroupHandle{cgroupName: "sbx-test", path: dir}

	require.NoError(t, h.SetCPUQuota(4))
	data, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	require.NoError(t, err)
	assert.Equal(t, "400000 100000", string(data))

	q, err := h.CPUQuota()
	require.NoError(t, err)
	assert.InDelta(t, 4.0, q, 1e-9)

	require.NoError(t, h.SetCPUQuota(0))
	data, err = os.ReadFile(filepath.Join(dir, "cpu.max"))
	require.NoError(t, err)
	assert.Equal(t, "max 100000", string(data))
}

func TestSetCPUQuota_NoCgroup(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, newNoopHandle("sbx-x").SetCPUQuota(4), ErrNoCPUQuota)

	var nilHandle *CgroupHandle
	require.ErrorIs(t, nilHandle.SetCPUQuota(4), ErrNoCPUQuota)
}

// On a real cgroup v2 (privileged test container or the host) the kernel
// accepts the value and reads it back.
func TestSetCPUQuota_RealCgroup(t *testing.T) {
	t.Parallel()

	requireWritableCgroup(t)

	ctx := t.Context()
	m, err := NewManager()
	require.NoError(t, err)
	require.NoError(t, m.Initialize(ctx))

	h, err := m.Create(ctx, "sbx-cpu-quota-test")
	require.NoError(t, err)
	defer h.Remove(ctx)

	require.NoError(t, h.SetCPUQuota(16))
	q, err := h.CPUQuota()
	require.NoError(t, err)
	assert.InDelta(t, 16.0, q, 1e-9)

	require.NoError(t, h.SetCPUQuota(2))
	q, err = h.CPUQuota()
	require.NoError(t, err)
	assert.InDelta(t, 2.0, q, 1e-9)
}
