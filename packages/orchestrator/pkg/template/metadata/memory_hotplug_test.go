package metadata

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMemoryHotplugLayout(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		base, max int64
		want      *MemoryHotplugLayout
		wantErr   bool
	}{
		"no device when max equals base": {base: 8192, max: 8192},
		"agent template 8 -> 64 GiB": {
			base: 8192, max: 65536,
			want: &MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2},
		},
		"one slot":                      {base: 1024, max: 1152, want: &MemoryHotplugLayout{BaseMiB: 1024, TotalMiB: 128, SlotMiB: 128, BlockMiB: 2}},
		"max below base":                {base: 2048, max: 1024, wantErr: true},
		"region not a slot multiple":    {base: 1024, max: 1100, wantErr: true},
		"odd base is not block aligned": {base: 1023, max: 1023 + 128, wantErr: true},
		"zero base":                     {base: 0, max: 128, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := NewMemoryHotplugLayout(tc.base, tc.max)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMemoryHotplugLayout)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMemoryHotplugLayout_Validate(t *testing.T) {
	t.Parallel()

	valid := MemoryHotplugLayout{BaseMiB: 1024, TotalMiB: 1024, SlotMiB: 128, BlockMiB: 2}
	require.NoError(t, valid.Validate())

	for name, mut := range map[string]func(*MemoryHotplugLayout){
		"block not power of two": func(l *MemoryHotplugLayout) { l.BlockMiB = 6 },
		"block below minimum":    func(l *MemoryHotplugLayout) { l.BlockMiB = 1 },
		"slot below minimum":     func(l *MemoryHotplugLayout) { l.SlotMiB = 64 },
		"slot not block multiple": func(l *MemoryHotplugLayout) {
			l.BlockMiB = 256
		},
		"empty region": func(l *MemoryHotplugLayout) { l.TotalMiB = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := valid
			mut(&l)
			require.ErrorIs(t, l.Validate(), ErrMemoryHotplugLayout)
		})
	}
}

func TestMemoryHotplugLayout_Math(t *testing.T) {
	t.Parallel()

	l := MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2}

	assert.Equal(t, int64(65536), l.MaxMiB())
	assert.Equal(t, int64(65536)<<20, l.MemfileSizeBytes())
	assert.Equal(t, int64(8192)<<20, l.HotplugOffsetBytes())
	assert.Equal(t, int64(57344)<<20, l.HotplugSizeBytes())

	for memoryMB, want := range map[int64]int64{
		8192:  0,     // base: nothing plugged
		8193:  2,     // rounded up to one block
		16384: 8192,  // whole blocks stay
		65535: 57344, // rounding never passes the slot-aligned maximum
		65536: 57344,
	} {
		got, err := l.RequestedHotplugMiB(memoryMB)
		require.NoError(t, err, memoryMB)
		assert.Equal(t, want, got, memoryMB)
	}

	for _, memoryMB := range []int64{8191, 65537, 0} {
		_, err := l.RequestedHotplugMiB(memoryMB)
		require.ErrorIs(t, err, ErrMemorySizeOutOfRange, memoryMB)
	}

	assert.Equal(t, int64(8192+4096), l.AccountedMiB(4096, 0), "grow pending: the request is promised")
	assert.Equal(t, int64(8192+4096), l.AccountedMiB(1024, 4096), "shrink stalled: still plugged")
	assert.Equal(t, int64(8192), l.AccountedMiB(0, 0))
}

func TestWithMemoryHotplug_StampsAndClearsCapability(t *testing.T) {
	t.Parallel()

	l := &MemoryHotplugLayout{BaseMiB: 1024, TotalMiB: 1024, SlotMiB: 128, BlockMiB: 2}
	m := Template{Version: CurrentVersion, RequiredCapabilities: []string{"other"}}

	with := m.WithMemoryHotplug(l)
	assert.Equal(t, l, with.MemoryHotplug)
	assert.NotSame(t, l, with.MemoryHotplug, "the layout is copied, not aliased")
	assert.Equal(t, []string{"other", CapabilityMemoryHotplugV1}, with.RequiredCapabilities)
	assert.Equal(t, []string{"other"}, m.RequiredCapabilities, "the receiver is not mutated")

	again := with.WithMemoryHotplug(l)
	assert.Equal(t, []string{"other", CapabilityMemoryHotplugV1}, again.RequiredCapabilities, "stamping twice does not duplicate")

	without := with.WithMemoryHotplug(nil)
	assert.Nil(t, without.MemoryHotplug)
	assert.Equal(t, []string{"other"}, without.RequiredCapabilities)
	assert.Nil(t, Template{}.WithMemoryHotplug(l).WithMemoryHotplug(nil).RequiredCapabilities)
}

func TestCopyConstructorsCarryMemoryHotplug(t *testing.T) {
	t.Parallel()

	l := &MemoryHotplugLayout{BaseMiB: 2048, TotalMiB: 2048, SlotMiB: 128, BlockMiB: 2}
	src := Template{Version: CurrentVersion}.WithMemoryHotplug(l).MarkMemoryHotplugPlugged(512)

	for name, got := range map[string]Template{
		"SameVersionTemplate": src.SameVersionTemplate(TemplateMetadata{BuildID: "b2"}),
		"NewVersionTemplate":  src.NewVersionTemplate(TemplateMetadata{BuildID: "b2"}),
		"BasedOn":             src.BasedOn(FromTemplate{BuildID: "b1"}),
	} {
		assert.Equal(t, l, got.MemoryHotplug, name)
		assert.Contains(t, got.RequiredCapabilities, CapabilityMemoryHotplugV1, name)
		assert.Zero(t, got.MemoryHotplugPluggedMiB, "%s: the plugged size describes one snapshot", name)
	}

	same := src.WithPrefetch(nil)
	assert.Equal(t, l, same.MemoryHotplug)
	assert.Equal(t, int64(512), same.MemoryHotplugPluggedMiB, "WithPrefetch rewrites the same snapshot")
}

func TestMemoryHotplugSurvivesFileRoundTrip(t *testing.T) {
	t.Parallel()

	l := &MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2}
	src := Template{Version: CurrentVersion, Template: TemplateMetadata{BuildID: "b1"}}.
		WithMemoryHotplug(l).MarkMemoryHotplugPlugged(4096)

	path := filepath.Join(t.TempDir(), "metadata.json")
	require.NoError(t, src.ToFile(path))

	got, err := FromFile(path)
	require.NoError(t, err)
	assert.Equal(t, l, got.MemoryHotplug)
	assert.Equal(t, int64(4096), got.MemoryHotplugPluggedMiB)
	assert.True(t, got.HasMemoryHotplug())
}

func TestDeserialize_MemoryHotplug(t *testing.T) {
	t.Parallel()

	legacy := `{"version": 2, "template": {"build_id": "b1"}}`
	got, err := deserialize(bytes.NewReader([]byte(legacy)))
	require.NoError(t, err)
	assert.False(t, got.HasMemoryHotplug(), "pre-existing metadata has no device")

	malformed := `{"version": 2, "template": {"build_id": "b1"}, "required_capabilities": ["memory-hotplug-v1"],
	              "memory_hotplug": {"base_mib": 1024, "total_mib": 100, "slot_mib": 128, "block_mib": 2}}`
	_, err = deserialize(bytes.NewReader([]byte(malformed)))
	require.ErrorIs(t, err, ErrMemoryHotplugLayout)
}

// A reader that predates hotplug must refuse the template: this is the bridge
// reader rule, and KnownCapabilities is what it consults.
func TestMemoryHotplugCapabilityIsKnownHereOnly(t *testing.T) {
	t.Parallel()

	_, ok := KnownCapabilities[CapabilityMemoryHotplugV1]
	require.True(t, ok)

	err := Template{RequiredCapabilities: []string{CapabilityMemoryHotplugV1 + "-next"}}.checkCapabilities()
	require.ErrorIs(t, err, ErrUnsupportedLayout)
}
