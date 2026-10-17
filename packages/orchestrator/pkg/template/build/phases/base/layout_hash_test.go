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
