//go:build linux

package layer

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func buildVMConfig(l *metadata.MemoryHotplugLayout) *sandbox.Config {
	return sandbox.NewConfig(sandbox.Config{
		RamMB:         8192,
		MemoryHotplug: l,
		FirecrackerConfig: fc.Config{
			KernelVersion:      "vmlinux-6.1.177_vmem1",
			FirecrackerVersion: "v1.14-0.2.0",
		},
	})
}

// Regression: only the finalize phase stamped the virtio-mem device, so the
// base and step layers of a hotplug build were written without it and the
// next step's resume failed with "the snapshot has no memory hotplug device".
// Every VM-built layer goes through layerMetadata, so each one must carry the
// device its VM booted with — and survive the metafile round trip the resume
// reads it back through.
func TestLayerMetadata_CarriesMemoryHotplugThroughEveryLayer(t *testing.T) {
	t.Parallel()

	l, err := metadata.NewMemoryHotplugLayout(8192, 65536)
	require.NoError(t, err)
	require.NotNil(t, l)

	// The base phase starts from a fresh Template that names no device
	// (phases/base BaseBuilder.Layer).
	base := metadata.Template{Version: metadata.CurrentVersion, Context: metadata.Context{User: "user"}}

	baseLayer := layerMetadata(base, uuid.NewString(), buildVMConfig(l.Clone()))
	assert.Equal(t, l, baseLayer.MemoryHotplug, "base layer")
	assert.Contains(t, baseLayer.RequiredCapabilities, metadata.CapabilityMemoryHotplugV1)
	assert.Equal(t, "vmlinux-6.1.177_vmem1", baseLayer.Template.KernelVersion)
	assert.Equal(t, "v1.14-0.2.0", baseLayer.Template.FirecrackerVersion)

	// A step layer starts as a copy of its source layer with a new build ID
	// (phases/steps StepBuilder.Layer); its VM resumed the base snapshot.
	path := filepath.Join(t.TempDir(), "metadata.json")
	require.NoError(t, baseLayer.ToFile(path))
	stored, err := metadata.FromFile(path)
	require.NoError(t, err, "a reader with hotplug support accepts the capability")
	assert.Equal(t, l, stored.MemoryHotplug, "the resume reads the device back from the metafile")

	step := stored
	step.Template = metadata.TemplateMetadata{BuildID: uuid.NewString()}
	stepLayer := layerMetadata(step, step.Template.BuildID, buildVMConfig(stored.MemoryHotplug.Clone()))
	assert.Equal(t, l, stepLayer.MemoryHotplug, "step layer")
	assert.Equal(t, []string{metadata.CapabilityMemoryHotplugV1}, stepLayer.RequiredCapabilities, "stamped once, not appended per layer")
}

func TestLayerMetadata_NoDeviceClearsStaleField(t *testing.T) {
	t.Parallel()

	l, err := metadata.NewMemoryHotplugLayout(8192, 65536)
	require.NoError(t, err)

	stale := metadata.Template{Version: metadata.CurrentVersion}.WithMemoryHotplug(l)
	got := layerMetadata(stale, uuid.NewString(), buildVMConfig(nil))
	assert.Nil(t, got.MemoryHotplug, "the metadata describes the VM that was paused")
	assert.Empty(t, got.RequiredCapabilities)
}
