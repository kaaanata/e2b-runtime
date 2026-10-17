package server

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

func TestResolveTemplateLayout(t *testing.T) {
	t.Parallel()

	blockParent := metadata.Template{
		Layout:    metadata.LayoutBlockV1,
		Workspace: &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 2 << 30}},
	}
	parents := func(m metadata.Template, err error) parentMetadata {
		return func(context.Context, string) (metadata.Template, error) { return m, err }
	}
	noParent := parents(metadata.Template{}, errors.New("unused"))
	fromTemplate := &templatemanager.TemplateConfig_FromTemplate{FromTemplate: &templatemanager.FromTemplateConfig{BuildID: "parent"}}

	t.Run("legacy by default", func(t *testing.T) {
		t.Parallel()

		layout, ws, err := resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{}, noParent)
		require.NoError(t, err)
		assert.Equal(t, metadata.LayoutLegacy, layout)
		assert.Nil(t, ws)
	})

	t.Run("block-v1 with slots", func(t *testing.T) {
		t.Parallel()

		layout, ws, err := resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{
			Layout: new(metadata.LayoutBlockV1), WorkspaceSlotCapacityBytes: []int64{1 << 30, 1 << 30, 1 << 30, 1 << 30},
		}, noParent)
		require.NoError(t, err)
		assert.Equal(t, metadata.LayoutBlockV1, layout)
		assert.Equal(t, 4, ws.Slots)
	})

	for name, cfg := range map[string]*templatemanager.TemplateConfig{
		"block-v1 without slots":   {Layout: new(metadata.LayoutBlockV1)},
		"too many slots":           {Layout: new(metadata.LayoutBlockV1), WorkspaceSlotCapacityBytes: []int64{1, 1, 1, 1, 1}},
		"non-positive slot":        {Layout: new(metadata.LayoutBlockV1), WorkspaceSlotCapacityBytes: []int64{0}},
		"slots on a legacy layout": {WorkspaceSlotCapacityBytes: []int64{1 << 30}},
		"unknown layout":           {Layout: new("block-v9")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, _, err := resolveTemplateLayout(t.Context(), cfg, noParent)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}

	t.Run("a child build inherits its parent's layout", func(t *testing.T) {
		t.Parallel()

		layout, ws, err := resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{Source: fromTemplate}, parents(blockParent, nil))
		require.NoError(t, err)
		assert.Equal(t, metadata.LayoutBlockV1, layout)
		assert.Equal(t, blockParent.Workspace, ws)
	})

	t.Run("a child build may repeat but not change its parent's layout", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{
			Source: fromTemplate, Layout: new(metadata.LayoutBlockV1), WorkspaceSlotCapacityBytes: []int64{1 << 30, 2 << 30},
		}, parents(blockParent, nil))
		require.NoError(t, err)

		_, _, err = resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{
			Source: fromTemplate, Layout: new(metadata.LayoutLegacy),
		}, parents(blockParent, nil))
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("a parent with an unknown layout is refused", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolveTemplateLayout(t.Context(), &templatemanager.TemplateConfig{Source: fromTemplate},
			parents(metadata.Template{}, metadata.ErrUnsupportedLayout))
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}

func TestWithBuildLayout(t *testing.T) {
	t.Parallel()

	legacy := withBuildLayout(&templatemanager.TemplateBuildMetadata{EnvdVersionKey: "0.10.0"}, metadata.LayoutLegacy, nil)
	require.NotNil(t, legacy.Layout, "a rootfs-only build still reports its layout")
	assert.Empty(t, legacy.GetLayout())
	assert.Empty(t, legacy.GetWorkspaceSlotCapacityBytes())
	assert.Equal(t, "0.10.0", legacy.GetEnvdVersionKey())

	ws := &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 2 << 30}}
	block := withBuildLayout(&templatemanager.TemplateBuildMetadata{}, metadata.LayoutBlockV1, ws)
	assert.Equal(t, metadata.LayoutBlockV1, block.GetLayout())
	assert.Equal(t, []int64{1 << 30, 2 << 30}, block.GetWorkspaceSlotCapacityBytes())

	ws.SlotCapacityBytes[0] = 0
	assert.Equal(t, int64(1<<30), block.GetWorkspaceSlotCapacityBytes()[0], "the metadata owns its copy")
}
