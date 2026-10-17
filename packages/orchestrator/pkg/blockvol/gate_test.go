package blockvol

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLiveness is an in-memory process table: pid -> starttime.
type fakeLiveness struct {
	mu    sync.Mutex
	procs map[int]uint64
}

func newFakeLiveness() *fakeLiveness {
	return &fakeLiveness{procs: map[int]uint64{}}
}

func (f *fakeLiveness) spawn(pid int, starttime uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = starttime
}

func (f *fakeLiveness) kill(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, pid)
}

func (f *fakeLiveness) Starttime(pid int) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, ok := f.procs[pid]
	if !ok {
		return 0, os.ErrNotExist
	}

	return st, nil
}

func (f *fakeLiveness) Alive(pid int, starttime uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, ok := f.procs[pid]

	return ok && st == starttime, nil
}

const bootA = "aaaaaaaa-1111-4222-8333-444444444444"

func newTestGate(t *testing.T) (*Gate, *fakeLiveness) {
	t.Helper()

	layout, err := NewLayout(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, layout.EnsureDirs())

	live := newFakeLiveness()
	clock := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)

	return newGate(layout, live, bootA, func() time.Time { return clock }), live
}

func incRef() IncarnationRef {
	return IncarnationRef{
		VolumeRef:   VolumeRef{Team: MustID(teamID), Volume: MustID(volID)},
		Incarnation: MustID(incID),
	}
}

func TestGateBindRecordsHolder(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	h, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	assert.Equal(t, incID, h.Incarnation.String())
	assert.Equal(t, "sbx", h.SandboxID)
	assert.Equal(t, "exec-1", h.ExecutionID)
	assert.Equal(t, bootA, h.NodeBootID)
	assert.Equal(t, 100, h.FCPid)
	assert.Equal(t, uint64(5000), h.FCStarttime)
	assert.Equal(t, g.layout.IncarnationImage(incRef()), h.BackingPath)
	assert.Equal(t, HolderActive, h.State)

	onDisk, err := g.Inspect(h.Incarnation)
	require.NoError(t, err)
	require.NotNil(t, onDisk)
	assert.Equal(t, h, *onDisk)

	// Same execution, same pid: idempotent.
	again, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)
	assert.Equal(t, h, again)
}

func TestGateBindReplayWithRecycledPidIsNotIdempotent(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	// The recorded FC died and an unrelated process reused its pid. A replay
	// naming the same execution and pid must not be mistaken for the original.
	live.kill(100)
	live.spawn(100, 9999)

	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})

	var blocked *BlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, uint64(5000), blocked.Holder.FCStarttime)
}

func TestGateBindRequiresLiveProcess(t *testing.T) {
	t.Parallel()

	g, _ := newTestGate(t)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.Error(t, err)

	h, err := g.Inspect(incRef().Incarnation)
	require.NoError(t, err)
	assert.Nil(t, h, "a failed bind must not leave a record")

	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "", FCPid: 100})
	require.Error(t, err)

	_, err = g.Bind(t.Context(), BindRequest{Incarnation: IncarnationRef{}, SandboxID: "sbx", ExecutionID: "e", FCPid: 100})
	require.ErrorIs(t, err, ErrInvalidID)
}

func TestGateBindRefusedWhileHolderAlive(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)
	live.spawn(200, 6000)

	first, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200})

	var held *HeldByError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, first, held.Holder)

	// Even with reclaim evidence a live holder cannot be taken over.
	_, err = g.Bind(t.Context(), BindRequest{
		Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200,
		Reclaim: &ReclaimEvidence{ExecutionID: "exec-1", FCPid: 100, FCStarttime: 5000, Reason: "test"},
	})
	require.ErrorAs(t, err, &held)

	// Pid reuse: same pid, different starttime is a different process, so the
	// recorded holder is gone.
	live.kill(100)
	live.spawn(100, 7777)

	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200})

	var blocked *BlockedError
	require.ErrorAs(t, err, &blocked)
}

func TestGateDeadHolderBlocksUntilReclaimed(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)
	live.spawn(200, 6000)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	live.kill(100)

	// Without evidence: blocked, and the state is persisted.
	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200})

	var blocked *BlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, HolderBlocked, blocked.Holder.State)
	assert.Equal(t, "exec-1", blocked.Holder.ExecutionID)

	onDisk, err := g.Inspect(incRef().Incarnation)
	require.NoError(t, err)
	require.NotNil(t, onDisk)
	assert.Equal(t, HolderBlocked, onDisk.State)
	assert.NotNil(t, onDisk.BlockedAt)

	// Repeating without evidence stays blocked.
	_, err = g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200})
	require.ErrorAs(t, err, &blocked)

	// Evidence naming the wrong process is rejected.
	_, err = g.Bind(t.Context(), BindRequest{
		Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200,
		Reclaim: &ReclaimEvidence{ExecutionID: "exec-1", FCPid: 100, FCStarttime: 1, Reason: "wrong"},
	})
	require.ErrorIs(t, err, ErrReclaimEvidenceMismatch)

	// Evidence matching the record takes over.
	h, err := g.Bind(t.Context(), BindRequest{
		Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 200,
		Reclaim: &ReclaimEvidence{ExecutionID: "exec-1", FCPid: 100, FCStarttime: 5000, Reason: "fc exited"},
	})
	require.NoError(t, err)
	assert.Equal(t, "exec-2", h.ExecutionID)
	assert.Equal(t, HolderActive, h.State)
	assert.Equal(t, uint64(6000), h.FCStarttime)
}

func TestGateHolderFromPreviousBootIsDead(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	// Reboot: new boot id, and by coincidence a process with the same pid and
	// starttime exists. It is not the holder.
	rebooted := newGate(g.layout, live, "bbbbbbbb-1111-4222-8333-444444444444", g.now)

	_, err = rebooted.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx-2", ExecutionID: "exec-2", FCPid: 100})

	var blocked *BlockedError
	require.ErrorAs(t, err, &blocked)

	// Release after reboot needs no process check beyond the boot id.
	require.NoError(t, rebooted.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation, ExecutionID: "exec-1"}))

	h, err := rebooted.Inspect(incRef().Incarnation)
	require.NoError(t, err)
	assert.Nil(t, h)
}

func TestGateRelease(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	// Wrong execution.
	err = g.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation, ExecutionID: "exec-9"})
	require.ErrorIs(t, err, ErrHolderMismatch)

	// Process still running.
	err = g.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation, ExecutionID: "exec-1"})

	var running *StillRunningError
	require.ErrorAs(t, err, &running)
	assert.Equal(t, 100, running.Holder.FCPid)

	live.kill(100)

	require.NoError(t, g.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation, ExecutionID: "exec-1"}))

	h, err := g.Inspect(incRef().Incarnation)
	require.NoError(t, err)
	assert.Nil(t, h)

	// Releasing a free incarnation is a no-op.
	require.NoError(t, g.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation, ExecutionID: "exec-1"}))

	require.ErrorIs(t, g.Release(t.Context(), ReleaseRequest{}), ErrInvalidID)
}

func TestGateBlockAndList(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.NoError(t, err)

	require.NoError(t, g.Block(t.Context(), incRef().Incarnation, "startup reclaim timed out"))

	holders, err := g.List()
	require.NoError(t, err)
	require.Len(t, holders, 1)
	assert.Equal(t, HolderBlocked, holders[0].State)
	assert.Equal(t, "startup reclaim timed out", holders[0].BlockedReason)

	// Blocking an unknown incarnation is a no-op and does not create a record.
	require.NoError(t, g.Block(t.Context(), MustID(revID), "x"))

	holders, err = g.List()
	require.NoError(t, err)
	assert.Len(t, holders, 1)
}

func TestGateCorruptRecordIsNotFree(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)
	live.spawn(100, 5000)

	require.NoError(t, os.WriteFile(g.layout.HolderPath(incRef().Incarnation), []byte("{not json"), 0o600))

	_, err := g.Bind(t.Context(), BindRequest{Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-1", FCPid: 100})
	require.Error(t, err)

	err = g.Release(t.Context(), ReleaseRequest{Incarnation: incRef().Incarnation})
	require.Error(t, err)
}

func TestGateConcurrentBindersAdmitOne(t *testing.T) {
	t.Parallel()

	g, live := newTestGate(t)

	const n = 16

	for i := range n {
		live.spawn(1000+i, uint64(10+i))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		refused int
	)

	for i := range n {
		wg.Go(func() {
			h, err := g.Bind(t.Context(), BindRequest{
				Incarnation: incRef(), SandboxID: "sbx", ExecutionID: "exec-" + string(rune('a'+i)), FCPid: 1000 + i,
			})

			mu.Lock()
			defer mu.Unlock()

			if err == nil {
				winners = append(winners, h.ExecutionID)

				return
			}

			var held *HeldByError
			require.ErrorAs(t, err, &held)
			refused++
		})
	}

	wg.Wait()

	assert.Len(t, winners, 1)
	assert.Equal(t, n-1, refused)
}
