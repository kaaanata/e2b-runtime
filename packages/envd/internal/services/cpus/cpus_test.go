package cpus

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSysfs lays out present/online and per-CPU online files. Writes to a
// cpuN/online file do not update "online" by themselves, so tests refresh it
// from the per-CPU files (the kernel does that in reality).
type fakeSysfs struct {
	root    string
	present int
}

func newFake(t *testing.T, present int, online []int, noControl ...int) *fakeSysfs {
	t.Helper()

	f := &fakeSysfs{root: t.TempDir(), present: present}
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "present"), []byte("0-"+strconv.Itoa(present-1)+"\n"), 0o644))

	for cpu := range present {
		dir := filepath.Join(f.root, "cpu"+strconv.Itoa(cpu))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		if cpu == 0 || slices.Contains(noControl, cpu) {
			continue
		}

		val := "0"
		if slices.Contains(online, cpu) {
			val = "1"
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "online"), []byte(val), 0o644))
	}

	f.refresh(t)

	return f
}

func (f *fakeSysfs) refresh(t *testing.T) {
	t.Helper()

	parts := []string{"0"}
	for cpu := 1; cpu < f.present; cpu++ {
		data, err := os.ReadFile(filepath.Join(f.root, "cpu"+strconv.Itoa(cpu), "online"))
		if err == nil && strings.TrimSpace(string(data)) == "1" {
			parts = append(parts, strconv.Itoa(cpu))
		}
	}

	require.NoError(t, os.WriteFile(filepath.Join(f.root, "online"), []byte(strings.Join(parts, ",")+"\n"), 0o644))
}

func (f *fakeSysfs) cpuOnline(t *testing.T, cpu int) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(f.root, "cpu"+strconv.Itoa(cpu), "online"))
	require.NoError(t, err)

	return string(data)
}

func TestParseList(t *testing.T) {
	t.Parallel()

	got, err := ParseList("0-3,8,10-11\n")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2, 3, 8, 10, 11}, got)

	got, err = ParseList("")
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = ParseList("3-1")
	require.Error(t, err)
	_, err = ParseList("a")
	require.Error(t, err)
}

func TestSet_GrowAndShrink(t *testing.T) {
	t.Parallel()

	f := newFake(t, 32, []int{1, 2, 3})
	c := New(f.root)

	st, err := c.Get()
	require.NoError(t, err)
	assert.Equal(t, State{Online: 4, Present: 32, Hotpluggable: true}, st)

	st, err = c.Set(16)
	require.NoError(t, err)
	assert.Equal(t, int64(12), st.Changed, "cpus 4..15 brought online, 1..3 untouched")
	assert.Equal(t, "1", f.cpuOnline(t, 15))
	assert.Equal(t, "0", f.cpuOnline(t, 16))
	assert.Empty(t, st.Errors)
	f.refresh(t)

	st, err = c.Set(2)
	require.NoError(t, err)
	assert.Equal(t, int64(14), st.Changed, "cpus 15..2 offlined")
	assert.Equal(t, "1", f.cpuOnline(t, 1))
	assert.Equal(t, "0", f.cpuOnline(t, 2))
	f.refresh(t)

	st, err = c.Get()
	require.NoError(t, err)
	assert.Equal(t, int64(2), st.Online)
}

func TestSet_FixesAGuestThatOnlinedTooMany(t *testing.T) {
	t.Parallel()

	// The guest's root onlined cpu 20 on its own; a Set to 4 takes it back.
	f := newFake(t, 32, []int{1, 2, 3, 20})
	st, err := New(f.root).Set(4)
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Changed)
	assert.Equal(t, "0", f.cpuOnline(t, 20))
}

func TestSet_OutOfRange(t *testing.T) {
	t.Parallel()

	f := newFake(t, 8, nil)
	_, err := New(f.root).Set(0)
	require.ErrorIs(t, err, ErrOutOfRange)
	_, err = New(f.root).Set(9)
	require.ErrorIs(t, err, ErrOutOfRange)
}

func TestSet_NoControlIsReported(t *testing.T) {
	t.Parallel()

	// A kernel without CONFIG_HOTPLUG_CPU has no cpuN/online files.
	f := newFake(t, 4, nil, 1, 2, 3)
	c := New(f.root)

	st, err := c.Get()
	require.NoError(t, err)
	assert.False(t, st.Hotpluggable)

	st, err = c.Set(3)
	require.NoError(t, err)
	assert.Len(t, st.Errors, 2, "cpu1 and cpu2 have no control")
	assert.Zero(t, st.Changed)
}
