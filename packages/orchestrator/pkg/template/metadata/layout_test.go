package metadata

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The layout gate is the bridge reader of the block-volume rollout: a reader
// that does not know a layout must refuse the whole Template rather than treat
// the snapshot as the rootfs-only layout it is not. These tests pin both sides —
// legacy and known layouts decode, unknown ones fail — through deserialize(),
// the one path FromFile and FromBuildID share.

func TestDeserialize_LegacyLayoutStillDecodes(t *testing.T) {
	t.Parallel()

	// Exactly what every pre-existing V2 metadata file looks like: no layout field.
	in := `{"version": 2, "template": {"build_id": "b1", "kernel_version": "6.1", "firecracker_version": "1.0"}, "context": {"user": "root"}}`

	got, err := deserialize(bytes.NewReader([]byte(in)))
	require.NoError(t, err)
	assert.Equal(t, LayoutLegacy, got.Layout)
	assert.Nil(t, got.Workspace)
	assert.Empty(t, got.RequiredCapabilities)
	assert.False(t, got.HasWorkspaceSlots())
}

func TestDeserialize_BlockV1Decodes(t *testing.T) {
	t.Parallel()

	in := `{"version": 2, "template": {"build_id": "b1", "kernel_version": "6.1", "firecracker_version": "1.0"},
	        "layout": "block-v1", "workspace": {"slots": 2, "slot_capacity_bytes": [1073741824, 2147483648]},
	        "required_capabilities": ["block-v1"],
	        "workspace_revisions": [{"revision_id": "rev-1", "subvol_uuid": "s-1", "fs_uuid": "f-1", "size_bytes": 1073741824, "consistency": "paired-memory"}]}`

	got, err := deserialize(bytes.NewReader([]byte(in)))
	require.NoError(t, err)
	assert.Equal(t, LayoutBlockV1, got.Layout)
	require.NotNil(t, got.Workspace)
	assert.Equal(t, WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1 << 30, 2 << 30}}, *got.Workspace)
	assert.Equal(t, []string{"block-v1"}, got.RequiredCapabilities)
	assert.Equal(t, []WorkspaceRevisionRef{{
		RevisionID: "rev-1", SubvolUUID: "s-1", FsUUID: "f-1", SizeBytes: 1 << 30, Consistency: "paired-memory",
	}}, got.WorkspaceRevisions)
	assert.True(t, got.HasWorkspaceSlots())
}

func TestDeserialize_UnknownLayoutIsRefused(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input        string
		wantLayout   string
		wantCapabils []string
	}{
		"unknown layout": {
			input:      `{"version": 2, "template": {"build_id": "b1"}, "layout": "block-v9"}`,
			wantLayout: "block-v9",
		},
		"unknown capability": {
			input:        `{"version": 2, "template": {"build_id": "b1"}, "required_capabilities": ["block-v1", "nvme-passthrough"]}`,
			wantCapabils: []string{"nvme-passthrough"},
		},
		"both": {
			input:        `{"version": 2, "template": {"build_id": "b1"}, "layout": "block-v9", "required_capabilities": ["x", "x"]}`,
			wantLayout:   "block-v9",
			wantCapabils: []string{"x"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := deserialize(bytes.NewReader([]byte(tt.input)))
			require.Error(t, err)
			require.ErrorIs(t, err, ErrUnsupportedLayout, "callers gate on the sentinel")

			var unsupported *UnsupportedLayoutError
			require.ErrorAs(t, err, &unsupported)
			assert.Equal(t, tt.wantLayout, unsupported.Layout)
			assert.Equal(t, tt.wantCapabils, unsupported.Capabilities)
		})
	}
}

func TestFromFile_UnknownLayoutIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "metadata.json")
	unknown := Template{Version: CurrentVersion, Template: TemplateMetadata{BuildID: "b1"}, Layout: "block-v9"}
	require.NoError(t, unknown.ToFile(path))

	_, err := FromFile(path)
	require.ErrorIs(t, err, ErrUnsupportedLayout)
}

func TestLayoutSurvivesFileRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "metadata.json")
	base := Template{
		Version:              CurrentVersion,
		Template:             TemplateMetadata{BuildID: "b1"},
		Layout:               LayoutBlockV1,
		Workspace:            &WorkspaceLayout{Slots: 1, SlotCapacityBytes: []int64{1 << 30}},
		RequiredCapabilities: []string{LayoutBlockV1},
		WorkspaceRevisions:   []WorkspaceRevisionRef{{RevisionID: "r", SizeBytes: 1 << 30, Consistency: "paired-memory"}},
	}
	require.NoError(t, base.ToFile(path))

	got, err := FromFile(path)
	require.NoError(t, err)
	assert.Equal(t, base, got)
}

func TestDeprecatedVersionIgnoresLayout(t *testing.T) {
	t.Parallel()

	// A version-1 file is stripped to its version before any field is read, so
	// no V1 writer could ever smuggle a layout past the gate — and none exists.
	got, err := deserialize(bytes.NewReader([]byte(`{"version": 1, "layout": "block-v9"}`)))
	require.NoError(t, err)
	assert.Equal(t, Template{Version: DeprecatedVersion}, got)
}

// Layout, Workspace and RequiredCapabilities are lineage state like CmdlineArgs:
// the device table is fixed when the base snapshot is taken and every later
// snapshot restores it. Every copy-constructor carries them. WorkspaceRevisions
// describe ONE snapshot like FilesystemOnly: only WithPrefetch, which rewrites
// the metadata of the same snapshot, keeps them.
func TestCopyConstructorsCarryLayout(t *testing.T) {
	t.Parallel()

	base := Template{
		Version:              CurrentVersion,
		Template:             TemplateMetadata{BuildID: "build-1"},
		Layout:               LayoutBlockV1,
		Workspace:            &WorkspaceLayout{Slots: 2, SlotCapacityBytes: []int64{1, 2}},
		RequiredCapabilities: []string{LayoutBlockV1},
		WorkspaceRevisions:   []WorkspaceRevisionRef{{RevisionID: "rev-1"}},
	}

	tests := map[string]struct {
		got           Template
		keepRevisions bool
	}{
		"SameVersionTemplate": {got: base.SameVersionTemplate(TemplateMetadata{BuildID: "build-2"})},
		"NewVersionTemplate":  {got: base.NewVersionTemplate(TemplateMetadata{BuildID: "build-2"})},
		"BasedOn":             {got: base.BasedOn(FromTemplate{Alias: "a", BuildID: "build-0"})},
		"WithPrefetch":        {got: base.WithPrefetch(&Prefetch{Memory: &MemoryPrefetchMapping{}}), keepRevisions: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, base.Layout, tt.got.Layout, "%s must carry the layout", name)
			assert.Equal(t, base.Workspace, tt.got.Workspace, "%s must carry the slots", name)
			assert.Equal(t, base.RequiredCapabilities, tt.got.RequiredCapabilities, "%s must carry the capabilities", name)

			if tt.keepRevisions {
				assert.Equal(t, base.WorkspaceRevisions, tt.got.WorkspaceRevisions, "%s describes the same snapshot", name)
			} else {
				assert.Nil(t, tt.got.WorkspaceRevisions, "%s produces a new snapshot that Pause re-stamps", name)
			}
		})
	}
}

func TestUnsupportedLayoutError_Message(t *testing.T) {
	t.Parallel()

	err := error(&UnsupportedLayoutError{Layout: "block-v9", Capabilities: []string{"x"}})
	assert.True(t, errors.Is(err, ErrUnsupportedLayout))
	assert.Contains(t, err.Error(), `layout "block-v9"`)
	assert.Contains(t, err.Error(), "[x]")
}
