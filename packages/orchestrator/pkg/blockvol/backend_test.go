package blockvol

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeCapacity = 1 << 20

func newFakeService(t *testing.T, wm Watermarks) (*Service, *fakeRunner, *fakeLiveness) {
	t.Helper()

	usage, err := os.ReadFile("testdata/usage-citadel-loop20g.txt")
	require.NoError(t, err)

	runner := newFakeRunner(usage)
	live := newFakeLiveness()

	svc, err := New(Config{Root: t.TempDir(), Watermarks: wm, Runner: runner, Liveness: live, BootID: bootA})
	require.NoError(t, err)

	return svc, runner, live
}

func smallWatermarks() Watermarks { return Watermarks{MinDataFree: 1 << 20, MinMetadataFree: 1 << 20} }

func newVolume() (VolumeRef, IncarnationRef) {
	vol := VolumeRef{Team: NewID(), Volume: NewID()}

	return vol, IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
}

func TestBackendCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	svc, runner, _ := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	_, inc := newVolume()
	op, rev := NewID(), NewID()

	res, err := svc.Create(ctx, op, inc, rev, fakeCapacity)
	require.NoError(t, err)

	assert.Equal(t, uint64(fakeCapacity), res.Incarnation.CapacityBytes)
	assert.Equal(t, res.Incarnation.FilesystemUUID, res.Revision.FilesystemUUID)
	assert.Equal(t, res.Incarnation.SubvolumeUUID, res.Revision.ParentSubvolumeUUID)
	assert.Equal(t, op, res.Revision.SourceOperation)
	assert.Equal(t, 1, runner.count("mkfs.ext4"))
	assert.Equal(t, 1, runner.count("btrfs subvolume snapshot"))

	again, err := svc.Create(ctx, op, inc, rev, fakeCapacity)
	require.NoError(t, err)
	assert.Equal(t, res, again)
	assert.Equal(t, 1, runner.count("mkfs.ext4"), "replay does no filesystem work")
	assert.Equal(t, 1, runner.count("btrfs subvolume snapshot"))

	_, err = svc.Create(ctx, op, inc, NewID(), fakeCapacity)
	require.ErrorIs(t, err, ErrOperationConflict)

	_, err = svc.Seal(ctx, op, inc, rev, "")
	require.ErrorIs(t, err, ErrOperationConflict, "an operation id is bound to its kind")
}

func TestBackendCreateResumesAfterFailure(t *testing.T) {
	t.Parallel()

	for _, step := range []string{"fallocate", "mkfs.ext4", "btrfs subvolume snapshot", "btrfs subvolume show"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()

			svc, runner, _ := newFakeService(t, smallWatermarks())
			ctx := t.Context()
			_, inc := newVolume()
			op, rev := NewID(), NewID()

			runner.failNext(step)

			_, err := svc.Create(ctx, op, inc, rev, fakeCapacity)
			var cmdErr *CommandError
			require.ErrorAs(t, err, &cmdErr)
			assert.Equal(t, "injected failure", cmdErr.Stderr)

			res, err := svc.Create(ctx, op, inc, rev, fakeCapacity)
			require.NoError(t, err, "retrying the same operation converges")
			assert.Equal(t, fakeCapacity, int(res.Incarnation.CapacityBytes))

			// The fs UUID is derived from the operation, so a rebuild after a
			// pre-publish failure yields the same identity.
			fresh, _, _ := newFakeService(t, smallWatermarks())
			want, err := fresh.Create(ctx, op, inc, rev, fakeCapacity)
			require.NoError(t, err)
			assert.Equal(t, want.Incarnation.FilesystemUUID, res.Incarnation.FilesystemUUID)

			entries, err := os.ReadDir(svc.Layout().StagingDir(op))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, "result.json", entries[0].Name(), "no half-built subvolume is left in staging")
		})
	}
}

func TestBackendCreateRefusedBelowWatermark(t *testing.T) {
	t.Parallel()

	svc, runner, _ := newFakeService(t, Watermarks{MinDataFree: 1 << 50, MinMetadataFree: 1})
	_, inc := newVolume()

	_, err := svc.Create(t.Context(), NewID(), inc, NewID(), fakeCapacity)
	require.ErrorIs(t, err, ErrResourceExhausted)
	assert.Zero(t, runner.count("btrfs subvolume create"), "nothing is materialised")

	ok, err := exists(svc.Layout().IncarnationDir(inc))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestBackendSealRequiresHolderAgreement(t *testing.T) {
	t.Parallel()

	svc, _, live := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	_, inc := newVolume()

	_, err := svc.Create(ctx, NewID(), inc, NewID(), fakeCapacity)
	require.NoError(t, err)

	_, err = svc.Seal(ctx, NewID(), inc, NewID(), "exec-1")
	require.ErrorIs(t, err, ErrHolderMismatch, "claiming to be the holder of an unbound incarnation")

	offline, err := svc.Seal(ctx, NewID(), inc, NewID(), "")
	require.NoError(t, err, "offline seal of an unbound incarnation")
	assert.False(t, offline.Ref.Revision.IsZero())

	live.spawn(100, 5000)
	_, err = svc.Bind(ctx, BindRequest{Incarnation: inc, SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	_, err = svc.Seal(ctx, NewID(), inc, NewID(), "")
	require.ErrorIs(t, err, ErrHolderMismatch, "offline seal while bound")

	_, err = svc.Seal(ctx, NewID(), inc, NewID(), "exec-2")
	require.ErrorIs(t, err, ErrHolderMismatch)

	op := NewID()
	sealed, err := svc.Seal(ctx, op, inc, NewID(), "exec-1")
	require.NoError(t, err)

	live.kill(100)
	require.NoError(t, svc.Release(ctx, ReleaseRequest{Incarnation: inc.Incarnation, ExecutionID: "exec-1"}))

	replay, err := svc.Seal(ctx, op, inc, sealed.Ref.Revision, "exec-1")
	require.NoError(t, err, "a recorded seal replays after the holder is gone")
	assert.Equal(t, sealed, replay)
}

func TestBackendBindRequiresImage(t *testing.T) {
	t.Parallel()

	svc, _, live := newFakeService(t, smallWatermarks())
	_, inc := newVolume()
	live.spawn(100, 1)

	_, err := svc.Bind(t.Context(), BindRequest{Incarnation: inc, SandboxID: "sbx", ExecutionID: "exec", FCPid: 100})
	require.ErrorIs(t, err, ErrIncarnationNotFound)

	_, err = os.Stat(svc.Layout().HolderPath(inc.Incarnation))
	require.ErrorIs(t, err, os.ErrNotExist, "no holder record for a missing image")
}

func TestBackendCloneUUIDHandling(t *testing.T) {
	t.Parallel()

	svc, runner, _ := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	vol, inc := newVolume()

	created, err := svc.Create(ctx, NewID(), inc, NewID(), fakeCapacity)
	require.NoError(t, err)

	rev := created.Revision.Ref

	cold := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	opCold := NewID()

	coldInfo, err := svc.Clone(ctx, opCold, rev, cold, CloneOptions{})
	require.NoError(t, err)
	assert.NotEqual(t, created.Revision.FilesystemUUID, coldInfo.FilesystemUUID)
	assert.Equal(t, rev.Revision, coldInfo.ParentRevision)
	assert.Equal(t, 1, runner.count("tune2fs"))

	replay, err := svc.Clone(ctx, opCold, rev, cold, CloneOptions{})
	require.NoError(t, err)
	assert.Equal(t, coldInfo, replay, "replay does not re-randomise")
	assert.Equal(t, 1, runner.count("tune2fs"))

	_, err = svc.Clone(ctx, opCold, rev, cold, CloneOptions{PreserveFilesystemUUID: true})
	require.ErrorIs(t, err, ErrOperationConflict, "options are part of the recorded request")

	mem := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}

	memInfo, err := svc.Clone(ctx, NewID(), rev, mem, CloneOptions{PreserveFilesystemUUID: true})
	require.NoError(t, err)
	assert.Equal(t, created.Revision.FilesystemUUID, memInfo.FilesystemUUID, "paired-memory clone stays byte-identical")
	assert.Equal(t, 1, runner.count("tune2fs"))

	_, err = svc.Clone(ctx, NewID(), RevisionRef{VolumeRef: vol, Revision: NewID()}, IncarnationRef{VolumeRef: vol, Incarnation: NewID()}, CloneOptions{})
	require.ErrorIs(t, err, ErrRevisionNotFound)
}

func TestBackendDeleteAndReconcile(t *testing.T) {
	t.Parallel()

	svc, _, live := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	vol, inc := newVolume()
	opCreate := NewID()

	created, err := svc.Create(ctx, opCreate, inc, NewID(), fakeCapacity)
	require.NoError(t, err)

	clone := IncarnationRef{VolumeRef: vol, Incarnation: NewID()}
	opClone := NewID()
	_, err = svc.Clone(ctx, opClone, created.Revision.Ref, clone, CloneOptions{})
	require.NoError(t, err)

	live.spawn(7, 70)
	_, err = svc.Bind(ctx, BindRequest{Incarnation: clone, SandboxID: "sbx", ExecutionID: "e", FCPid: 7})
	require.NoError(t, err)

	// Stray entries that do not fit the layout are reported, never removed.
	stray := filepath.Join(svc.Layout().VolumeDir(vol), "inc", "not-a-uuid")
	require.NoError(t, os.Mkdir(stray, 0o700))

	diff, err := svc.Reconcile(ctx, Expected{
		Incarnations: []IncarnationRef{inc},
		Revisions:    []RevisionRef{created.Revision.Ref},
	})
	require.NoError(t, err)
	assert.Equal(t, []IncarnationRef{clone}, diff.UnexpectedIncarnations)
	assert.Len(t, diff.UnexpectedHolders, 1)
	assert.Equal(t, []string{stray}, diff.Invalid)
	assert.ElementsMatch(t, []ID{opCreate, opClone}, diff.StagingOperations)
	assert.False(t, diff.Clean())

	ok, err := exists(stray)
	require.NoError(t, err)
	assert.True(t, ok, "reconcile never deletes")
	require.NoError(t, os.Remove(stray))

	require.ErrorIs(t, svc.DeleteIncarnation(ctx, NewID(), clone), ErrIncarnationHeld)

	live.kill(7)
	require.NoError(t, svc.Release(ctx, ReleaseRequest{Incarnation: clone.Incarnation, ExecutionID: "e"}))
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), clone))
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), clone), "delete is idempotent")
	require.NoError(t, svc.DeleteRevision(ctx, NewID(), created.Revision.Ref))
	require.NoError(t, svc.DeleteIncarnation(ctx, NewID(), inc))

	ok, err = exists(svc.Layout().TeamDir(vol.Team))
	require.NoError(t, err)
	assert.False(t, ok, "an emptied team directory is pruned")

	require.NoError(t, svc.ForgetOperation(ctx, opCreate))
	require.NoError(t, svc.ForgetOperation(ctx, opClone))
	require.NoError(t, svc.ForgetOperation(ctx, opClone), "forget is idempotent")

	diff, err = svc.Reconcile(ctx, Expected{})
	require.NoError(t, err)
	assert.True(t, diff.Clean(), "%+v", diff)
}

func TestBackendForgetRemovesHalfBuiltSubvolume(t *testing.T) {
	t.Parallel()

	svc, runner, _ := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	_, inc := newVolume()
	op := NewID()

	runner.failNext("mkfs.ext4")
	_, err := svc.Create(ctx, op, inc, NewID(), fakeCapacity)
	require.Error(t, err)

	staged := filepath.Join(svc.Layout().StagingDir(op), incarnationsDir)
	_, err = readFakeSubvol(staged)
	require.NoError(t, err, "the failed attempt left a staged subvolume behind")

	before := runner.count("btrfs subvolume delete")
	require.NoError(t, svc.ForgetOperation(ctx, op))
	assert.Equal(t, before+1, runner.count("btrfs subvolume delete"), "staged subvolume is deleted through btrfs, not rm -rf")

	ok, err := exists(svc.Layout().StagingDir(op))
	require.NoError(t, err)
	assert.False(t, ok)
}
