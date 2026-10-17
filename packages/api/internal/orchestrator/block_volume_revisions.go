package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

var errVolumeOfAnotherTeam = errors.New("volume belongs to another team")

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
	// Slot is the template workspace slot the volume occupied; a resume or
	// fork binds it to the same slot.
	Slot int16
}

// sealedRevisionsFromPause converts the workspace revisions a node reported
// for a pause or checkpoint (Sandbox{Pause,Checkpoint}Response field 20).
// The node reports volumes and slots; the mount path comes from the
// sandbox's own record of its block mounts, matched by volume id (a volume is
// mounted at most once). A revision of a volume the sandbox does not mount,
// or with an unparsable id, fails the conversion: recording it would make the
// snapshot unresumable.
func sealedRevisionsFromPause(revisions []*orchestrator.SandboxWorkspaceRevision, mounts []*types.SandboxVolumeMountConfig) ([]SealedWorkspaceRevision, error) {
	if len(revisions) == 0 {
		return nil, nil
	}

	paths := make(map[string]string, len(mounts))
	for _, m := range mounts {
		if m.IsBlock() {
			paths[m.ID] = m.Path
		}
	}

	out := make([]SealedWorkspaceRevision, 0, len(revisions))
	for _, r := range revisions {
		mountPath, ok := paths[r.GetVolumeId()]
		if !ok {
			return nil, fmt.Errorf("revision %s: volume %s is not a block mount of the sandbox", r.GetRevisionId(), r.GetVolumeId())
		}

		if r.GetSlot() > math.MaxInt16 {
			return nil, fmt.Errorf("revision %s: slot %d out of range", r.GetRevisionId(), r.GetSlot())
		}

		if r.GetSizeBytes() > math.MaxInt64 {
			return nil, fmt.Errorf("revision %s: size %d out of range", r.GetRevisionId(), r.GetSizeBytes())
		}

		rev := SealedWorkspaceRevision{
			SizeBytes:   int64(r.GetSizeBytes()),
			Consistency: r.GetConsistency(),
			MountPath:   mountPath,
			Slot:        int16(r.GetSlot()),
		}

		for _, f := range []struct {
			name  string
			value string
			dst   *uuid.UUID
		}{
			{"volume_id", r.GetVolumeId(), &rev.VolumeID},
			{"source_incarnation_id", r.GetSourceIncarnationId(), &rev.IncarnationID},
			{"revision_id", r.GetRevisionId(), &rev.RevisionID},
			{"subvolume_uuid", r.GetSubvolumeUuid(), &rev.SubvolUUID},
			{"fs_uuid", r.GetFsUuid(), &rev.FsUUID},
		} {
			id, err := uuid.Parse(f.value)
			if err != nil {
				return nil, fmt.Errorf("revision %s: %s %q: %w", r.GetRevisionId(), f.name, f.value, err)
			}

			*f.dst = id
		}

		out = append(out, rev)
	}

	return out, nil
}

// recordPauseRevisions records the revisions a pause or checkpoint of sbx
// sealed against the snapshot build. It must succeed before the build is
// marked successful: the snapshot is only resumable with them.
func (o *Orchestrator) recordPauseRevisions(ctx context.Context, sbx sandbox.Sandbox, snapshotBuildID uuid.UUID, revisions []*orchestrator.SandboxWorkspaceRevision) error {
	if len(revisions) == 0 {
		return nil
	}

	sealed, err := sealedRevisionsFromPause(revisions, sbx.VolumeMounts)
	if err != nil {
		return fmt.Errorf("workspace revisions of the pause: %w", err)
	}

	if o.sqlcDB == nil {
		return errors.New("workspace revisions of the pause: no database to record them in")
	}

	if err := RecordSealedWorkspaceRevisions(ctx, o.sqlcDB, sbx.TeamID, snapshotBuildID, sealed); err != nil {
		return fmt.Errorf("record workspace revisions of the pause: %w", err)
	}

	return nil
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
		return errVolumeOfAnotherTeam
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
		Slot:            rev.Slot,
	})
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
