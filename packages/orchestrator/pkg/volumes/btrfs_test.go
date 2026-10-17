//go:build linux

package volumes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/chrooted"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// btrfsTestRoot is a directory on a Btrfs filesystem the tests may create
// subvolumes under. Set E2B_TEST_BTRFS_ROOT to run these; without it (or without
// root and the btrfs tool) they skip, because a Btrfs filesystem is not
// something a unit test can conjure.
func btrfsTestRoot(tb testing.TB) string {
	tb.Helper()

	root := os.Getenv("E2B_TEST_BTRFS_ROOT")
	if root == "" {
		tb.Skip("E2B_TEST_BTRFS_ROOT not set")
	}
	if os.Geteuid() != 0 {
		tb.Skip("Test requires root privileges")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		tb.Skip("btrfs tool not installed")
	}

	onBtrfs, err := isOnBtrfs(root)
	require.NoError(tb, err)
	if !onBtrfs {
		tb.Skipf("%s is not on Btrfs", root)
	}

	// Not tb.TempDir: the directory has to be on the Btrfs filesystem under root.
	dir, err := os.MkdirTemp(root, "volumes-test-") //nolint:usetesting // must live under the Btrfs root

	require.NoError(tb, err)
	tb.Cleanup(func() {
		// Subvolumes left behind by a failed test cannot be RemoveAll'd; delete
		// them first so the tree can go.
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = deleteVolumePath(tb.Context(), filepath.Join(dir, e.Name()))
		}
		_ = os.RemoveAll(dir)
	})

	return dir
}

func newBtrfsService(tb testing.TB, root string, limit int64) (*Service, string, *orchestrator.VolumeInfo) {
	tb.Helper()

	config := cfg.Config{
		PersistentVolumeMounts:      map[string]string{volumeType: root},
		VolumeBtrfsSubvolumes:       true,
		VolumeBtrfsQgroupLimitBytes: limit,
	}
	s := New(config, chrooted.NewBuilder(config))
	info := &orchestrator.VolumeInfo{VolumeType: volumeType, TeamId: uuid.New().String(), VolumeId: uuid.New().String()}
	path, err := s.getVolumeRootPath(tb.Context(), info)
	require.NoError(tb, err)

	return s, path, info
}

func TestBtrfsVolume_CreateIsASubvolumeAndDeleteRemovesIt(t *testing.T) {
	t.Parallel()

	root := btrfsTestRoot(t)
	s, path, info := newBtrfsService(t, root, 0)

	_, err := s.CreateVolume(t.Context(), &orchestrator.CreateVolumeRequest{Volume: info})
	require.NoError(t, err)

	sub, err := isBtrfsSubvolume(path)
	require.NoError(t, err)
	assert.True(t, sub, "%s must be a subvolume, not a directory", path)

	parentSub, err := isBtrfsSubvolume(filepath.Dir(path))
	require.NoError(t, err)
	assert.False(t, parentSub, "the team directory stays a plain directory")

	// Idempotent create.
	_, err = s.CreateVolume(t.Context(), &orchestrator.CreateVolumeRequest{Volume: info})
	require.NoError(t, err)

	// Content inside goes with the subvolume.
	require.NoError(t, os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o600))

	_, err = s.DeleteVolume(t.Context(), &orchestrator.DeleteVolumeRequest{Volume: info})
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Deleting again is not an error, as RemoveAll is not.
	_, err = s.DeleteVolume(t.Context(), &orchestrator.DeleteVolumeRequest{Volume: info})
	require.NoError(t, err)
}

func TestBtrfsVolume_QgroupLimitIsApplied(t *testing.T) {
	t.Parallel()

	root := btrfsTestRoot(t)

	// The limit needs quotas on; enable them for this filesystem if they are not
	// (a test-only decision, on a loop-backed test filesystem).
	out, err := exec.CommandContext(t.Context(), "btrfs", "quota", "enable", root).CombinedOutput()
	require.NoError(t, err, string(out))

	const limit = int64(64 << 20)
	s, path, info := newBtrfsService(t, root, limit)

	_, err = s.CreateVolume(t.Context(), &orchestrator.CreateVolumeRequest{Volume: info})
	require.NoError(t, err)

	out, err = exec.CommandContext(t.Context(), "btrfs", "qgroup", "show", "--raw", "-r", "-e", "-f", path).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "67108864", "qgroup show must list the 64 MiB referenced limit")

	// Writing past the limit fails: the limit is enforced, not merely recorded.
	f, err := os.Create(filepath.Join(path, "big"))
	require.NoError(t, err)
	chunk := make([]byte, 1<<20)
	var written int64
	for range 128 {
		n, werr := f.Write(chunk)
		written += int64(n)
		if werr != nil {
			break
		}
		if err := f.Sync(); err != nil {
			break
		}
	}
	_ = f.Close()
	assert.Less(t, written, int64(128<<20), "the qgroup limit must stop the write before 128 MiB")

	_, err = s.DeleteVolume(t.Context(), &orchestrator.DeleteVolumeRequest{Volume: info})
	require.NoError(t, err)
}

func TestBtrfsVolume_RefusesNonBtrfsRoot(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	tmp := t.TempDir()
	onBtrfs, err := isOnBtrfs(tmp)
	require.NoError(t, err)
	if onBtrfs {
		t.Skip("TempDir is on Btrfs; nothing to refuse")
	}

	s, _, info := newBtrfsService(t, tmp, 0)
	_, err = s.CreateVolume(t.Context(), &orchestrator.CreateVolumeRequest{Volume: info})
	require.ErrorIs(t, err, ErrNotBtrfs)
}

func TestIsBtrfsSubvolume_PlainDirectoryAndMissingPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	sub, err := isBtrfsSubvolume(tmp)
	require.NoError(t, err)
	assert.False(t, sub)

	sub, err = isBtrfsSubvolume(filepath.Join(tmp, "missing"))
	require.NoError(t, err)
	assert.False(t, sub)
}

// TestVolume_DefaultLayoutUnchanged pins the default: without the flag a volume
// is a plain directory, and the service never shells out to btrfs.
//
//nolint:paralleltest // swaps the package-level btrfsRunner
func TestVolume_DefaultLayoutUnchanged(t *testing.T) {
	s, rootPath, info := setupTestService(t)

	calls := 0
	prev := btrfsRunner
	btrfsRunner = func(_ context.Context, _ ...string) ([]byte, error) {
		calls++

		return nil, nil
	}
	t.Cleanup(func() { btrfsRunner = prev })

	_, err := s.CreateVolume(t.Context(), &orchestrator.CreateVolumeRequest{Volume: info})
	require.NoError(t, err)
	sub, err := isBtrfsSubvolume(rootPath)
	require.NoError(t, err)
	assert.False(t, sub)

	_, err = s.DeleteVolume(t.Context(), &orchestrator.DeleteVolumeRequest{Volume: info})
	require.NoError(t, err)
	assert.Equal(t, 0, calls)
}
