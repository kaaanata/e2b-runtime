//go:build linux

package base

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/buildcontext"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func layoutContext(layout string, ws *metadata.WorkspaceLayout) buildcontext.BuildContext {
	return buildcontext.BuildContext{
		Config: config.TemplateConfig{
			DiskSizeMB: testDiskSizeMB,
			Layout:     layout,
			Workspace:  ws,
		},
	}
}

// The device table is part of the base snapshot, so the layout is part of the
// base-layer key — but only once a build asks for one. A legacy build must hash
// exactly as before the field existed.
func TestLayoutCacheKey(t *testing.T) {
	t.Parallel()

	legacy := testLayerKey(layoutContext(metadata.LayoutLegacy, nil))
	assert.Equal(t, legacyLayerKey(), legacy, "a legacy build's key is unchanged")

	two := &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 1 << 30}}
	block := testLayerKey(layoutContext(metadata.LayoutBlockV1, two))
	assert.NotEqual(t, legacy, block, "a block-layout base layer cannot serve a legacy build")

	bigger := &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 2 << 30}}
	assert.NotEqual(t, block, testLayerKey(layoutContext(metadata.LayoutBlockV1, bigger)), "slot capacities are part of the device table")

	one := &metadata.WorkspaceLayout{Slots: 1, SlotCapacityBytes: []int64{1 << 30}}
	assert.NotEqual(t, block, testLayerKey(layoutContext(metadata.LayoutBlockV1, one)), "so is the slot count")

	assert.Equal(t, block, testLayerKey(layoutContext(metadata.LayoutBlockV1, two)), "the key is stable")
}

// The hotplug device is part of the base snapshot as well; a build without
// one keeps the key it always had.
func TestMemoryHotplugCacheKey(t *testing.T) {
	t.Parallel()

	withHotplug := func(l *metadata.MemoryHotplugLayout) buildcontext.BuildContext {
		bc := layoutContext(metadata.LayoutLegacy, nil)
		bc.Config.MemoryHotplug = l

		return bc
	}

	assert.Equal(t, legacyLayerKey(), testLayerKey(withHotplug(nil)), "no device: unchanged key")

	small := &metadata.MemoryHotplugLayout{BaseMiB: 1024, TotalMiB: 1024, SlotMiB: 128, BlockMiB: 2}
	big := &metadata.MemoryHotplugLayout{BaseMiB: 1024, TotalMiB: 2048, SlotMiB: 128, BlockMiB: 2}

	key := testLayerKey(withHotplug(small))
	assert.NotEqual(t, legacyLayerKey(), key, "a hotplug base layer cannot serve a fixed-size build")
	assert.NotEqual(t, key, testLayerKey(withHotplug(big)), "the region size is part of the key")
	assert.Equal(t, key, testLayerKey(withHotplug(small.Clone())), "the key is stable")
}

func TestCPUHotplugCacheKey(t *testing.T) {
	t.Parallel()

	withCPU := func(l *metadata.CPUHotplugLayout) buildcontext.BuildContext {
		bc := layoutContext(metadata.LayoutLegacy, nil)
		bc.Config.CPUHotplug = l

		return bc
	}

	assert.Equal(t, legacyLayerKey(), testLayerKey(withCPU(nil)), "fixed vCPUs: unchanged key")

	key := testLayerKey(withCPU(&metadata.CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}))
	assert.NotEqual(t, legacyLayerKey(), key)
	assert.NotEqual(t, key, testLayerKey(withCPU(&metadata.CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 16})))
}
