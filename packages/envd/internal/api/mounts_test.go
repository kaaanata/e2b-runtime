package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
)

func TestNFSMountOptions(t *testing.T) {
	t.Parallel()

	t.Run("absent selects the built-in defaults verbatim", func(t *testing.T) {
		t.Parallel()
		got, err := nfsMountOptions(nil)
		require.NoError(t, err)
		assert.Equal(t, nfsDefaultOptions, got)

		got, err = nfsMountOptions(&[]string{})
		require.NoError(t, err)
		assert.Equal(t, nfsDefaultOptions, got)
		// The default must not have grown a nolock: an orchestrator predating
		// mount_options gets exactly the mount it always got.
		assert.NotContains(t, got, "nolock")
	})

	t.Run("whitelisted options pass and nolock is forced", func(t *testing.T) {
		t.Parallel()
		got, err := nfsMountOptions(&[]string{
			"sync", "rsize=1048576", "wsize=1048576", "mountproto=tcp", "mountport=2049",
			"proto=tcp", "port=2049", "nfsvers=3", "noacl", "actimeo=30", "lookupcache=pos", "fg", "hard", "retry=2",
		})
		require.NoError(t, err)
		assert.Equal(t, "sync,rsize=1048576,wsize=1048576,mountproto=tcp,mountport=2049,proto=tcp,port=2049,nfsvers=3,noacl,actimeo=30,lookupcache=pos,fg,hard,retry=2,nolock", got)
	})

	t.Run("an explicit locking choice is not overridden", func(t *testing.T) {
		t.Parallel()
		got, err := nfsMountOptions(&[]string{"sync", "nolock"})
		require.NoError(t, err)
		assert.Equal(t, "sync,nolock", got)

		got, err = nfsMountOptions(&[]string{"sync", "local_lock=all"})
		require.NoError(t, err)
		assert.Equal(t, "sync,local_lock=all", got)
	})

	rejected := map[string][]string{
		"unknown option":            {"sync", "sec=krb5"},
		"server-pointing option":    {"addr=10.0.0.1"},
		"wrong nfs version":         {"nfsvers=4"},
		"wrong proto":               {"proto=udp"},
		"flag with a value":         {"sync=1"},
		"value option without one":  {"rsize"},
		"value smuggling a comma":   {"actimeo=1,nosuid"},
		"value with whitespace":     {"lookupcache=none nosuid"},
		"empty entry":               {""},
		"shell metacharacter value": {"retry=2;reboot"},
	}
	for name, opts := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()
			_, err := nfsMountOptions(&opts)
			require.ErrorIs(t, err, ErrInvalidNFSMountOption)
		})
	}
}

func TestVolumeMountValidate(t *testing.T) {
	t.Parallel()

	uuid := "0f5a4d21-8e3b-4c7a-9d1e-2b3c4d5e6f70"

	cases := map[string]struct {
		mount VolumeMount
		ok    bool
	}{
		"legacy nfs shape":          {VolumeMount{NfsTarget: new("10.0.0.1:/vol"), Path: "/mnt/v"}, true},
		"explicit nfs":              {VolumeMount{SourceType: new(Nfs), NfsTarget: new("10.0.0.1:/vol"), Path: "/mnt/v", MountOptions: &[]string{"sync"}}, true},
		"nfs without target":        {VolumeMount{SourceType: new(Nfs), Path: "/mnt/v"}, false},
		"legacy shape no target":    {VolumeMount{Path: "/mnt/v"}, false},
		"block":                     {VolumeMount{SourceType: new(Block), FsUuid: new(uuid), Path: "/mnt/ws", CapacityBytes: new(int64(1 << 30))}, true},
		"block without uuid":        {VolumeMount{SourceType: new(Block), Path: "/mnt/ws"}, false},
		"block with malformed uuid": {VolumeMount{SourceType: new(Block), FsUuid: new("../../etc"), Path: "/mnt/ws"}, false},
		"block with mount options":  {VolumeMount{SourceType: new(Block), FsUuid: new(uuid), Path: "/mnt/ws", MountOptions: &[]string{"sync"}}, false},
		"relative path":             {VolumeMount{NfsTarget: new("10.0.0.1:/vol"), Path: "mnt/v"}, false},
		"empty path":                {VolumeMount{NfsTarget: new("10.0.0.1:/vol")}, false},
		"unknown source type":       {VolumeMount{SourceType: new(VolumeMountSourceType("iscsi")), Path: "/mnt/v"}, false},
		"block over the rootfs":     {VolumeMount{SourceType: new(Block), FsUuid: new(uuid), Path: "/"}, false},
		"nfs over the rootfs":       {VolumeMount{NfsTarget: new("10.0.0.1:/vol"), Path: "/./"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.mount.validate()
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrVolumeMountInvalid)
			}
		})
	}
}

func TestSetupVolumes_ValidatesBeforeMounting(t *testing.T) {
	t.Parallel()

	api := newAPIWithCgroupManager(cgroups.NewNoopManager())

	// The second entry is malformed; the first must not have been attempted (it
	// would have failed on this machine for the lack of a findmnt/mount, and it
	// must not be in the ledger either).
	err := api.setupVolumes(t.Context(), zerolog.Nop(), new("lc-1"), []VolumeMount{
		{NfsTarget: new("10.0.0.1:/vol"), Path: "/mnt/v"},
		{SourceType: new(Block), Path: "/mnt/ws"},
	})
	require.ErrorIs(t, err, ErrVolumeMountInvalid)
	_, mounted := api.loadMount("/mnt/v")
	assert.False(t, mounted)
}

func TestSetupVolumes_SkipsMountedSameLifecycle(t *testing.T) {
	t.Parallel()

	api := newAPIWithCgroupManager(cgroups.NewNoopManager())
	api.storeMount("/mnt/ws", mountRecord{LifecycleID: "lc-1", SourceType: Block, Device: "/dev/vdb"})
	api.storeMount("/mnt/v", mountRecord{LifecycleID: "lc-1", SourceType: Nfs})

	// Both are already mounted for this lifecycle, so nothing is executed (an
	// attempt would fail here: no findmnt, no block device) and the ledger keeps
	// its records, device included.
	err := api.setupVolumes(t.Context(), zerolog.Nop(), new("lc-1"), []VolumeMount{
		{SourceType: new(Block), FsUuid: new("0f5a4d21-8e3b-4c7a-9d1e-2b3c4d5e6f70"), Path: "/mnt/ws"},
		{NfsTarget: new("10.0.0.1:/vol"), Path: "/mnt/v"},
	})
	require.NoError(t, err)

	record, ok := api.loadMount("/mnt/ws")
	require.True(t, ok)
	assert.Equal(t, mountRecord{LifecycleID: "lc-1", SourceType: Block, Device: "/dev/vdb"}, record)
}

func TestLoadMount_LegacyStringValue(t *testing.T) {
	t.Parallel()

	api := &API{}
	api.mountedPaths.Store("/mnt/v", "lc-9")

	record, ok := api.loadMount("/mnt/v")
	require.True(t, ok)
	assert.Equal(t, mountRecord{LifecycleID: "lc-9", SourceType: Nfs}, record)
	assert.Empty(t, api.blockMountpoints())
}

func TestBlockMountpoints_DeepestFirst(t *testing.T) {
	t.Parallel()

	api := &API{}
	api.storeMount("/data", mountRecord{SourceType: Block, Device: "/dev/vdb"})
	api.storeMount("/data/project/deps", mountRecord{SourceType: Block, Device: "/dev/vdd"})
	api.storeMount("/data/project", mountRecord{SourceType: Block, Device: "/dev/vdc"})
	api.storeMount("/cache", mountRecord{SourceType: Block, Device: "/dev/vde"})
	api.storeMount("/shared", mountRecord{SourceType: Nfs})

	assert.Equal(t, []string{"/data/project/deps", "/data/project", "/data", "/cache"}, api.blockMountpoints())
	assert.Equal(t, []string{"/data/project/deps", "/data/project", "/data", "/cache", "/"}, api.freezeSet())
}

func TestPostVolumesResize_RejectsPathsOutsideTheLedger(t *testing.T) {
	t.Parallel()

	api := newAPIWithCgroupManager(cgroups.NewNoopManager())
	api.storeMount("/mnt/v", mountRecord{LifecycleID: "lc-1", SourceType: Nfs})

	for name, body := range map[string]string{
		"unknown path": `{"path":"/mnt/nothing"}`,
		"nfs mount":    `{"path":"/mnt/v"}`,
		"rootfs":       `{"path":"/"}`,
		"bad json":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/volumes/resize", strings.NewReader(body))
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			api.PostVolumesResize(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestVolumeMountJSON_LegacyShapeDecodes(t *testing.T) {
	t.Parallel()

	// What an orchestrator predating block volumes sends.
	var m VolumeMount
	require.NoError(t, json.NewDecoder(bytes.NewReader([]byte(`{"nfs_target":"10.0.0.1:/vol","path":"/mnt/v"}`))).Decode(&m))
	assert.Equal(t, Nfs, m.sourceType())
	require.NoError(t, m.validate())
	assert.Nil(t, m.MountOptions)
}

func TestSetupVolumes_NormalizesPathsAndRejectsDuplicates(t *testing.T) {
	t.Parallel()

	api := newAPIWithCgroupManager(cgroups.NewNoopManager())
	api.storeMount("/mnt/ws", mountRecord{LifecycleID: "lc-1", SourceType: Block, Device: "/dev/vdb"})

	// A trailing slash names the same ledger entry, so nothing is remounted.
	err := api.setupVolumes(t.Context(), zerolog.Nop(), new("lc-1"), []VolumeMount{
		{SourceType: new(Block), FsUuid: new("0f5a4d21-8e3b-4c7a-9d1e-2b3c4d5e6f70"), Path: "/mnt/ws/"},
	})
	require.NoError(t, err)

	// Two entries for one mountpoint are refused before anything runs.
	err = api.setupVolumes(t.Context(), zerolog.Nop(), new("lc-2"), []VolumeMount{
		{NfsTarget: new("10.0.0.1:/a"), Path: "/mnt/v"},
		{NfsTarget: new("10.0.0.1:/b"), Path: "/mnt/v/"},
	})
	require.ErrorIs(t, err, ErrVolumeMountInvalid)
	record, ok := api.loadMount("/mnt/ws")
	require.True(t, ok)
	assert.Equal(t, "lc-1", record.LifecycleID)
}

func TestMountLevels_ParentsBeforeChildren(t *testing.T) {
	t.Parallel()

	levels := mountLevels([]VolumeMount{
		{Path: "/workspace/shared/cache"},
		{Path: "/workspace"},
		{Path: "/data"},
		{Path: "/workspace/shared"},
	})

	paths := make([][]string, 0, len(levels))
	for _, level := range levels {
		var ps []string
		for _, m := range level {
			ps = append(ps, m.Path)
		}
		paths = append(paths, ps)
	}

	assert.Equal(t, [][]string{{"/workspace", "/data"}, {"/workspace/shared"}, {"/workspace/shared/cache"}}, paths)
}
