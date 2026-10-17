package outbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	dbmodule "github.com/e2b-dev/infra/packages/db"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/sharedriver"
)

// teardownRecorder stands in for the orchestrator and records the calls the
// worker makes in order.
type teardownRecorder struct {
	mu    sync.Mutex
	calls []string
	// stored is what the sandbox store holds for the team.
	stored []sandbox.Sandbox
	// killLeaves keeps the stored sandboxes through a kill, as one still
	// pausing would be.
	killLeaves bool
}

func (r *teardownRecorder) record(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *teardownRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

func (r *teardownRecorder) KillTeamSandboxes(context.Context, uuid.UUID, sandbox.KillReason) (int, int, error) {
	r.record("kill_sandboxes")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.killLeaves {
		return 0, len(r.stored), nil
	}
	killed := len(r.stored)
	r.stored = nil

	return killed, 0, nil
}

func (r *teardownRecorder) GetSandboxes(context.Context, uuid.UUID, []sandbox.State) ([]sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stored, nil
}

type teardownHarness struct {
	db     *testutils.Database
	worker *rivertest.Worker[outbox.TeardownTeamResources, pgx.Tx]
}

func newTeardownHarness(t *testing.T, db *testutils.Database, recorder *teardownRecorder) teardownHarness {
	t.Helper()

	return newTeardownHarnessWithVolumes(t, db, recorder, nil)
}

func newTeardownHarnessWithVolumes(t *testing.T, db *testutils.Database, recorder *teardownRecorder, volumes *VolumeExecutor) teardownHarness {
	t.Helper()

	return teardownHarness{
		db: db,
		worker: rivertest.NewWorker(t, riverpgxv5.New(db.SqlcClient.Pool()), &river.Config{Schema: dbmodule.RiverSchema}, &teamResourcesTeardownWorker{
			sandboxes: recorder,
			teams:     db.AuthDb,
			volumes:   volumes,
			steps:     sharedriver.NewSteps(nil, nil, sharedriver.StepsConfig{MetricPrefix: "api"}).Job(outbox.TeardownTeamResources{}.Kind()),
		}),
	}
}

func (h teardownHarness) blockedTeam(t *testing.T) uuid.UUID {
	t.Helper()

	teamID := testutils.CreateTestTeam(t, h.db)
	require.NoError(t, h.db.AuthDb.TestsRawSQL(t.Context(), `UPDATE teams SET is_blocked = true WHERE id = $1`, teamID))

	return teamID
}

func (h teardownHarness) work(t *testing.T, teamID uuid.UUID) (*rivertest.WorkResult, error) {
	t.Helper()

	tx, err := h.db.SqlcClient.Pool().Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })

	return h.worker.Work(t.Context(), t, tx, outbox.TeardownTeamResources{TeamID: teamID}, nil)
}

var fullTeardown = []string{
	"kill_sandboxes",
}

func TestTeardownTeamResourcesWorker(t *testing.T) {
	t.Parallel()

	// Each case works its job in its own rolled-back transaction, against its
	// own team, so the cases share one database.
	db := testutils.SetupDatabase(t)

	t.Run("a blocked team has its sandboxes killed", func(t *testing.T) {
		t.Parallel()

		recorder := &teardownRecorder{stored: []sandbox.Sandbox{{SandboxID: "running"}}}
		h := newTeardownHarness(t, db, recorder)

		result, err := h.work(t, h.blockedTeam(t))
		require.NoError(t, err)
		require.Equal(t, river.EventKindJobCompleted, result.EventKind)
		require.Equal(t, fullTeardown, recorder.recorded())
	})

	t.Run("a team whose row is already gone is still torn down", func(t *testing.T) {
		t.Parallel()

		recorder := &teardownRecorder{}
		h := newTeardownHarness(t, db, recorder)

		result, err := h.work(t, uuid.New())
		require.NoError(t, err)
		require.Equal(t, river.EventKindJobCompleted, result.EventKind)
		require.Equal(t, fullTeardown, recorder.recorded())
	})

	t.Run("a team that is not blocked keeps its workloads and the job retries", func(t *testing.T) {
		t.Parallel()

		recorder := &teardownRecorder{stored: []sandbox.Sandbox{{SandboxID: "running"}}}
		h := newTeardownHarness(t, db, recorder)

		result, err := h.work(t, testutils.CreateTestTeam(t, h.db))
		require.ErrorIs(t, err, errTeamNotBlocked)
		require.Equal(t, river.EventKindJobFailed, result.EventKind)
		require.Empty(t, recorder.recorded())
	})

	t.Run("a sandbox left in the store fails the attempt so the job retries", func(t *testing.T) {
		t.Parallel()

		recorder := &teardownRecorder{stored: []sandbox.Sandbox{{SandboxID: "pausing"}}, killLeaves: true}
		h := newTeardownHarness(t, db, recorder)

		result, err := h.work(t, h.blockedTeam(t))
		require.ErrorIs(t, err, errSandboxesLeft)
		require.Equal(t, river.EventKindJobFailed, result.EventKind)
		require.Equal(t, []string{"kill_sandboxes"}, recorder.recorded())
	})
}

func TestTeardownTeamResourcesDeletesBlockVolumes(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	ctx := t.Context()
	q := db.SqlcClient
	recorder := &teardownRecorder{}
	h := newTeardownHarnessWithVolumes(t, db, recorder, NewVolumeExecutor(q, &fakeNodes{client: newFakeBlockVolumes()}))
	teamID := h.blockedTeam(t)

	newVolume := func(name string) queries.Volume {
		vol, err := q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
			ID: uuid.New(), TeamID: teamID, VolumeType: "block", Name: name,
			OwnerNodeID: new("node-a"), CapacityBytes: new(int64(1 << 30)),
		})
		require.NoError(t, err)

		return vol
	}

	// A volume whose create failed is deleted like a ready one (the
	// executor's own tests cover a ready volume's delete).
	ready := newVolume("failed")
	require.NoError(t, q.SetBlockVolumeFailed(ctx, ready.ID))
	creating := newVolume("creating")

	deleteJobs := func(volumeID uuid.UUID) []string {
		rows, err := q.Pool().Query(ctx, `SELECT j.kind FROM river.river_job j JOIN volume_operations o ON o.id::text = j.args->>'operation_id'
			WHERE o.volume_id = $1 AND o.kind = 'delete'`, volumeID)
		require.NoError(t, err)
		kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)

		return kinds
	}

	// A volume still being created holds the teardown back; the others are
	// already deleting.
	result, err := h.work(t, teamID)
	require.ErrorIs(t, err, errBlockVolumesQueued)
	require.Equal(t, river.EventKindJobFailed, result.EventKind)

	got, err := q.GetBlockVolumeForUpdate(ctx, ready.ID)
	require.NoError(t, err)
	require.Equal(t, "deleting", got.Lifecycle)
	require.Equal(t, []string{"volume.delete"}, deleteJobs(ready.ID))

	require.NoError(t, q.SetBlockVolumeFailed(ctx, creating.ID))

	result, err = h.work(t, teamID)
	require.NoError(t, err)
	require.Equal(t, river.EventKindJobCompleted, result.EventKind)

	got, err = q.GetBlockVolumeForUpdate(ctx, creating.ID)
	require.NoError(t, err)
	require.Equal(t, "deleting", got.Lifecycle)
	require.Equal(t, []string{"volume.delete"}, deleteJobs(creating.ID))
	require.Equal(t, []string{"volume.delete"}, deleteJobs(ready.ID), "a retried teardown does not enqueue a second delete")
}

func TestTeardownTeamResourcesRunsThroughTheOutboxClient(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	recorder := &teardownRecorder{stored: []sandbox.Sandbox{{SandboxID: "running"}}}
	outboxRiver, err := New(Dependencies{
		Pool:      db.SqlcClient.Pool(),
		Sandboxes: recorder,
		Teams:     db.AuthDb,
		Config:    Config{MaxWorkers: 1, BacklogInterval: time.Second},
	})
	require.NoError(t, err)

	completed, cancelSubscription := outboxRiver.client.Subscribe(river.EventKindJobCompleted)
	defer cancelSubscription()

	require.NoError(t, outboxRiver.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, outboxRiver.Stop(context.WithoutCancel(t.Context()))) })

	teamID := testutils.CreateTestTeam(t, db)
	require.NoError(t, db.AuthDb.TestsRawSQL(t.Context(), `UPDATE teams SET is_blocked = true WHERE id = $1`, teamID))

	// The enqueuing service inserts through its own client, which knows only
	// the job's arguments.
	enqueuer, err := river.NewClient(riverpgxv5.New(db.SqlcClient.Pool()), &river.Config{Schema: dbmodule.RiverSchema})
	require.NoError(t, err)
	inserted, err := enqueuer.Insert(t.Context(), outbox.TeardownTeamResources{TeamID: teamID}, nil)
	require.NoError(t, err)

	select {
	case event := <-completed:
		require.Equal(t, inserted.Job.ID, event.Job.ID)
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown job did not complete")
	}
	require.Equal(t, fullTeardown, recorder.recorded())
}
