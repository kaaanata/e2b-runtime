package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/sharedriver"
)

const volumeAttemptTimeout = 10 * time.Minute

// ErrVolumeNodeUnavailable is returned by VolumeNodes when a block volume's
// owner node is not connected or not accepting requests. Nothing was sent, so
// the operation is retried as is.
var ErrVolumeNodeUnavailable = errors.New("block volume owner node is unavailable")

// VolumeNodes resolves the BlockVolumeService of the node owning a volume.
// Block volumes never move: there is no fallback to another node.
type VolumeNodes interface {
	BlockVolumeClient(ctx context.Context, clusterID uuid.UUID, nodeID string) (orchestrator.BlockVolumeServiceClient, error)
}

// VolumeExecutor carries out block volume operations recorded in
// volume_operations. It is the single place that turns an operation into
// RPCs and database effects; River workers and the synchronous fork path both
// call it, and running it twice for one operation is safe: the orchestrator
// replays operations by id and the effects are committed under the
// operation's row lock only while it is still open.
type VolumeExecutor struct {
	db    *sqlcdb.Client
	nodes VolumeNodes
}

func NewVolumeExecutor(db *sqlcdb.Client, nodes VolumeNodes) *VolumeExecutor {
	return &VolumeExecutor{db: db, nodes: nodes}
}

// ErrVolumeOperationFailed wraps the reason an operation ended in "failed".
var ErrVolumeOperationFailed = errors.New("volume operation failed")

// Execute runs one attempt of an operation. It returns nil once the
// operation is finished (succeeded, or already finished before), an error
// wrapping ErrVolumeOperationFailed when it failed definitely, and any other
// error when the attempt should be retried. final marks the last attempt: a
// retryable failure then fails the operation.
func (e *VolumeExecutor) Execute(ctx context.Context, operationID uuid.UUID, final bool) error {
	op, err := e.db.GetVolumeOperationForUpdate(ctx, operationID)
	if err != nil {
		return fmt.Errorf("read volume operation %s: %w", operationID, err)
	}

	if isFinished(op.State) {
		if op.State == outbox.VolumeOperationFailed {
			return fmt.Errorf("%w: %s", ErrVolumeOperationFailed, derefString(op.Error))
		}

		return nil
	}

	var params outbox.VolumeOperationParams
	if err := json.Unmarshal(op.Params, &params); err != nil {
		// A document this binary cannot read will not become readable on retry.
		return e.fail(ctx, op, params, fmt.Errorf("decode params: %w", err))
	}

	client, err := e.nodes.BlockVolumeClient(ctx, params.ClusterID, params.NodeID)
	if err != nil {
		if final {
			return e.fail(ctx, op, params, err)
		}

		return err
	}

	// The last attempt ended without an answer. Read the node's state back
	// before retrying, so the ledger records what the retry is acting on; the
	// retry itself is the same request, which the node replays by id.
	if op.State == outbox.VolumeOperationUnknown {
		e.logReconcile(ctx, client, op, params)
	}

	if err := e.db.MarkVolumeOperationRunning(ctx, op.ID); err != nil {
		return fmt.Errorf("mark volume operation running: %w", err)
	}

	result, effects, err := e.call(ctx, client, op, params)
	if err != nil {
		code := status.Code(err)
		if outcomeUnknown(code) {
			if markErr := e.db.MarkVolumeOperationUnknown(ctx, queries.MarkVolumeOperationUnknownParams{ID: op.ID, Error: new(err.Error())}); markErr != nil {
				logger.L().Error(ctx, "failed to mark volume operation unknown", zap.Error(markErr), zap.Stringer("operation_id", op.ID))
			}
		}

		if final || definiteFailure(op.Kind, code) {
			return e.fail(ctx, op, params, err)
		}

		return fmt.Errorf("volume %s attempt: %w", op.Kind, err)
	}

	return e.complete(ctx, op, params, result, effects)
}

// effectsFunc applies an operation's database effects inside the
// transaction that marks it succeeded.
type effectsFunc func(ctx context.Context, q *sqlcdb.Client) error

func (e *VolumeExecutor) call(ctx context.Context, client orchestrator.BlockVolumeServiceClient, op queries.VolumeOperation, p outbox.VolumeOperationParams) (proto.Message, effectsFunc, error) {
	ref := &orchestrator.BlockVolumeRef{TeamId: p.TeamID.String(), VolumeId: p.VolumeID.String()}

	switch op.Kind {
	case outbox.VolumeOperationCreate:
		if p.Create == nil {
			return nil, nil, status.Error(codes.InvalidArgument, "create parameters missing")
		}

		resp, err := client.CreateBlockVolume(ctx, &orchestrator.CreateBlockVolumeRequest{
			OperationId:   op.ID.String(),
			Volume:        ref,
			IncarnationId: p.Create.IncarnationID.String(),
			RevisionId:    p.Create.RevisionID.String(),
			CapacityBytes: uint64(p.Create.CapacityBytes),
		})
		if err != nil {
			return nil, nil, err
		}

		return resp, func(ctx context.Context, q *sqlcdb.Client) error {
			inc, err := insertIncarnation(ctx, q, op, p, nil, resp.GetIncarnation())
			if err != nil {
				return err
			}

			if err := insertRevision(ctx, q, op, p.VolumeID, inc, nil, resp.GetRevision()); err != nil {
				return err
			}

			return q.SetBlockVolumeReady(ctx, queries.SetBlockVolumeReadyParams{VolumeID: p.VolumeID, HeadIncarnationID: &inc})
		}, nil

	case outbox.VolumeOperationClone:
		c := p.Clone
		if c == nil {
			return nil, nil, status.Error(codes.InvalidArgument, "clone parameters missing")
		}

		resp, err := client.CloneBlockVolume(ctx, &orchestrator.CloneBlockVolumeRequest{
			OperationId:       op.ID.String(),
			Volume:            ref,
			SourceVolume:      &orchestrator.BlockVolumeRef{TeamId: p.TeamID.String(), VolumeId: c.SourceVolumeID.String()},
			SourceRevisionId:  c.SourceRevisionID.String(),
			SourceConsistency: ConsistencyToProto(c.SourceConsistency),
			Mode:              cloneModeToProto(c.Mode),
			IncarnationId:     c.IncarnationID.String(),
		})
		if err != nil {
			return nil, nil, err
		}

		return resp, func(ctx context.Context, q *sqlcdb.Client) error {
			inc, err := insertIncarnation(ctx, q, op, p, &c.SourceRevisionID, resp.GetIncarnation())
			if err != nil {
				return err
			}

			if !c.NewVolume {
				return nil
			}

			return q.SetBlockVolumeReady(ctx, queries.SetBlockVolumeReadyParams{VolumeID: p.VolumeID, HeadIncarnationID: &inc})
		}, nil

	case outbox.VolumeOperationSeal:
		sp := p.Seal
		if sp == nil {
			return nil, nil, status.Error(codes.InvalidArgument, "seal parameters missing")
		}

		resp, err := client.SealBlockVolume(ctx, &orchestrator.SealBlockVolumeRequest{
			OperationId:   op.ID.String(),
			Volume:        ref,
			IncarnationId: sp.IncarnationID.String(),
			RevisionId:    sp.RevisionID.String(),
			ExecutionId:   sp.ExecutionID,
			Consistency:   ConsistencyToProto(sp.Consistency),
		})
		if err != nil {
			return nil, nil, err
		}

		return resp, func(ctx context.Context, q *sqlcdb.Client) error {
			var parent *uuid.UUID

			latest, err := q.GetLatestIncarnationRevision(ctx, sp.IncarnationID)
			switch {
			case err == nil:
				if latest.ID != sp.RevisionID {
					parent = &latest.ID
				}
			case dberrors.IsNotFoundError(err):
			default:
				return fmt.Errorf("read latest revision: %w", err)
			}

			return insertRevision(ctx, q, op, p.VolumeID, sp.IncarnationID, parent, resp.GetRevision())
		}, nil

	case outbox.VolumeOperationDelete:
		if p.Delete.Scoped() {
			return e.deleteIncarnations(ctx, client, op, p, ref)
		}

		incs, revs, err := e.liveImages(ctx, p.VolumeID)
		if err != nil {
			return nil, nil, status.Error(codes.Unavailable, err.Error())
		}

		req := &orchestrator.DeleteBlockVolumeRequest{OperationId: op.ID.String(), Volume: ref}
		for _, r := range revs {
			req.RevisionIds = append(req.RevisionIds, r.ID.String())
		}

		for _, i := range incs {
			req.IncarnationIds = append(req.IncarnationIds, i.ID.String())
		}

		resp, err := client.DeleteBlockVolume(ctx, req)
		if err != nil {
			return nil, nil, err
		}

		return resp, func(ctx context.Context, q *sqlcdb.Client) error {
			for _, r := range revs {
				if err := q.MarkVolumeRevisionDeleted(ctx, queries.MarkVolumeRevisionDeletedParams{ID: r.ID, VolumeID: p.VolumeID}); err != nil {
					return err
				}
			}

			for _, i := range incs {
				if err := q.MarkVolumeIncarnationDeleted(ctx, queries.MarkVolumeIncarnationDeletedParams{ID: i.ID, VolumeID: p.VolumeID}); err != nil {
					return err
				}
			}

			return q.TombstoneBlockVolume(ctx, p.VolumeID)
		}, nil

	case outbox.VolumeOperationReconcile:
		req, err := e.reconcileRequest(ctx, op, p)
		if err != nil {
			return nil, nil, status.Error(codes.Unavailable, err.Error())
		}

		resp, err := client.ReconcileBlockVolume(ctx, req)
		if err != nil {
			return nil, nil, err
		}

		// Reconcile only reports; acting on a difference is an operator
		// decision.
		return resp, func(context.Context, *sqlcdb.Client) error { return nil }, nil

	default:
		return nil, nil, status.Errorf(codes.InvalidArgument, "unknown volume operation kind %q", op.Kind)
	}
}

// deleteIncarnations removes only the incarnations a scoped delete names
// (VolumeDeleteParams), skipping any already gone or still attached: an
// attached incarnation is in use by an execution again and is not an orphan.
// The volume, its revisions and its other incarnations are untouched.
func (e *VolumeExecutor) deleteIncarnations(ctx context.Context, client orchestrator.BlockVolumeServiceClient, op queries.VolumeOperation, p outbox.VolumeOperationParams, ref *orchestrator.BlockVolumeRef) (proto.Message, effectsFunc, error) {
	live, err := e.db.ListLiveVolumeIncarnations(ctx, p.VolumeID)
	if err != nil {
		return nil, nil, status.Error(codes.Unavailable, fmt.Sprintf("list incarnations: %v", err))
	}

	wanted := make(map[uuid.UUID]bool, len(p.Delete.IncarnationIDs))
	for _, id := range p.Delete.IncarnationIDs {
		wanted[id] = true
	}

	req := &orchestrator.DeleteBlockVolumeRequest{OperationId: op.ID.String(), Volume: ref}

	var targets []uuid.UUID

	for _, inc := range live {
		if !wanted[inc.ID] {
			continue
		}

		if _, err := e.db.GetActiveIncarnationWriter(ctx, inc.ID); err == nil {
			continue
		} else if !dberrors.IsNotFoundError(err) {
			return nil, nil, status.Error(codes.Unavailable, fmt.Sprintf("read writer: %v", err))
		}

		targets = append(targets, inc.ID)
		req.IncarnationIds = append(req.IncarnationIds, inc.ID.String())
	}

	if len(targets) == 0 {
		return &orchestrator.DeleteBlockVolumeResponse{}, func(context.Context, *sqlcdb.Client) error { return nil }, nil
	}

	resp, err := client.DeleteBlockVolume(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	return resp, func(ctx context.Context, q *sqlcdb.Client) error {
		for _, id := range targets {
			if err := q.MarkVolumeIncarnationDeleted(ctx, queries.MarkVolumeIncarnationDeletedParams{ID: id, VolumeID: p.VolumeID}); err != nil {
				return err
			}
		}

		return nil
	}, nil
}

func (e *VolumeExecutor) liveImages(ctx context.Context, volumeID uuid.UUID) ([]queries.VolumeIncarnation, []queries.VolumeRevision, error) {
	incs, err := e.db.ListLiveVolumeIncarnations(ctx, volumeID)
	if err != nil {
		return nil, nil, fmt.Errorf("list incarnations: %w", err)
	}

	revs, err := e.db.ListLiveVolumeRevisions(ctx, volumeID)
	if err != nil {
		return nil, nil, fmt.Errorf("list revisions: %w", err)
	}

	return incs, revs, nil
}

func (e *VolumeExecutor) reconcileRequest(ctx context.Context, op queries.VolumeOperation, p outbox.VolumeOperationParams) (*orchestrator.ReconcileBlockVolumeRequest, error) {
	incs, revs, err := e.liveImages(ctx, p.VolumeID)
	if err != nil {
		return nil, err
	}

	req := &orchestrator.ReconcileBlockVolumeRequest{
		OperationId: op.ID.String(),
		Volume:      &orchestrator.BlockVolumeRef{TeamId: p.TeamID.String(), VolumeId: p.VolumeID.String()},
	}

	for _, i := range incs {
		req.IncarnationIds = append(req.IncarnationIds, i.ID.String())

		if _, err := e.db.GetActiveIncarnationWriter(ctx, i.ID); err == nil {
			req.HeldIncarnationIds = append(req.HeldIncarnationIds, i.ID.String())
		} else if !dberrors.IsNotFoundError(err) {
			return nil, fmt.Errorf("read writer: %w", err)
		}
	}

	for _, r := range revs {
		req.RevisionIds = append(req.RevisionIds, r.ID.String())
	}

	return req, nil
}

// logReconcile records what the node holds before an operation with an
// unknown outcome is retried. A failure to read it does not block the retry.
func (e *VolumeExecutor) logReconcile(ctx context.Context, client orchestrator.BlockVolumeServiceClient, op queries.VolumeOperation, p outbox.VolumeOperationParams) {
	req, err := e.reconcileRequest(ctx, op, p)
	if err == nil {
		var resp *orchestrator.ReconcileBlockVolumeResponse

		resp, err = client.ReconcileBlockVolume(ctx, req)
		if err == nil {
			logger.L().Info(ctx, "reconciled block volume before retrying an operation with an unknown outcome",
				zap.Stringer("operation_id", op.ID),
				zap.String("kind", op.Kind),
				zap.Bool("clean", resp.GetClean()),
				zap.Strings("missing_incarnations", resp.GetMissingIncarnationIds()),
				zap.Strings("unexpected_incarnations", resp.GetUnexpectedIncarnationIds()),
				zap.Strings("missing_revisions", resp.GetMissingRevisionIds()),
				zap.Strings("unexpected_revisions", resp.GetUnexpectedRevisionIds()),
			)

			return
		}
	}

	logger.L().Warn(ctx, "could not reconcile block volume before retry", zap.Error(err), zap.Stringer("operation_id", op.ID))
}

// complete commits an operation's effects and its success together, unless
// another run finished it first.
func (e *VolumeExecutor) complete(ctx context.Context, op queries.VolumeOperation, p outbox.VolumeOperationParams, result proto.Message, effects effectsFunc) error {
	raw, err := protojson.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}

	err = e.inTx(ctx, func(ctx context.Context, q *sqlcdb.Client) error {
		locked, err := q.GetVolumeOperationForUpdate(ctx, op.ID)
		if err != nil {
			return err
		}

		if isFinished(locked.State) {
			return nil
		}

		if err := effects(ctx, q); err != nil {
			return fmt.Errorf("apply %s effects: %w", op.Kind, err)
		}

		return q.CompleteVolumeOperation(ctx, queries.CompleteVolumeOperationParams{ID: op.ID, Result: raw})
	})
	if err != nil {
		return err
	}

	e.forget(ctx, op, p)

	return nil
}

// fail records a definite failure and its kind-specific effects.
func (e *VolumeExecutor) fail(ctx context.Context, op queries.VolumeOperation, p outbox.VolumeOperationParams, cause error) error {
	err := e.inTx(ctx, func(ctx context.Context, q *sqlcdb.Client) error {
		locked, err := q.GetVolumeOperationForUpdate(ctx, op.ID)
		if err != nil {
			return err
		}

		if isFinished(locked.State) {
			return nil
		}

		// A volume whose first image never materialised cannot be used; the
		// owner deletes it (the delete has nothing to remove).
		if op.Kind == outbox.VolumeOperationCreate || (op.Kind == outbox.VolumeOperationClone && p.Clone != nil && p.Clone.NewVolume) {
			if err := q.SetBlockVolumeFailed(ctx, p.VolumeID); err != nil {
				return err
			}
		}

		return q.FailVolumeOperation(ctx, queries.FailVolumeOperationParams{ID: op.ID, Error: new(cause.Error())})
	})
	if err != nil {
		return fmt.Errorf("record failure (%w): %w", cause, err)
	}

	if p.NodeID != "" {
		e.forget(ctx, op, p)
	}

	return fmt.Errorf("%w: %w", ErrVolumeOperationFailed, cause)
}

// forget drops the node's record of a committed operation. It is best
// effort: a leftover only shows up in Reconcile's staging list.
func (e *VolumeExecutor) forget(ctx context.Context, op queries.VolumeOperation, p outbox.VolumeOperationParams) {
	if op.Kind == outbox.VolumeOperationReconcile {
		return
	}

	client, err := e.nodes.BlockVolumeClient(ctx, p.ClusterID, p.NodeID)
	if err == nil {
		_, err = client.ForgetBlockVolumeOperation(ctx, &orchestrator.ForgetBlockVolumeOperationRequest{OperationId: op.ID.String()})
	}

	if err != nil {
		logger.L().Warn(ctx, "failed to forget block volume operation on its node", zap.Error(err), zap.Stringer("operation_id", op.ID))
	}
}

func (e *VolumeExecutor) inTx(ctx context.Context, fn func(context.Context, *sqlcdb.Client) error) error {
	q, tx, err := e.db.WithTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(ctx, q); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func insertIncarnation(ctx context.Context, q *sqlcdb.Client, op queries.VolumeOperation, p outbox.VolumeOperationParams, parent *uuid.UUID, inc *orchestrator.BlockVolumeIncarnation) (uuid.UUID, error) {
	id, err := uuid.Parse(inc.GetIncarnationId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("incarnation id: %w", err)
	}

	subvolUUID, err := uuid.Parse(inc.GetSubvolUuid())
	if err != nil {
		return uuid.Nil, fmt.Errorf("incarnation subvolume uuid: %w", err)
	}

	fsUUID, err := uuid.Parse(inc.GetFsUuid())
	if err != nil {
		return uuid.Nil, fmt.Errorf("incarnation filesystem uuid: %w", err)
	}

	_, err = q.InsertVolumeIncarnation(ctx, queries.InsertVolumeIncarnationParams{
		ID:               id,
		VolumeID:         p.VolumeID,
		ParentRevisionID: parent,
		NodeID:           p.NodeID,
		SubvolPath:       inc.GetSubvolPath(),
		SubvolUuid:       subvolUUID,
		FsUuid:           fsUUID,
		CapacityBytes:    int64(inc.GetCapacityBytes()),
		OperationID:      op.ID,
	})
	if err != nil && !dberrors.IsNotFoundError(err) { // not found: already recorded
		return uuid.Nil, fmt.Errorf("record incarnation: %w", err)
	}

	return id, nil
}

func insertRevision(ctx context.Context, q *sqlcdb.Client, op queries.VolumeOperation, volumeID, incarnationID uuid.UUID, parent *uuid.UUID, rev *orchestrator.BlockVolumeRevision) error {
	id, err := uuid.Parse(rev.GetRevisionId())
	if err != nil {
		return fmt.Errorf("revision id: %w", err)
	}

	subvolUUID, err := uuid.Parse(rev.GetSubvolUuid())
	if err != nil {
		return fmt.Errorf("revision subvolume uuid: %w", err)
	}

	fsUUID, err := uuid.Parse(rev.GetFsUuid())
	if err != nil {
		return fmt.Errorf("revision filesystem uuid: %w", err)
	}

	consistency := ConsistencyFromProto(rev.GetConsistency())
	if consistency == "" {
		return fmt.Errorf("revision %s has no consistency", id)
	}

	_, err = q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID:            id,
		VolumeID:      volumeID,
		IncarnationID: incarnationID,
		ParentID:      parent,
		SubvolUuid:    subvolUUID,
		FsUuid:        fsUUID,
		SizeBytes:     int64(rev.GetSizeBytes()),
		Consistency:   consistency,
		OperationID:   op.ID,
	})
	if err != nil && !dberrors.IsNotFoundError(err) {
		return fmt.Errorf("record revision: %w", err)
	}

	return nil
}

func isFinished(state string) bool {
	return state == outbox.VolumeOperationSucceeded || state == outbox.VolumeOperationFailed
}

// outcomeUnknown reports whether an RPC error leaves open whether the node
// acted on the request.
func outcomeUnknown(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Aborted:
		return true
	default:
		return false
	}
}

// definiteFailure reports whether a node's answer means retrying the same
// request cannot succeed. FAILED_PRECONDITION (an incarnation still held) and
// INTERNAL (a tool failed) are retried: holders go away and tools are rerun
// from the operation's staging state.
func definiteFailure(kind string, code codes.Code) bool {
	switch code {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied, codes.Unimplemented, codes.OutOfRange:
		return true
	case codes.ResourceExhausted:
		// A full node is reported to whoever is waiting for a new volume rather
		// than retried for a day; a delete frees space and keeps trying.
		return kind != outbox.VolumeOperationDelete
	default:
		return false
	}
}

// ConsistencyToProto maps a volume_revisions.consistency value to its wire form.
func ConsistencyToProto(c string) orchestrator.BlockVolumeConsistency {
	switch c {
	case "filesystem-quiesced":
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED
	case "paired-memory":
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY
	default:
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_UNSPECIFIED
	}
}

// ConsistencyFromProto is the inverse of ConsistencyToProto; "" for
// unspecified.
func ConsistencyFromProto(c orchestrator.BlockVolumeConsistency) string {
	switch c {
	case orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED:
		return "filesystem-quiesced"
	case orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY:
		return "paired-memory"
	default:
		return ""
	}
}

func cloneModeToProto(mode string) orchestrator.BlockVolumeCloneMode {
	switch mode {
	case outbox.VolumeCloneColdBoot:
		return orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_COLD_BOOT
	case outbox.VolumeCloneMemoryRestore:
		return orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_MEMORY_RESTORE
	default:
		return orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_UNSPECIFIED
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// volumeJobArgs is the River args of one volume job kind.
type volumeJobArgs interface {
	river.JobArgs
	Operation() uuid.UUID
}

// volumeWorker works one volume job kind through the executor. A definite
// failure cancels the job (the operation row already says why); anything else
// is retried with River's backoff until the last attempt, which fails the
// operation.
type volumeWorker[T volumeJobArgs] struct {
	river.WorkerDefaults[T]

	exec  *VolumeExecutor
	steps sharedriver.JobSteps
}

func (w *volumeWorker[T]) Timeout(*river.Job[T]) time.Duration { return volumeAttemptTimeout }

func (w *volumeWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	final := job.Attempt >= job.MaxAttempts

	err := w.steps.StepWith(ctx, "execute", sharedriver.StepOptions{
		Subject: []zap.Field{zap.Stringer("operation_id", job.Args.Operation())},
	}, func(ctx context.Context) error {
		return w.exec.Execute(ctx, job.Args.Operation(), final)
	})
	if errors.Is(err, ErrVolumeOperationFailed) {
		return river.JobCancel(err)
	}

	return err
}

// addVolumeWorkers registers one worker per volume job kind.
func addVolumeWorkers(workers *river.Workers, exec *VolumeExecutor, steps *sharedriver.Steps) {
	river.AddWorker(workers, &volumeWorker[outbox.VolumeCreate]{exec: exec, steps: steps.Job(outbox.VolumeCreate{}.Kind())})
	river.AddWorker(workers, &volumeWorker[outbox.VolumeClone]{exec: exec, steps: steps.Job(outbox.VolumeClone{}.Kind())})
	river.AddWorker(workers, &volumeWorker[outbox.VolumeSeal]{exec: exec, steps: steps.Job(outbox.VolumeSeal{}.Kind())})
	river.AddWorker(workers, &volumeWorker[outbox.VolumeDelete]{exec: exec, steps: steps.Job(outbox.VolumeDelete{}.Kind())})
	river.AddWorker(workers, &volumeWorker[outbox.VolumeReconcile]{exec: exec, steps: steps.Job(outbox.VolumeReconcile{}.Kind())})
}

// DeleteTeamBlockVolumes starts the delete of every block volume of a team
// being torn down: one "delete" operation per volume, keyed by the volume so
// a retried teardown replays it, and its volume.delete job, committed with
// the volume's move to "deleting" in one transaction. The River client comes
// from the running job's context. The volume's snapshots do not keep it: the
// team and its snapshots are going. A volume still being created is left
// for the next attempt (errBlockVolumesQueued), once its create finished.
func (e *VolumeExecutor) DeleteTeamBlockVolumes(ctx context.Context, teamID, clusterID uuid.UUID) error {
	riverClient, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}

	volumes, err := e.db.ListTeamBlockVolumesToDelete(ctx, teamID)
	if err != nil {
		return fmt.Errorf("list team block volumes: %w", err)
	}

	creating := 0

	for _, vol := range volumes {
		if vol.Lifecycle == "creating" {
			creating++

			continue
		}

		if err := e.startTeardownDelete(ctx, riverClient, teamID, clusterID, vol.ID); err != nil {
			return fmt.Errorf("delete block volume %s: %w", vol.ID, err)
		}
	}

	if creating > 0 {
		return fmt.Errorf("%w: %d", errBlockVolumesQueued, creating)
	}

	return nil
}

func (e *VolumeExecutor) startTeardownDelete(ctx context.Context, riverClient *river.Client[pgx.Tx], teamID, clusterID, volumeID uuid.UUID) error {
	q, tx, err := e.db.WithTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	vol, err := q.GetBlockVolumeForUpdate(ctx, volumeID)
	if err != nil {
		return err
	}

	if vol.DeletedAt.Valid || vol.Lifecycle == "deleting" || vol.Lifecycle == "deleted" || vol.OwnerNodeID == nil {
		return nil
	}

	params, err := json.Marshal(outbox.VolumeOperationParams{
		ClusterID: clusterID,
		NodeID:    *vol.OwnerNodeID,
		TeamID:    teamID,
		VolumeID:  vol.ID,
	})
	if err != nil {
		return err
	}

	key := "team-teardown:" + vol.ID.String()

	op, err := q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID:             uuid.New(),
		TeamID:         teamID,
		VolumeID:       &vol.ID,
		Kind:           outbox.VolumeOperationDelete,
		IdempotencyKey: key,
		ParamsHash:     key,
		Params:         params,
	})
	switch {
	case dberrors.IsNotFoundError(err):
		// The key is taken: the operation and its job were committed together
		// before, so only the volume's state is left to (re)apply.
	case err != nil:
		return fmt.Errorf("record delete operation: %w", err)
	default:
		args, err := outbox.VolumeJobArgs(op.Kind, op.ID)
		if err != nil {
			return err
		}

		if _, err := riverClient.InsertTx(ctx, tx, args, nil); err != nil {
			return fmt.Errorf("enqueue %s: %w", args.Kind(), err)
		}
	}

	if _, err := q.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: vol.ID, TeamID: teamID}); err != nil {
		return fmt.Errorf("mark volume deleting: %w", err)
	}

	return tx.Commit(ctx)
}

// EnqueueVolumeOperation inserts the River job working an operation in the
// caller's transaction, so the job exists exactly when the operation row
// does. scheduledAt delays it (the synchronous fork path runs the operation
// itself and leaves the job as a fallback); zero runs it now.
func (r *River) EnqueueVolumeOperation(ctx context.Context, tx pgx.Tx, kind string, operationID uuid.UUID, scheduledAt time.Time) error {
	args, err := outbox.VolumeJobArgs(kind, operationID)
	if err != nil {
		return err
	}

	var opts *river.InsertOpts
	if !scheduledAt.IsZero() {
		o := args.(river.JobArgsWithInsertOpts).InsertOpts()
		o.ScheduledAt = scheduledAt
		opts = &o
	}

	if _, err := r.client.InsertTx(ctx, tx, args, opts); err != nil {
		return fmt.Errorf("enqueue %s: %w", args.Kind(), err)
	}

	return nil
}
