package metadata

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCPUHotplugLayout(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		base, max int64
		smt       bool
		want      *CPUHotplugLayout
		wantErr   bool
	}{
		"fixed when max equals base":     {base: 8, max: 8, smt: true},
		"agent template 8 -> 32":         {base: 8, max: 32, smt: true, want: &CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}},
		"odd base is fine":               {base: 3, max: 16, smt: true, want: &CPUHotplugLayout{BaseVCPUs: 3, MaxVCPUs: 16}},
		"odd max without smt":            {base: 2, max: 7, smt: false, want: &CPUHotplugLayout{BaseVCPUs: 2, MaxVCPUs: 7}},
		"odd max with smt":               {base: 2, max: 7, smt: true, wantErr: true},
		"max below base":                 {base: 8, max: 4, wantErr: true},
		"above firecracker's ceiling":    {base: 8, max: 34, smt: true, wantErr: true},
		"zero base":                      {base: 0, max: 4, wantErr: true},
		"firecracker's ceiling is legal": {base: 1, max: 32, smt: true, want: &CPUHotplugLayout{BaseVCPUs: 1, MaxVCPUs: 32}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := NewCPUHotplugLayout(tc.base, tc.max, tc.smt)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrCPUHotplugLayout)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCPUHotplugLayout_CheckRequestedVCPUs(t *testing.T) {
	t.Parallel()

	l := &CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}
	require.NoError(t, l.CheckRequestedVCPUs(8))
	require.NoError(t, l.CheckRequestedVCPUs(17))
	require.NoError(t, l.CheckRequestedVCPUs(32))
	require.ErrorIs(t, l.CheckRequestedVCPUs(7), ErrCPUCountOutOfRange)
	require.ErrorIs(t, l.CheckRequestedVCPUs(33), ErrCPUCountOutOfRange)
}

func TestWithCPUHotplug_StampsAndClearsCapability(t *testing.T) {
	t.Parallel()

	l := &CPUHotplugLayout{BaseVCPUs: 4, MaxVCPUs: 16}
	m := Template{Version: CurrentVersion, RequiredCapabilities: []string{CapabilityMemoryHotplugV1}}

	with := m.WithCPUHotplug(l)
	assert.Equal(t, l, with.CPUHotplug)
	assert.NotSame(t, l, with.CPUHotplug)
	assert.Equal(t, []string{CapabilityMemoryHotplugV1, CapabilityCPUHotplugV1}, with.RequiredCapabilities)
	assert.Equal(t, []string{CapabilityMemoryHotplugV1}, m.RequiredCapabilities, "the receiver is not mutated")
	assert.Equal(t, with.RequiredCapabilities, with.WithCPUHotplug(l).RequiredCapabilities, "stamping twice does not duplicate")

	without := with.WithCPUHotplug(nil)
	assert.Nil(t, without.CPUHotplug)
	assert.Equal(t, []string{CapabilityMemoryHotplugV1}, without.RequiredCapabilities)
	assert.Nil(t, Template{}.WithCPUHotplug(l).WithCPUHotplug(nil).RequiredCapabilities)
}

func TestCopyConstructorsCarryCPUHotplug(t *testing.T) {
	t.Parallel()

	l := &CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}
	src := Template{Version: CurrentVersion}.WithCPUHotplug(l).MarkCPUHotplugVCPUs(16)

	for name, got := range map[string]Template{
		"SameVersionTemplate": src.SameVersionTemplate(TemplateMetadata{BuildID: "b2"}),
		"NewVersionTemplate":  src.NewVersionTemplate(TemplateMetadata{BuildID: "b2"}),
		"BasedOn":             src.BasedOn(FromTemplate{BuildID: "b1"}),
	} {
		assert.Equal(t, l, got.CPUHotplug, name)
		assert.Contains(t, got.RequiredCapabilities, CapabilityCPUHotplugV1, name)
		assert.Zero(t, got.CPUHotplugVCPUs, "%s: the granted count describes one snapshot", name)
	}

	same := src.WithPrefetch(nil)
	assert.Equal(t, l, same.CPUHotplug)
	assert.Equal(t, int64(16), same.CPUHotplugVCPUs, "WithPrefetch rewrites the same snapshot")
}

func TestCPUHotplugSurvivesFileRoundTrip(t *testing.T) {
	t.Parallel()

	l := &CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}
	src := Template{Version: CurrentVersion, Template: TemplateMetadata{BuildID: "b1"}}.
		WithCPUHotplug(l).MarkCPUHotplugVCPUs(12)

	path := filepath.Join(t.TempDir(), "metadata.json")
	require.NoError(t, src.ToFile(path))

	got, err := FromFile(path)
	require.NoError(t, err)
	assert.Equal(t, l, got.CPUHotplug)
	assert.Equal(t, int64(12), got.CPUHotplugVCPUs)
}

func TestDeserialize_CPUHotplug(t *testing.T) {
	t.Parallel()

	legacy := `{"version": 2, "template": {"build_id": "b1"}}`
	got, err := deserialize(bytes.NewReader([]byte(legacy)))
	require.NoError(t, err)
	assert.Nil(t, got.CPUHotplug, "pre-existing metadata has a fixed vCPU count")

	malformed := `{"version": 2, "template": {"build_id": "b1"}, "required_capabilities": ["cpu-hotplug-v1"],
	              "cpu_hotplug": {"base_vcpus": 8, "max_vcpus": 64}}`
	_, err = deserialize(bytes.NewReader([]byte(malformed)))
	require.ErrorIs(t, err, ErrCPUHotplugLayout)

	_, ok := KnownCapabilities[CapabilityCPUHotplugV1]
	require.True(t, ok, "the bridge reader rule: this release knows the capability it writes")
}
