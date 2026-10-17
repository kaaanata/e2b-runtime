package template_manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
)

func TestTemplateLayoutFromAPI(t *testing.T) {
	t.Parallel()

	none, err := TemplateLayoutFromAPI(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, TemplateLayout{}, none, "no layout fields ask for nothing (rootfs-only or inherited)")

	block, err := TemplateLayoutFromAPI(new(api.BlockV1), &[]int64{10, 20})
	require.NoError(t, err)
	require.NotNil(t, block.Layout)
	assert.Equal(t, "block-v1", *block.Layout)
	assert.Equal(t, []int64{10 << 30, 20 << 30}, block.WorkspaceSlotCapacityBytes)

	_, err = TemplateLayoutFromAPI(new(api.TemplateLayout("block-v2")), nil)
	require.Error(t, err)

	_, err = TemplateLayoutFromAPI(new(api.BlockV1), &[]int64{0})
	require.Error(t, err)

	_, err = TemplateLayoutFromAPI(new(api.BlockV1), &[]int64{1 << 40})
	require.Error(t, err, "a capacity that overflows bytes is refused")
}
