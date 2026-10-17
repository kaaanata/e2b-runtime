package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/queries"
)

// SealedWorkspaceRevision is a block volume revision a node sealed while it
// paused or checkpointed a sandbox (the VM was paused and its drives drained).
type SealedWorkspaceRevision struct {
	VolumeID      uuid.UUID
	IncarnationID uuid.UUID
	RevisionID    uuid.UUID
	SubvolUUID    uuid.UUID
	FsUUID        uuid.UUID
	SizeBytes     int64
	// Consistency is "paired-memory" for a memory snapshot and
	// "filesystem-quiesced" for a filesystem-only one.
	Consistency string
	// MountPath is where the snapshot's sandbox mounted the volume; resume
	// and fork match revisions to mounts by it.
	MountPath string
}

// RecordSealedWorkspaceRevisions records the revisions a pause produced and
// references them from the snapshot build, in one transaction: a snapshot
// whose revisions are not recorded must not be resumable, and a recorded
// revision referenced by a snapshot cannot be deleted. Each revision gets a
// finished "seal" operation (the node already did the work), keyed by the
// revision so a repeat call is a no-op.
func RecordSealedWorkspaceRevisions(ctx context.Context, db *sqlcdb.Client, teamID, snapshotBuildID uuid.UUID, revisions []SealedWorkspaceRevision) error {
	if len(revisions) == 0 {
		return nil
	}

	q, tx, err := db.WithTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	for _, rev := range revisions {
		if err := recordSealedRevision(ctx, q, teamID, snapshotBuildID, rev); err != nil {
			return fmt.Errorf("record revision %s of volume %s: %w", rev.RevisionID, rev.VolumeID, err)
		}
	}

	return tx.Commit(ctx)
}

func recordSealedRevision(ctx context.Context, q *sqlcdb.Client, teamID, snapshotBuildID uuid.UUID, rev SealedWorkspaceRevision) error {
	vol, err := q.GetBlockVolumeForUpdate(ctx, rev.VolumeID)
	if err != nil {
		return fmt.Errorf("lock volume: %w", err)
	}

	if vol.TeamID != teamID {
		return fmt.Errorf("volume belongs to another team")
	}

	params, err := json.Marshal(outbox.VolumeOperationParams{
		TeamID:   teamID,
		VolumeID: rev.VolumeID,
		NodeID:   derefOrEmpty(vol.OwnerNodeID),
		Seal: &outbox.VolumeSealParams{
			IncarnationID: rev.IncarnationID,
			RevisionID:    rev.RevisionID,
			Consistency:   rev.Consistency,
		},
	})
	if err != nil {
		return err
	}

	key := "sealed:" + rev.RevisionID.String()

	op, err := q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID:             uuid.New(),
		TeamID:         teamID,
		VolumeID:       &rev.VolumeID,
		Kind:           outbox.VolumeOperationSeal,
		IdempotencyKey: key,
		ParamsHash:     key,
		Params:         params,
	})
	if dberrors.IsNotFoundError(err) {
		op, err = q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: outbox.VolumeOperationSeal, IdempotencyKey: key})
	}

	if err != nil {
		return fmt.Errorf("record seal operation: %w", err)
	}

	if err := q.CompleteVolumeOperation(ctx, queries.CompleteVolumeOperationParams{ID: op.ID, Result: params}); err != nil {
		return err
	}

	var parent *uuid.UUID

	latest, err := q.GetLatestIncarnationRevision(ctx, rev.IncarnationID)
	switch {
	case err == nil:
		if latest.ID != rev.RevisionID {
			parent = &latest.ID
		}
	case dberrors.IsNotFoundError(err):
	default:
		return fmt.Errorf("read latest revision: %w", err)
	}

	_, err = q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID:            rev.RevisionID,
		VolumeID:      rev.VolumeID,
		IncarnationID: rev.IncarnationID,
		ParentID:      parent,
		SubvolUuid:    rev.SubvolUUID,
		FsUuid:        rev.FsUUID,
		SizeBytes:     rev.SizeBytes,
		Consistency:   rev.Consistency,
		OperationID:   op.ID,
	})
	if err != nil && !dberrors.IsNotFoundError(err) {
		return fmt.Errorf("insert revision: %w", err)
	}

	return q.InsertSnapshotVolumeRef(ctx, queries.InsertSnapshotVolumeRefParams{
		SnapshotBuildID: snapshotBuildID,
		RevisionID:      rev.RevisionID,
		VolumeID:        rev.VolumeID,
		MountPath:       rev.MountPath,
	})
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
