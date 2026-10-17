package blockvol

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func imageBytes(t *testing.T, svc *Service, inc IncarnationRef) int64 {
	t.Helper()

	st, err := os.Stat(svc.Layout().IncarnationImage(inc))
	require.NoError(t, err)

	return st.Size()
}

// TestGrowImageNeedsTheHolder pins the online (paired-memory) grow: only the
// admitted writer may extend the device, the filesystem is left alone (no
// resize2fs, the guest grows it online), equal size is a no-op and a smaller
// size is refused.
func TestGrowImageNeedsTheHolder(t *testing.T) {
	t.Parallel()

	svc, runner, live := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	_, inc := newVolume()

	_, err := svc.Create(ctx, NewID(), inc, NewID(), fakeCapacity)
	require.NoError(t, err)

	require.ErrorIs(t, svc.GrowImage(ctx, inc, "exec-1", 2*fakeCapacity), ErrHolderMismatch, "unbound incarnation")

	live.spawn(100, 7)
	_, err = svc.Bind(ctx, BindRequest{Incarnation: inc, SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	require.ErrorIs(t, svc.GrowImage(ctx, inc, "exec-2", 2*fakeCapacity), ErrHolderMismatch, "someone else's incarnation")
	require.ErrorIs(t, svc.GrowImage(ctx, inc, "", 2*fakeCapacity), ErrHolderMismatch)

	require.NoError(t, svc.GrowImage(ctx, inc, "exec-1", fakeCapacity), "same size is a no-op")
	require.NoError(t, svc.GrowImage(ctx, inc, "exec-1", 2*fakeCapacity))
	assert.Equal(t, int64(2*fakeCapacity), imageBytes(t, svc, inc))
	assert.Zero(t, runner.count("resize2fs"), "the filesystem of a mounted guest volume is never rewritten on the host")

	require.ErrorIs(t, svc.GrowImage(ctx, inc, "exec-1", fakeCapacity), ErrShrinkRefused)
	assert.Equal(t, int64(2*fakeCapacity), imageBytes(t, svc, inc))

	info, err := svc.DescribeIncarnation(ctx, inc)
	require.NoError(t, err)
	assert.Equal(t, uint64(2*fakeCapacity), info.CapacityBytes)
}

// TestGrowFilesystemOfflineRefusesWhileHeld pins the cold-boot grow: it is the
// one place the host runs resize2fs, so it must never touch an image a VM may
// hold.
func TestGrowFilesystemOfflineRefusesWhileHeld(t *testing.T) {
	t.Parallel()

	svc, runner, live := newFakeService(t, smallWatermarks())
	ctx := t.Context()
	_, inc := newVolume()

	_, err := svc.Create(ctx, NewID(), inc, NewID(), fakeCapacity)
	require.NoError(t, err)

	live.spawn(100, 7)
	_, err = svc.Bind(ctx, BindRequest{Incarnation: inc, SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	require.ErrorIs(t, svc.GrowFilesystemOffline(ctx, inc, 2*fakeCapacity), ErrHolderMismatch)
	assert.Equal(t, int64(fakeCapacity), imageBytes(t, svc, inc))
	assert.Zero(t, runner.count("resize2fs"))

	live.kill(100)
	require.NoError(t, svc.Release(ctx, ReleaseRequest{Incarnation: inc.Incarnation, ExecutionID: "exec-1"}))

	require.ErrorIs(t, svc.GrowFilesystemOffline(ctx, inc, fakeCapacity/2), ErrShrinkRefused)

	require.NoError(t, svc.GrowFilesystemOffline(ctx, inc, 2*fakeCapacity))
	assert.Equal(t, int64(2*fakeCapacity), imageBytes(t, svc, inc))
	assert.Equal(t, 1, runner.count("e2fsck"))
	assert.Equal(t, 1, runner.count("resize2fs"))

	_, err = svc.DescribeIncarnation(ctx, IncarnationRef{VolumeRef: inc.VolumeRef, Incarnation: NewID()})
	require.ErrorIs(t, err, ErrIncarnationNotFound)
}
