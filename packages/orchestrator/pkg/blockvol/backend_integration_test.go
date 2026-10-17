package blockvol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationRootEnv points at a directory on a real Btrfs filesystem. When
// it is unset every test in this file is skipped. The suite needs root (or
// CAP_SYS_ADMIN) for `btrfs subvolume` and the ext4 tools on PATH.
const integrationRootEnv = "BLOCKVOL_TEST_ROOT"

func newIntegrationService(t *testing.T, wm Watermarks) *Service {
	t.Helper()

	base := os.Getenv(integrationRootEnv)
	if base == "" {
		t.Skipf("%s not set; skipping Btrfs integration test", integrationRootEnv)
	}

	//nolint:usetesting // the root must live on the Btrfs mount named by the env var, not in $TMPDIR
	root, err := os.MkdirTemp(base, "blockvol-"+strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))+"-")
	require.NoError(t, err)

	t.Cleanup(func() {
		// Subvolumes cannot be removed with os.RemoveAll; delete them first.
		// t.Context() is already cancelled when cleanups run.
		ctx := context.Background() //nolint:usetesting // t.Context() is cancelled before cleanups run
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || path == root {
				return nil //nolint:nilerr // best-effort cleanup
			}

			if isSubvolume(path) {
				_ = exec.CommandContext(ctx, "btrfs", "subvolume", "delete", path).Run()

				return filepath.SkipDir
			}

			return nil
		})
		_ = os.RemoveAll(root)
	})

	svc, err := New(Config{Root: root, Watermarks: wm})
	require.NoError(t, err)

	return svc
}

// isSubvolume: a Btrfs subvolume root always has inode number 256.
func isSubvolume(path string) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false
	}

	return st.Ino == 256
}

// requireCleanExt4 runs a read-only forced fsck; exit 0 means no problems.
func requireCleanExt4(t *testing.T, image string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "e2fsck", "-f", "-n", image).CombinedOutput()
	require.NoError(t, err, "e2fsck -fn %s:\n%s", image, out)
}

const integrationCapacity = 256 << 20

// markerOffset is the last MiB of the image: data area of the final block
// group, clear of the primary superblock, group descriptors, inode tables
// (packed into the first groups by flex_bg) and every backup superblock.
const markerOffset = integrationCapacity - 1<<20

func readMarker(t *testing.T, image string, off int64) string {
	t.Helper()

	buf := make([]byte, len("blockvol-marker"))
	f, err := os.Open(image)
	require.NoError(t, err)
	defer f.Close()

	_, err = f.ReadAt(buf, off)
	require.NoError(t, err)

	return string(buf)
}

func readOnlyFlag(t *testing.T, svc *Service, path string) bool {
	t.Helper()

	info, err := svc.tools.subvolumeShow(t.Context(), path)
	require.NoError(t, err)

	return info.ReadOnly
}

func TestIntegrationCreateCloneSealDelete(t *testing.T) {
	t.Parallel()

	svc := newIntegrationService(t, Watermarks{MinDataFree: 64 << 20, MinMetadataFree: 16 << 20})
	ctx := t.Context()

	vol := VolumeRef{Team: NewID(), Volume: NewID()}
	inc0 := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	rev0 := NewID()
	opCreate := NewID()

	const capacity = integrationCapacity

	// --- Create ---------------------------------------------------------
	created, err := svc.Create(ctx, opCreate, inc0, rev0, capacity)
	require.NoError(t, err)

	assert.Equal(t, inc0, created.Incarnation.Ref)
	assert.Equal(t, uint64(capacity), created.Incarnation.CapacityBytes)
	assert.Equal(t, svc.Layout().IncarnationImage(inc0), created.Incarnation.ImagePath)
	assert.NotEmpty(t, created.Incarnation.SubvolumeUUID)
	assert.NotEmpty(t, created.Incarnation.FilesystemUUID)
	assert.True(t, created.Incarnation.ParentRevision.IsZero())
	assert.GreaterOrEqual(t, created.Incarnation.AllocatedBytes, uint64(capacity), "fallocate must reserve the full capacity")
	requireCleanExt4(t, created.Incarnation.ImagePath)

	sb, err := exec.CommandContext(ctx, "tune2fs", "-l", created.Incarnation.ImagePath).Output()
	require.NoError(t, err)
	assert.Contains(t, string(sb), "metadata_csum_seed")
	assert.Contains(t, string(sb), "Block size:               4096")
	assert.Contains(t, string(sb), "Filesystem UUID:          "+created.Incarnation.FilesystemUUID)

	assert.Equal(t, rev0, created.Revision.Ref.Revision)
	assert.Equal(t, inc0.Incarnation, created.Revision.SourceIncarnation)
	assert.Equal(t, opCreate, created.Revision.SourceOperation)
	assert.Equal(t, created.Incarnation.SubvolumeUUID, created.Revision.ParentSubvolumeUUID)
	assert.NotEqual(t, created.Incarnation.SubvolumeUUID, created.Revision.SubvolumeUUID)
	assert.Equal(t, created.Incarnation.FilesystemUUID, created.Revision.FilesystemUUID, "revision 0 is a byte-identical snapshot")
	assert.Empty(t, created.Revision.Consistency, "consistency is the caller's to set")
	assert.True(t, readOnlyFlag(t, svc, svc.Layout().RevisionDir(created.Revision.Ref)))
	assert.False(t, readOnlyFlag(t, svc, svc.Layout().IncarnationDir(inc0)))

	// Both are real subvolumes, not plain directories.
	assert.True(t, isSubvolume(svc.Layout().IncarnationDir(inc0)))
	assert.True(t, isSubvolume(svc.Layout().RevisionDir(created.Revision.Ref)))

	// The staged subvolumes were moved, not copied.
	stagingEntries, err := os.ReadDir(svc.Layout().StagingDir(opCreate))
	require.NoError(t, err)
	names := make([]string, 0, len(stagingEntries))
	for _, e := range stagingEntries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"result.json"}, names)

	// Replay: identical result, no new work.
	replayed, err := svc.Create(ctx, opCreate, inc0, rev0, capacity)
	require.NoError(t, err)
	assert.Equal(t, created, replayed)

	// Same op id with different arguments is a conflict.
	_, err = svc.Create(ctx, opCreate, inc0, rev0, capacity*2)
	require.ErrorIs(t, err, ErrOperationConflict)

	// --- Bind / Seal / Release with a real process --------------------------
	fc := exec.CommandContext(ctx, "sleep", "600")
	require.NoError(t, fc.Start())
	t.Cleanup(func() { _ = fc.Process.Kill(); _, _ = fc.Process.Wait() })

	holder, err := svc.Bind(ctx, BindRequest{Incarnation: inc0, SandboxID: "sbx-1", ExecutionID: "exec-1", FCPid: fc.Process.Pid})
	require.NoError(t, err)
	assert.Equal(t, created.Incarnation.ImagePath, holder.BackingPath)
	assert.NotZero(t, holder.FCStarttime)

	bootID, err := NodeBootID()
	require.NoError(t, err)
	assert.Equal(t, bootID, holder.NodeBootID)

	// Write something through the image so the seal has content to carry.
	f, err := os.OpenFile(holder.BackingPath, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("blockvol-marker"), markerOffset)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())

	// Seal by a non-holder is refused.
	rev1 := NewID()
	opSeal := NewID()
	_, err = svc.Seal(ctx, opSeal, inc0, rev1, "exec-other")
	require.ErrorIs(t, err, ErrHolderMismatch)
	_, err = svc.Seal(ctx, opSeal, inc0, rev1, "")
	require.ErrorIs(t, err, ErrHolderMismatch, "offline seal while bound")

	sealed, err := svc.Seal(ctx, opSeal, inc0, rev1, "exec-1")
	require.NoError(t, err)
	assert.Equal(t, rev1, sealed.Ref.Revision)
	assert.Equal(t, created.Incarnation.SubvolumeUUID, sealed.ParentSubvolumeUUID)
	assert.True(t, readOnlyFlag(t, svc, svc.Layout().RevisionDir(sealed.Ref)))

	assert.Equal(t, "blockvol-marker", readMarker(t, sealed.ImagePath, markerOffset))

	// The revision is immutable: even root cannot open its image for writing.
	_, err = os.OpenFile(sealed.ImagePath, os.O_WRONLY, 0)
	require.ErrorIs(t, err, syscall.EROFS)

	// Replay of the seal returns the recorded revision.
	sealedAgain, err := svc.Seal(ctx, opSeal, inc0, rev1, "exec-1")
	require.NoError(t, err)
	assert.Equal(t, sealed, sealedAgain)

	// Delete while held is refused; release while the process lives is refused.
	require.ErrorIs(t, svc.DeleteIncarnation(ctx, NewID(), inc0), ErrIncarnationHeld)

	var running *StillRunningError
	require.ErrorAs(t, svc.Release(ctx, ReleaseRequest{Incarnation: inc0.Incarnation, ExecutionID: "exec-1"}), &running)

	require.NoError(t, fc.Process.Kill())
	_, _ = fc.Process.Wait()
	require.NoError(t, svc.Release(ctx, ReleaseRequest{Incarnation: inc0.Incarnation, ExecutionID: "exec-1"}))

	// --- Clone -----------------------------------------------------------
	inc1 := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	opClone := NewID()

	cloned, err := svc.Clone(ctx, opClone, sealed.Ref, inc1, CloneOptions{})
	require.NoError(t, err)
	assert.Equal(t, inc1, cloned.Ref)
	assert.Equal(t, rev1, cloned.ParentRevision)
	assert.Equal(t, uint64(capacity), cloned.CapacityBytes)
	assert.NotEqual(t, sealed.FilesystemUUID, cloned.FilesystemUUID, "clone must get a fresh ext4 UUID")
	assert.NotEqual(t, sealed.SubvolumeUUID, cloned.SubvolumeUUID)
	assert.False(t, readOnlyFlag(t, svc, svc.Layout().IncarnationDir(inc1)))

	assert.Equal(t, "blockvol-marker", readMarker(t, cloned.ImagePath, markerOffset), "clone carries the sealed content")
	requireCleanExt4(t, cloned.ImagePath)

	clonedAgain, err := svc.Clone(ctx, opClone, sealed.Ref, inc1, CloneOptions{})
	require.NoError(t, err)
	assert.Equal(t, cloned, clonedAgain, "replay must not re-randomise the UUID")

	_, err = svc.Clone(ctx, NewID(), RevisionRef{VolumeRef: vol, Revision: NewID()}, IncarnationRef{VolumeRef: vol, Incarnation: NewID()}, CloneOptions{})
	require.ErrorIs(t, err, ErrRevisionNotFound)

	// A paired-memory fork keeps the image byte-identical.
	incMem := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	opMem := NewID()
	memClone, err := svc.Clone(ctx, opMem, sealed.Ref, incMem, CloneOptions{PreserveFilesystemUUID: true})
	require.NoError(t, err)
	assert.Equal(t, sealed.FilesystemUUID, memClone.FilesystemUUID)
	requireCleanExt4(t, memClone.ImagePath)
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), incMem))
	require.NoError(t, svc.ForgetOperation(ctx, opMem))

	// --- Reconcile -------------------------------------------------------
	diff, err := svc.Reconcile(ctx, Expected{
		Incarnations: []IncarnationRef{inc0, inc1},
		Revisions:    []RevisionRef{created.Revision.Ref, sealed.Ref},
	})
	require.NoError(t, err)
	assert.Empty(t, diff.MissingIncarnations)
	assert.Empty(t, diff.UnexpectedIncarnations)
	assert.Empty(t, diff.MissingRevisions)
	assert.Empty(t, diff.UnexpectedRevisions)
	assert.Empty(t, diff.UnexpectedHolders)
	assert.Empty(t, diff.Invalid)
	assert.ElementsMatch(t, []ID{opCreate, opSeal, opClone}, diff.StagingOperations, "completed operations keep their result record until the ledger GCs them")

	ghost := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	diff, err = svc.Reconcile(ctx, Expected{Incarnations: []IncarnationRef{inc0, ghost}, Holders: []ID{inc0.Incarnation}})
	require.NoError(t, err)
	assert.Equal(t, []IncarnationRef{ghost}, diff.MissingIncarnations)
	assert.Equal(t, []IncarnationRef{inc1}, diff.UnexpectedIncarnations)
	assert.ElementsMatch(t, []RevisionRef{created.Revision.Ref, sealed.Ref}, diff.UnexpectedRevisions)
	assert.Equal(t, []ID{inc0.Incarnation}, diff.MissingHolders)
	assert.False(t, diff.Clean())

	// --- Delete ----------------------------------------------------------
	require.NoError(t, svc.DeleteRevision(ctx, NewID(), created.Revision.Ref))
	require.NoError(t, svc.DeleteRevision(ctx, NewID(), created.Revision.Ref), "delete is idempotent")

	ok, err := exists(svc.Layout().RevisionDir(created.Revision.Ref))
	require.NoError(t, err)
	assert.False(t, ok)

	// The clone stays readable after its source revision is gone.
	assert.Equal(t, "blockvol-marker", readMarker(t, cloned.ImagePath, markerOffset))

	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), inc0))
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), inc1))
	require.NoError(t, svc.DeleteRevision(ctx, NewID(), sealed.Ref))

	ok, err = exists(svc.Layout().VolumeDir(vol))
	require.NoError(t, err)
	assert.False(t, ok, "an emptied volume directory is pruned")

	for _, op := range []ID{opCreate, opSeal, opClone} {
		require.NoError(t, svc.ForgetOperation(ctx, op))
	}

	diff, err = svc.Reconcile(ctx, Expected{})
	require.NoError(t, err)
	assert.True(t, diff.Clean(), "%+v", diff)
}

func TestIntegrationCreateResumesAfterPartialPublish(t *testing.T) {
	t.Parallel()

	svc := newIntegrationService(t, Watermarks{MinDataFree: 64 << 20, MinMetadataFree: 16 << 20})
	ctx := t.Context()

	vol := VolumeRef{Team: NewID(), Volume: NewID()}
	inc := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	rev := NewID()
	op := NewID()

	// Simulate a crash after the incarnation was published but before the
	// revision snapshot and the result record: build the incarnation by hand
	// through the same tools, leave a half-built staging snapshot behind.
	staged := filepath.Join(svc.Layout().StagingDir(op), incarnationsDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o700))
	require.NoError(t, svc.tools.subvolumeCreate(ctx, staged))
	require.NoError(t, svc.tools.fallocate(ctx, filepath.Join(staged, ImageName), 128<<20))
	require.NoError(t, svc.tools.mkfsExt4(ctx, filepath.Join(staged, ImageName), "1b4e28ba-2fa1-4d3b-9c2e-000000000001"))
	require.NoError(t, publish(staged, svc.Layout().IncarnationDir(inc)))

	res, err := svc.Create(ctx, op, inc, rev, 128<<20)
	require.NoError(t, err)
	assert.Equal(t, "1b4e28ba-2fa1-4d3b-9c2e-000000000001", res.Incarnation.FilesystemUUID, "the published incarnation is kept, not rebuilt")
	assert.True(t, readOnlyFlag(t, svc, svc.Layout().RevisionDir(res.Revision.Ref)))
	requireCleanExt4(t, res.Revision.ImagePath)

	// Crash after the read-only revision snapshot but before the result
	// record: the replay reuses the revision instead of failing on it.
	require.NoError(t, os.Remove(svc.resultPath(op)))

	again, err := svc.Create(ctx, op, inc, rev, 128<<20)
	require.NoError(t, err)
	assert.Equal(t, res.Revision.SubvolumeUUID, again.Revision.SubvolumeUUID)

	require.NoError(t, svc.ForgetOperation(ctx, op))
	require.NoError(t, svc.DeleteRevision(ctx, NewID(), res.Revision.Ref))
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), inc))
}

// TestIntegrationReadOnlySubvolumeCannotChangeParent pins the Btrfs behaviour
// that forces revisions to be snapshotted in place rather than staged.
func TestIntegrationReadOnlySubvolumeCannotChangeParent(t *testing.T) {
	t.Parallel()

	svc := newIntegrationService(t, Watermarks{MinDataFree: 1, MinMetadataFree: 1})
	ctx := t.Context()
	root := svc.Layout().Root()

	src := filepath.Join(root, "src")
	require.NoError(t, svc.tools.subvolumeCreate(ctx, src))

	stagedDir := filepath.Join(root, "a")
	require.NoError(t, os.Mkdir(stagedDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(root, "b"), 0o700))

	ro := filepath.Join(stagedDir, "ro")
	require.NoError(t, svc.tools.subvolumeSnapshot(ctx, src, ro, true))

	err := os.Rename(ro, filepath.Join(root, "b", "ro"))
	require.ErrorIs(t, err, syscall.EROFS)

	require.NoError(t, svc.tools.subvolumeDelete(ctx, ro))
	require.NoError(t, svc.tools.subvolumeDelete(ctx, src))
}

func TestIntegrationCapacityWatermarkRefuses(t *testing.T) {
	t.Parallel()

	// A data watermark larger than any loop image we test on.
	svc := newIntegrationService(t, Watermarks{MinDataFree: 1 << 50, MinMetadataFree: 1})
	ctx := t.Context()

	vol := VolumeRef{Team: NewID(), Volume: NewID()}
	inc := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}

	_, err := svc.Create(ctx, NewID(), inc, NewID(), 64<<20)
	require.ErrorIs(t, err, ErrResourceExhausted)

	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "data", capErr.Pool)

	ok, err := exists(svc.Layout().IncarnationDir(inc))
	require.NoError(t, err)
	assert.False(t, ok, "nothing is materialised when capacity is refused")

	usageOut, err := svc.tools.filesystemUsage(ctx, svc.Layout().Root())
	require.NoError(t, err)
	usage, err := ParseUsage(usageOut)
	require.NoError(t, err)
	assert.Positive(t, usage.DeviceSize)
	assert.Positive(t, usage.DataFree())
}

func TestIntegrationPlaceholder(t *testing.T) {
	t.Parallel()

	svc := newIntegrationService(t, Watermarks{MinDataFree: 1, MinMetadataFree: 1})
	ctx := t.Context()

	const tier = 8 << 30

	p1, err := svc.Placeholder(ctx, tier)
	require.NoError(t, err)

	p2, err := svc.Placeholder(ctx, tier)
	require.NoError(t, err)
	assert.Equal(t, p1, p2)

	size, allocated, err := imageSize(p1)
	require.NoError(t, err)
	assert.Equal(t, uint64(tier), size)
	assert.Less(t, allocated, uint64(1<<20), "placeholder is sparse")

	// Replacing the file with a wrong size is detected rather than silently used.
	require.NoError(t, os.Truncate(p1, tier-1))
	_, err = svc.Placeholder(ctx, tier)
	require.Error(t, err)
}

func TestIntegrationProcLiveness(t *testing.T) {
	t.Parallel()

	if os.Getenv(integrationRootEnv) == "" {
		t.Skipf("%s not set; skipping procfs test", integrationRootEnv)
	}

	live := ProcLiveness{}

	cmd := exec.CommandContext(t.Context(), "sleep", "600")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	st, err := live.Starttime(cmd.Process.Pid)
	require.NoError(t, err)
	assert.Positive(t, st)

	alive, err := live.Alive(cmd.Process.Pid, st)
	require.NoError(t, err)
	assert.True(t, alive)

	alive, err = live.Alive(cmd.Process.Pid, st+1)
	require.NoError(t, err)
	assert.False(t, alive, "a different starttime is a different process")

	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()

	require.Eventually(t, func() bool {
		alive, err := live.Alive(cmd.Process.Pid, st)

		return err == nil && !alive
	}, 5*time.Second, 50*time.Millisecond)

	_, err = live.Starttime(cmd.Process.Pid)
	require.ErrorIs(t, err, os.ErrNotExist)
}
