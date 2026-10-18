//go:build linux

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func hotplugLayout() *metadata.MemoryHotplugLayout {
	return &metadata.MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2}
}

func TestConfig_MemoryDefaults(t *testing.T) {
	t.Parallel()

	fixed := NewConfig(Config{RamMB: 4096})
	assert.Equal(t, int64(4096), fixed.RequestedMemoryMB())
	assert.Equal(t, int64(4096), fixed.CurrentMemoryMB())
	assert.Equal(t, int64(4096), fixed.MaxMemoryMB())
	assert.Equal(t, int64(4096), fixed.AccountedMemoryMB())
	assert.Equal(t, int64(4096)<<20, fixed.MemfileSizeBytes())

	hot := NewConfig(Config{RamMB: 8192, MemoryHotplug: hotplugLayout()}).WithRequestedMemoryMB(16384)
	assert.Equal(t, int64(16384), hot.RequestedMemoryMB())
	assert.Equal(t, int64(8192), hot.CurrentMemoryMB(), "nothing plugged yet")
	assert.Equal(t, int64(65536), hot.MaxMemoryMB())
	assert.Equal(t, int64(65536)<<20, hot.MemfileSizeBytes(), "the memory file covers the whole region")
}

func TestConfig_AccountedMemory(t *testing.T) {
	t.Parallel()

	c := NewConfig(Config{RamMB: 8192, MemoryHotplug: hotplugLayout()}).WithRequestedMemoryMB(16384)
	assert.Equal(t, int64(16384), c.AccountedMemoryMB(), "a grow is promised before it is plugged")

	c.setMemory(16384, 8192)
	assert.Equal(t, int64(16384), c.AccountedMemoryMB())
	assert.Equal(t, int64(16384), c.CurrentMemoryMB())

	c.setMemory(8192, 8192)
	assert.Equal(t, int64(16384), c.AccountedMemoryMB(), "a stalled shrink is still plugged")

	c.setMemory(8192, 0)
	assert.Equal(t, int64(8192), c.AccountedMemoryMB())

	assert.Equal(t, MemoryAccountingRequested, parseMemoryAccounting(""))
	assert.Equal(t, MemoryAccountingRequested, parseMemoryAccounting("nonsense"))
	assert.Equal(t, MemoryAccountingBoot, parseMemoryAccounting("boot"))
}

func TestConfig_AdoptMemoryHotplug(t *testing.T) {
	t.Parallel()

	withDevice := metadata.Template{}.WithMemoryHotplug(hotplugLayout()).MarkMemoryHotplugPlugged(4096)
	fixed := metadata.Template{}

	t.Run("fixed template, default size", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 4096})
		require.NoError(t, c.adoptMemoryHotplug(fixed))
		assert.Nil(t, c.MemoryHotplug)
		assert.Equal(t, int64(4096), c.RequestedMemoryMB())
	})

	t.Run("fixed template refuses another size", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 4096}).WithRequestedMemoryMB(8192)
		require.ErrorIs(t, c.adoptMemoryHotplug(fixed), ErrMemoryNotHotResizable)
	})

	t.Run("fixed template refuses a config naming a device", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 8192, MemoryHotplug: hotplugLayout()})
		require.ErrorIs(t, c.adoptMemoryHotplug(fixed), ErrMemoryNotHotResizable)
	})

	t.Run("hotplug template, default size is the snapshot's", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 8192})
		require.NoError(t, c.adoptMemoryHotplug(withDevice))
		assert.Equal(t, hotplugLayout(), c.MemoryHotplug)
		assert.Equal(t, int64(8192+4096), c.RequestedMemoryMB(), "an older caller keeps the paused size")
		assert.Equal(t, int64(8192+4096), c.CurrentMemoryMB())

		want, err := c.resumeHotplugRequest(withDevice)
		require.NoError(t, err)
		assert.Nil(t, want, "nothing to request: the snapshot restores it")
	})

	t.Run("hotplug template, resize on resume", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 8192}).WithRequestedMemoryMB(32768)
		require.NoError(t, c.adoptMemoryHotplug(withDevice))
		assert.Equal(t, int64(32768), c.RequestedMemoryMB())
		assert.Equal(t, int64(8192+4096), c.CurrentMemoryMB(), "plugged as at pause until the guest acts")

		want, err := c.resumeHotplugRequest(withDevice)
		require.NoError(t, err)
		require.NotNil(t, want)
		assert.Equal(t, int64(24576), *want)

		shrink := NewConfig(Config{RamMB: 8192}).WithRequestedMemoryMB(8192)
		require.NoError(t, shrink.adoptMemoryHotplug(withDevice))
		want, err = shrink.resumeHotplugRequest(withDevice)
		require.NoError(t, err)
		require.NotNil(t, want)
		assert.Zero(t, *want)
	})

	t.Run("hotplug template refuses sizes outside the range", func(t *testing.T) {
		t.Parallel()

		too := NewConfig(Config{RamMB: 8192}).WithRequestedMemoryMB(65536 + 128)
		require.ErrorIs(t, too.adoptMemoryHotplug(withDevice), metadata.ErrMemorySizeOutOfRange)

		low := NewConfig(Config{RamMB: 8192}).WithRequestedMemoryMB(4096)
		require.ErrorIs(t, low.adoptMemoryHotplug(withDevice), metadata.ErrMemorySizeOutOfRange)
	})

	// A template build resumes each intermediate layer with a config naming
	// the build's device (layer.ResumeSandbox). Its layer snapshot was taken
	// with nothing plugged, so the resume keeps the boot size; the same config
	// against a layer metadata missing the device is the failure every step of
	// a hotplug build hit before every layer was stamped.
	t.Run("build resume of a layer that carries the device", func(t *testing.T) {
		t.Parallel()

		buildLayer := metadata.Template{}.WithMemoryHotplug(hotplugLayout()).MarkMemoryHotplugPlugged(0)
		c := NewConfig(Config{RamMB: 8192, MemoryHotplug: hotplugLayout()})
		require.NoError(t, c.adoptMemoryHotplug(buildLayer))
		assert.Equal(t, hotplugLayout(), c.MemoryHotplug)
		assert.Equal(t, int64(8192), c.RequestedMemoryMB())

		want, err := c.resumeHotplugRequest(buildLayer)
		require.NoError(t, err)
		assert.Nil(t, want, "the layer restores at boot size")

		unstamped := NewConfig(Config{RamMB: 8192, MemoryHotplug: hotplugLayout()})
		require.ErrorIs(t, unstamped.adoptMemoryHotplug(metadata.Template{}.MarkMemoryHotplugPlugged(0)), ErrMemoryNotHotResizable)
	})

	t.Run("hotplug template refuses another boot memory", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{RamMB: 4096})
		require.Error(t, c.adoptMemoryHotplug(withDevice))

		other := hotplugLayout()
		other.TotalMiB = 128
		mismatch := NewConfig(Config{RamMB: 8192, MemoryHotplug: other})
		require.Error(t, mismatch.adoptMemoryHotplug(withDevice))
	})
}
