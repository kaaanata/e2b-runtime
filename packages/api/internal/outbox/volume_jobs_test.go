package outbox

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	dbmodule "github.com/e2b-dev/infra/packages/db"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/sharedriver"
)

// fakeBlockVolumes stands in for a node's BlockVolumeService. It answers the
// way blockvol does (echoing the requested ids) and can be told to fail the
// next calls of a method.
type fakeBlockVolumes struct {
	orchestrator.BlockVolumeServiceClient

	mu    sync.Mutex
	calls []string
	fail  map[string][]error
	last  map[string]any
}

func newFakeBlockVolumes() *fakeBlockVolumes {
	return &fakeBlockVolumes{fail: map[string][]error{}, last: map[string]any{}}
}

func (f *fakeBlockVolumes) failNext(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = append(f.fail[method], errs...)
}

func (f *fakeBlockVolumes) record(method string, req any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method)
	f.last[method] = req

	if errs := f.fail[method]; len(errs) > 0 {
		f.fail[method] = errs[1:]

		return errs[0]
	}

	return nil
}

func (f *fakeBlockVolumes) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeBlockVolumes) CreateBlockVolume(_ context.Context, req *orchestrator.CreateBlockVolumeRequest, _ ...grpc.CallOption) (*orchestrator.CreateBlockVolumeResponse, error) {
	if err := f.record("create", req); err != nil {
		return nil, err
	}

	fs := uuid.NewString()

	return &orchestrator.CreateBlockVolumeResponse{
		Incarnation: &orchestrator.BlockVolumeIncarnation{
			IncarnationId: req.GetIncarnationId(), SubvolUuid: uuid.NewString(), FsUuid: fs,
			SubvolPath: "teams/t/v/inc/" + req.GetIncarnationId(), CapacityBytes: req.GetCapacityBytes(),
		},
		Revision: &orchestrator.BlockVolumeRevision{
			RevisionId: req.GetRevisionId(), SourceIncarnationId: req.GetIncarnationId(), SourceOperationId: req.GetOperationId(),
			SubvolUuid: uuid.NewString(), FsUuid: fs, SizeBytes: req.GetCapacityBytes(), SealedAt: timestamppb.Now(),
			Consistency: orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED,
		},
	}, nil
}

func (f *fakeBlockVolumes) CloneBlockVolume(_ context.Context, req *orchestrator.CloneBlockVolumeRequest, _ ...grpc.CallOption) (*orchestrator.CloneBlockVolumeResponse, error) {
	if err := f.record("clone", req); err != nil {
		return nil, err
	}

	return &orchestrator.CloneBlockVolumeResponse{Incarnation: &orchestrator.BlockVolumeIncarnation{
		IncarnationId: req.GetIncarnationId(), ParentRevisionId: req.GetSourceRevisionId(),
		SubvolUuid: uuid.NewString(), FsUuid: uuid.NewString(), SubvolPath: "inc", CapacityBytes: 1 << 30,
	}}, nil
}

func (f *fakeBlockVolumes) SealBlockVolume(_ context.Context, req *orchestrator.SealBlockVolumeRequest, _ ...grpc.CallOption) (*orchestrator.SealBlockVolumeResponse, error) {
	if err := f.record("seal", req); err != nil {
		return nil, err
	}

	return &orchestrator.SealBlockVolumeResponse{Revision: &orchestrator.BlockVolumeRevision{
		RevisionId: req.GetRevisionId(), SourceIncarnationId: req.GetIncarnationId(), SubvolUuid: uuid.NewString(),
		FsUuid: uuid.NewString(), SizeBytes: 1 << 30, Consistency: req.GetConsistency(),
	}}, nil
}

func (f *fakeBlockVolumes) DeleteBlockVolume(_ context.Context, req *orchestrator.DeleteBlockVolumeRequest, _ ...grpc.CallOption) (*orchestrator.DeleteBlockVolumeResponse, error) {
	if err := f.record("delete", req); err != nil {
		return nil, err
	}

	return &orchestrator.DeleteBlockVolumeResponse{}, nil
}

func (f *fakeBlockVolumes) ReconcileBlockVolume(_ context.Context, req *orchestrator.ReconcileBlockVolumeRequest, _ ...grpc.CallOption) (*orchestrator.ReconcileBlockVolumeResponse, error) {
	if err := f.record("reconcile", req); err != nil {
		return nil, err
	}

	return &orchestrator.ReconcileBlockVolumeResponse{Clean: true}, nil
}

func (f *fakeBlockVolumes) ForgetBlockVolumeOperation(_ context.Context, req *orchestrator.ForgetBlockVolumeOperationRequest, _ ...grpc.CallOption) (*orchestrator.ForgetBlockVolumeOperationResponse, error) {
	if err := f.record("forget", req); err != nil {
		return nil, err
	}

	return &orchestrator.ForgetBlockVolumeOperationResponse{}, nil
}

// fakeNodes serves one node, which can be switched off.
type fakeNodes struct {
	client *fakeBlockVolumes
	down   bool
}

func (n *fakeNodes) BlockVolumeClient(context.Context, uuid.UUID, string) (orchestrator.BlockVolumeServiceClient, error) {
	if n.down {
		return nil, ErrVolumeNodeUnavailable
	}

	return n.client, nil
}

type volumeFixture struct {
	db     *testutils.Database
	q      *queries.Queries
	exec   *VolumeExecutor
	nodes  *fakeNodes
	node   *fakeBlockVolumes
	teamID uuid.UUID
}

func newVolumeFixture(t *testing.T, db *testutils.Database) volumeFixture {
	t.Helper()

	node := newFakeBlockVolumes()
	nodes := &fakeNodes{client: node}

	return volumeFixture{
		db:     db,
		q:      db.SqlcClient.Queries,
		exec:   NewVolumeExecutor(db.SqlcClient, nodes),
		nodes:  nodes,
		node:   node,
		teamID: testutils.CreateTestTeam(t, db),
	}
}

// operation records an operation for a fresh block volume, the way the API
// does before enqueueing its job.
func (f volumeFixture) operation(t *testing.T, kind string, volume queries.Volume, params outbox.VolumeOperationParams) queries.VolumeOperation {
	t.Helper()

	params.TeamID = f.teamID
	params.VolumeID = volume.ID
	params.NodeID = "node-a"

	raw, err := json.Marshal(params)
	require.NoError(t, err)

	op, err := f.q.CreateVolumeOperation(t.Context(), queries.CreateVolumeOperationParams{
		ID: uuid.New(), TeamID: f.teamID, VolumeID: &volume.ID, Kind: kind,
		IdempotencyKey: uuid.NewString(), ParamsHash: "h", Params: raw,
	})
	require.NoError(t, err)

	return op
}

func (f volumeFixture) volume(t *testing.T) queries.Volume {
	t.Helper()

	vol, err := f.q.CreateBlockVolume(t.Context(), queries.CreateBlockVolumeParams{
		ID: uuid.New(), TeamID: f.teamID, VolumeType: "block", Name: "ws-" + uuid.NewString()[:8],
		OwnerNodeID: new("node-a"), CapacityBytes: new(int64(1 << 30)),
	})
	require.NoError(t, err)

	return vol
}

func (f volumeFixture) createOp(t *testing.T, vol queries.Volume) queries.VolumeOperation {
	t.Helper()

	return f.operation(t, outbox.VolumeOperationCreate, vol, outbox.VolumeOperationParams{
		Create: &outbox.VolumeCreateParams{IncarnationID: uuid.New(), RevisionID: uuid.New(), CapacityBytes: 1 << 30},
	})
}

// ready creates a volume and runs its create operation.
func (f volumeFixture) ready(t *testing.T) (queries.Volume, queries.VolumeRevision) {
	t.Helper()

	vol := f.volume(t)
	require.NoError(t, f.exec.Execute(t.Context(), f.createOp(t, vol).ID, false))

	revs, err := f.q.ListVolumeRevisions(t.Context(), vol.ID)
	require.NoError(t, err)
	require.Len(t, revs, 1)

	vol, err = f.q.GetBlockVolumeForUpdate(t.Context(), vol.ID)
	require.NoError(t, err)

	return vol, revs[0]
}

func (f volumeFixture) state(t *testing.T, op queries.VolumeOperation) queries.VolumeOperation {
	t.Helper()

	got, err := f.q.GetVolumeOperation(t.Context(), queries.GetVolumeOperationParams{ID: op.ID, TeamID: f.teamID})
	require.NoError(t, err)

	return got
}

var errUnavailable = status.Error(codes.Unavailable, "connection reset")

func TestVolumeExecutor(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)

	t.Run("create materialises the head and revision 0 and replays without another RPC", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol := f.volume(t)
		op := f.createOp(t, vol)

		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		done := f.state(t, op)
		assert.Equal(t, outbox.VolumeOperationSucceeded, done.State)
		assert.EqualValues(t, 1, done.Attempts)
		assert.Contains(t, string(done.Result), "incarnation")

		ready, err := f.q.GetBlockVolumeForUpdate(ctx, vol.ID)
		require.NoError(t, err)
		assert.Equal(t, "ready", ready.Lifecycle)
		require.NotNil(t, ready.HeadIncarnationID)

		var params outbox.VolumeOperationParams
		require.NoError(t, json.Unmarshal(op.Params, &params))
		assert.Equal(t, params.Create.IncarnationID, *ready.HeadIncarnationID, "the node got the ids allocated up front")

		rev, err := f.q.GetVolumeRevision(ctx, queries.GetVolumeRevisionParams{ID: params.Create.RevisionID, VolumeID: vol.ID})
		require.NoError(t, err)
		assert.Equal(t, "filesystem-quiesced", rev.Consistency)

		require.NoError(t, f.exec.Execute(ctx, op.ID, false), "a finished operation is a no-op")
		assert.Equal(t, []string{"create", "forget"}, f.node.recorded())
	})

	t.Run("an unknown outcome is reconciled before the same request is retried", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol := f.volume(t)
		op := f.createOp(t, vol)
		f.node.failNext("create", errUnavailable)

		err := f.exec.Execute(ctx, op.ID, false)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrVolumeOperationFailed)
		assert.Equal(t, outbox.VolumeOperationUnknown, f.state(t, op).State)

		first := f.node.last["create"].(*orchestrator.CreateBlockVolumeRequest)

		require.NoError(t, f.exec.Execute(ctx, op.ID, false))
		assert.Equal(t, []string{"create", "reconcile", "create", "forget"}, f.node.recorded())

		retried := f.node.last["create"].(*orchestrator.CreateBlockVolumeRequest)
		assert.Equal(t, first.GetOperationId(), retried.GetOperationId())
		assert.Equal(t, first.GetIncarnationId(), retried.GetIncarnationId(), "a retry is the same request")
		assert.Equal(t, outbox.VolumeOperationSucceeded, f.state(t, op).State)
	})

	t.Run("a definite failure fails the operation and the volume", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol := f.volume(t)
		op := f.createOp(t, vol)
		f.node.failNext("create", status.Error(codes.ResourceExhausted, "node is full"))

		err := f.exec.Execute(ctx, op.ID, false)
		require.ErrorIs(t, err, ErrVolumeOperationFailed)

		failed := f.state(t, op)
		assert.Equal(t, outbox.VolumeOperationFailed, failed.State)
		require.NotNil(t, failed.Error)
		assert.Contains(t, *failed.Error, "node is full")

		got, err := f.q.GetBlockVolumeForUpdate(ctx, vol.ID)
		require.NoError(t, err)
		assert.Equal(t, "failed", got.Lifecycle)

		require.ErrorIs(t, f.exec.Execute(ctx, op.ID, false), ErrVolumeOperationFailed, "a failed operation stays failed")
	})

	t.Run("an unavailable node is retried and fails only on the last attempt", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		op := f.createOp(t, f.volume(t))
		f.nodes.down = true

		err := f.exec.Execute(ctx, op.ID, false)
		require.ErrorIs(t, err, ErrVolumeNodeUnavailable)
		assert.Equal(t, outbox.VolumeOperationPending, f.state(t, op).State, "nothing was sent")

		require.ErrorIs(t, f.exec.Execute(ctx, op.ID, true), ErrVolumeOperationFailed)
		assert.Equal(t, outbox.VolumeOperationFailed, f.state(t, op).State)
		assert.Empty(t, f.node.recorded())
	})

	t.Run("clone into a new volume makes the clone its head", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		source, rev := f.ready(t)
		target := f.volume(t)
		inc := uuid.New()

		op := f.operation(t, outbox.VolumeOperationClone, target, outbox.VolumeOperationParams{Clone: &outbox.VolumeCloneParams{
			SourceVolumeID: source.ID, SourceRevisionID: rev.ID, SourceConsistency: rev.Consistency,
			Mode: outbox.VolumeCloneColdBoot, IncarnationID: inc, CapacityBytes: 1 << 30, NewVolume: true,
		}})
		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		req := f.node.last["clone"].(*orchestrator.CloneBlockVolumeRequest)
		assert.Equal(t, source.ID.String(), req.GetSourceVolume().GetVolumeId())
		assert.Equal(t, target.ID.String(), req.GetVolume().GetVolumeId())
		assert.Equal(t, orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_COLD_BOOT, req.GetMode())
		assert.Equal(t, orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED, req.GetSourceConsistency())

		got, err := f.q.GetBlockVolumeForUpdate(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, "ready", got.Lifecycle)
		assert.Equal(t, inc, *got.HeadIncarnationID)

		cloned, err := f.q.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: inc, VolumeID: target.ID})
		require.NoError(t, err)
		assert.Equal(t, rev.ID, *cloned.ParentRevisionID)
	})

	t.Run("a fork clone adds an incarnation without moving the head", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol, rev := f.ready(t)
		inc := uuid.New()

		op := f.operation(t, outbox.VolumeOperationClone, vol, outbox.VolumeOperationParams{Clone: &outbox.VolumeCloneParams{
			SourceVolumeID: vol.ID, SourceRevisionID: rev.ID, SourceConsistency: "paired-memory",
			Mode: outbox.VolumeCloneMemoryRestore, IncarnationID: inc, CapacityBytes: 1 << 30,
		}})
		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		req := f.node.last["clone"].(*orchestrator.CloneBlockVolumeRequest)
		assert.Equal(t, orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_MEMORY_RESTORE, req.GetMode())

		got, err := f.q.GetBlockVolumeForUpdate(ctx, vol.ID)
		require.NoError(t, err)
		assert.Equal(t, *vol.HeadIncarnationID, *got.HeadIncarnationID)

		_, err = f.q.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: inc, VolumeID: vol.ID})
		require.NoError(t, err)
	})

	t.Run("seal records a revision chained to the previous one", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol, rev0 := f.ready(t)
		revID := uuid.New()

		op := f.operation(t, outbox.VolumeOperationSeal, vol, outbox.VolumeOperationParams{Seal: &outbox.VolumeSealParams{
			IncarnationID: *vol.HeadIncarnationID, RevisionID: revID, Consistency: "filesystem-quiesced",
		}})
		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		rev, err := f.q.GetVolumeRevision(ctx, queries.GetVolumeRevisionParams{ID: revID, VolumeID: vol.ID})
		require.NoError(t, err)
		require.NotNil(t, rev.ParentID)
		assert.Equal(t, rev0.ID, *rev.ParentID)
		assert.Empty(t, f.node.last["seal"].(*orchestrator.SealBlockVolumeRequest).GetExecutionId(), "an offline seal names no holder")
	})

	t.Run("delete removes every image and tombstones the volume", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol, rev := f.ready(t)

		_, err := f.q.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: vol.ID, TeamID: f.teamID})
		require.NoError(t, err)

		op := f.operation(t, outbox.VolumeOperationDelete, vol, outbox.VolumeOperationParams{})

		// A held incarnation is retried, not failed: the holder goes away.
		f.node.failNext("delete", status.Error(codes.FailedPrecondition, "held"))
		err = f.exec.Execute(ctx, op.ID, false)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrVolumeOperationFailed)
		assert.Equal(t, outbox.VolumeOperationRunning, f.state(t, op).State)

		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		req := f.node.last["delete"].(*orchestrator.DeleteBlockVolumeRequest)
		assert.Equal(t, []string{rev.ID.String()}, req.GetRevisionIds())
		assert.Equal(t, []string{vol.HeadIncarnationID.String()}, req.GetIncarnationIds())

		_, err = f.q.GetVolume(ctx, queries.GetVolumeParams{VolumeID: vol.ID, TeamID: f.teamID})
		require.True(t, dberrors.IsNotFoundError(err), "the volume is a tombstone")

		live, err := f.q.ListLiveVolumeRevisions(ctx, vol.ID)
		require.NoError(t, err)
		assert.Empty(t, live)
	})

	t.Run("reconcile reports what the database expects", func(t *testing.T) {
		t.Parallel()

		f := newVolumeFixture(t, db)
		ctx := t.Context()
		vol, rev := f.ready(t)

		_, err := f.q.CreateVolumeAttachment(ctx, queries.CreateVolumeAttachmentParams{
			ID: uuid.New(), VolumeID: vol.ID, IncarnationID: *vol.HeadIncarnationID, SandboxID: "sbx", ExecutionID: "e",
			NodeID: "node-a", Mode: "rw", GuestPath: "/w",
		})
		require.NoError(t, err)

		op := f.operation(t, outbox.VolumeOperationReconcile, vol, outbox.VolumeOperationParams{})
		require.NoError(t, f.exec.Execute(ctx, op.ID, false))

		req := f.node.last["reconcile"].(*orchestrator.ReconcileBlockVolumeRequest)
		assert.Equal(t, []string{rev.ID.String()}, req.GetRevisionIds())
		assert.Equal(t, []string{vol.HeadIncarnationID.String()}, req.GetHeldIncarnationIds())
		var report map[string]any
		require.NoError(t, json.Unmarshal(f.state(t, op).Result, &report))
		assert.Equal(t, true, report["clean"])
		assert.NotContains(t, f.node.recorded()[len(f.node.recorded())-1], "forget", "a reconcile leaves nothing to forget")
	})
}

func TestVolumeWorkerCancelsDefiniteFailures(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	f := newVolumeFixture(t, db)
	worker := rivertest.NewWorker(t, riverpgxv5.New(nil), &river.Config{Schema: dbmodule.RiverSchema}, &volumeWorker[outbox.VolumeCreate]{
		exec:  f.exec,
		steps: sharedriver.NewSteps(nil, nil, sharedriver.StepsConfig{MetricPrefix: "api"}).Job(outbox.VolumeCreate{}.Kind()),
	})

	work := func(op queries.VolumeOperation) (*rivertest.WorkResult, error) {
		tx, err := db.SqlcClient.Pool().Begin(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })

		return worker.Work(t.Context(), t, tx, outbox.VolumeCreate{OperationID: op.ID}, nil)
	}

	ok := f.createOp(t, f.volume(t))
	result, err := work(ok)
	require.NoError(t, err)
	assert.Equal(t, river.EventKindJobCompleted, result.EventKind)

	retry := f.createOp(t, f.volume(t))
	f.node.failNext("create", errUnavailable)
	result, err = work(retry)
	require.Error(t, err)
	assert.Equal(t, river.EventKindJobFailed, result.EventKind, "an unknown outcome is retried by River")

	bad := f.createOp(t, f.volume(t))
	f.node.failNext("create", status.Error(codes.InvalidArgument, "capacity off a tier"))
	result, err = work(bad)
	require.NoError(t, err, "rivertest reports a cancellation through the event, not an error")
	assert.Equal(t, river.EventKindJobCancelled, result.EventKind, "a definite failure is not retried")
	assert.Equal(t, outbox.VolumeOperationFailed, f.state(t, bad).State)
}
