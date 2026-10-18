package template_manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

func TestTemplateLayoutFromAPI(t *testing.T) {
	t.Parallel()

	none, err := TemplateLayoutFromAPI(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, TemplateLayout{}, none, "no layout fields ask for nothing (rootfs-only or inherited)")

	block, err := TemplateLayoutFromAPI(new(api.TemplateLayoutBlockV1), &[]int64{10, 20})
	require.NoError(t, err)
	require.NotNil(t, block.Layout)
	assert.Equal(t, "block-v1", *block.Layout)
	assert.Equal(t, []int64{10 << 30, 20 << 30}, block.WorkspaceSlotCapacityBytes)

	_, err = TemplateLayoutFromAPI(new(api.TemplateLayout("block-v2")), nil)
	require.Error(t, err)

	_, err = TemplateLayoutFromAPI(new(api.TemplateLayoutBlockV1), &[]int64{0})
	require.Error(t, err)

	_, err = TemplateLayoutFromAPI(new(api.TemplateLayoutBlockV1), &[]int64{1 << 40})
	require.Error(t, err, "a capacity that overflows bytes is refused")
}

func TestTemplateLayoutFromBuildMetadata(t *testing.T) {
	t.Parallel()

	assert.Equal(t, TemplateLayout{}, TemplateLayoutFromBuildMetadata(nil))
	assert.Equal(t, TemplateLayout{}, TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{}),
		"an unreported layout stays unknown")

	legacy := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new("")})
	require.NotNil(t, legacy.Layout)
	assert.Empty(t, *legacy.Layout, "a reported rootfs-only layout is recorded as such")

	block := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new("block-v1"), WorkspaceSlotCapacityBytes: []int64{10 << 30}})
	require.NotNil(t, block.Layout)
	assert.Equal(t, "block-v1", *block.Layout)
	assert.Equal(t, []int64{10 << 30}, block.WorkspaceSlotCapacityBytes)
}

func TestTemplateLayoutFromBuildMetadata_MaxMemory(t *testing.T) {
	t.Parallel()

	hotplug := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new(""), MaxMemoryMB: new(int32(65536))})
	require.NotNil(t, hotplug.MaxMemoryMB)
	assert.Equal(t, int64(65536), *hotplug.MaxMemoryMB)

	fixed := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new("")})
	assert.Nil(t, fixed.MaxMemoryMB, "no device reported: fixed size")

	unknown := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{MaxMemoryMB: new(int32(65536))})
	assert.Nil(t, unknown.MaxMemoryMB, "without a layout the metadata is from a template-manager that predates both")
}

func TestTemplateLayoutFromBuildMetadata_MaxCPU(t *testing.T) {
	t.Parallel()

	hot := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new(""), MaxCpuCount: new(int32(32))})
	require.NotNil(t, hot.MaxCPUCount)
	assert.Equal(t, int64(32), *hot.MaxCPUCount)

	fixed := TemplateLayoutFromBuildMetadata(&templatemanagergrpc.TemplateBuildMetadata{Layout: new("")})
	assert.Nil(t, fixed.MaxCPUCount)
}
