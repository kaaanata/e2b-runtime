package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func TestValidateLayout(t *testing.T) {
	t.Parallel()

	ok := &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 2 << 30}}

	tests := map[string]struct {
		layout  string
		ws      *metadata.WorkspaceLayout
		wantErr bool
	}{
		"legacy":                     {metadata.LayoutLegacy, nil, false},
		"legacy with workspace":      {metadata.LayoutLegacy, ok, true},
		"block-v1":                   {metadata.LayoutBlockV1, ok, false},
		"block-v1 without workspace": {metadata.LayoutBlockV1, nil, true},
		"block-v1 zero slots":        {metadata.LayoutBlockV1, &metadata.WorkspaceLayout{}, true},
		"block-v1 too many slots":    {metadata.LayoutBlockV1, &metadata.WorkspaceLayout{Slots: 5, SlotCapacityBytes: []int64{1, 1, 1, 1, 1}}, true},
		"block-v1 capacity mismatch": {metadata.LayoutBlockV1, &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1}}, true},
		"block-v1 zero capacity":     {metadata.LayoutBlockV1, &metadata.WorkspaceLayout{Slots: 1, SlotCapacityBytes: []int64{0}}, true},
		"unknown layout":             {"block-v9", ok, true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateLayout(tt.layout, tt.ws, 4)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPlaceholders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ws := &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{4 << 20, 64 << 20}}

	paths, err := Placeholders(dir, ws)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(dir, "workspace0.img"),
		filepath.Join(dir, "workspace1.img"),
	}, paths)

	for i, p := range paths {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, ws.SlotCapacityBytes[i], info.Size(), "slot %d has the slot capacity", i)
		assert.True(t, info.Mode().IsRegular())
	}
}

func TestPlaceholders_NilLayout(t *testing.T) {
	t.Parallel()

	paths, err := Placeholders(t.TempDir(), nil)
	require.NoError(t, err)
	assert.Empty(t, paths)
}

func TestPlaceholders_CleansUpOnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Pre-create slot 1 so O_EXCL fails there after slot 0 was created.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace1.img"), nil, 0o600))

	paths, err := Placeholders(dir, &metadata.WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 20, 1 << 20}})
	require.Error(t, err)
	assert.Nil(t, paths)
	assert.NoFileExists(t, filepath.Join(dir, "workspace0.img"), "the slot created before the failure is removed")
	assert.FileExists(t, filepath.Join(dir, "workspace1.img"), "a file this call did not create is left alone")
}
