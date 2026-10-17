package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/outbox"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	dboutbox "github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// maxBlockVolumeMounts is the number of workspace slots a block-v1 template
// reserves (packages/orchestrator/pkg/sandbox/fc); a sandbox cannot mount
// more block volumes than that.
const maxBlockVolumeMounts = 4

// forkCloneFallbackDelay is how long the River job backing an inline fork
// clone waits before it runs: it only matters when the request died before
// recording the outcome.
const forkCloneFallbackDelay = 2 * time.Minute

const attachmentModeRW = "rw"

// blockMountBinding is how one block mount is bound at start.
type blockMountBinding int

const (
	// bindHead attaches the volume's head incarnation (a new sandbox).
	bindHead blockMountBinding = iota
	// bindExact attaches the incarnation a paused execution sealed, if no
	// other execution wrote to it since (a resume).
	bindExact
	// bindClone clones the snapshot's revision into a new incarnation of the
	// volume and attaches that (a fork).
	bindClone
)

type blockMountPlan struct {
	// mount is the sandbox's mount (name, path) of the volume.
	mount   *orchestrator.SandboxVolumeMount
	volume  queries.Volume
	binding blockMountBinding
	// slot is the template workspace slot the volume is bound to: its
	// position among the sandbox's block mounts for a new sandbox, the slot
	// the snapshot recorded for a resume or fork (a memory restore must see
	// every volume in the slot it was sealed in).
	slot int
	// revision is the snapshot's revision (bindExact, bindClone).
	revision queries.VolumeRevision
	// cloneMode is the dboutbox clone mode (bindClone).
	cloneMode string
}

// blockVolumeAttacher implements orchestrator.VolumeAttacher.
type blockVolumeAttacher struct {
	a         *APIStore
	teamID    uuid.UUID
	clusterID uuid.UUID
	ownerNode string
	plans     []blockMountPlan
	// clones are the incarnations Attach cloned for a fork (volume ->
	// incarnation), deleted again by Release.
	clones map[uuid.UUID]uuid.UUID
}

type attachConflictError struct{ msg string }

func (e attachConflictError) Error() string { return e.msg }

// Attach records one RW attachment per block mount, in one transaction that
// locks the volumes (in id order) against deletion and against each other,
// and returns the workspace slot bindings the node needs, in slot order.
// A fork's clones run first, synchronously, through the operation ledger;
// if the start does not get as far as its attachments, the clones are
// deleted again (see discardForkClones).
func (b *blockVolumeAttacher) Attach(ctx context.Context, clusterID uuid.UUID, sandboxID, executionID string) ([]*orchestrator.SandboxWorkspaceSlot, *api.APIError) {
	b.clusterID = clusterID

	clones := map[uuid.UUID]uuid.UUID{} // volume -> cloned incarnation

	for _, p := range b.plans {
		if p.binding != bindClone {
			continue
		}

		inc, apiErr := b.clone(ctx, sandboxID, p)
		if apiErr != nil {
			b.discardForkClones(context.WithoutCancel(ctx), sandboxID, clones)

			return nil, apiErr
		}

		clones[p.volume.ID] = inc
	}

	b.clones = clones

	plans := slices.Clone(b.plans)
	slices.SortFunc(plans, func(x, y blockMountPlan) int { return strings.Compare(x.volume.ID.String(), y.volume.ID.String()) })

	bound := make(map[uuid.UUID]*orchestrator.SandboxWorkspaceSlot, len(plans))

	err := b.a.inTx(ctx, func(q *sqlcdb.Client, _ pgx.Tx) error {
		for _, p := range plans {
			slot, err := b.attachOne(ctx, q, sandboxID, executionID, p, clones[p.volume.ID])
			if err != nil {
				return err
			}

			bound[p.volume.ID] = slot
		}

		return nil
	})

	var conflict attachConflictError

	switch {
	case errors.As(err, &conflict):
		b.discardForkClones(context.WithoutCancel(ctx), sandboxID, clones)

		return nil, &api.APIError{Code: http.StatusConflict, ClientMsg: conflict.msg, Err: err}
	case err != nil:
		b.discardForkClones(context.WithoutCancel(ctx), sandboxID, clones)

		return nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when attaching block volumes", Err: err}
	}

	slots := make([]*orchestrator.SandboxWorkspaceSlot, 0, len(b.plans))
	for _, p := range b.plans {
		slots = append(slots, bound[p.volume.ID])
	}

	slices.SortFunc(slots, func(x, y *orchestrator.SandboxWorkspaceSlot) int { return int(x.GetSlot()) - int(y.GetSlot()) })

	return slots, nil
}

func (b *blockVolumeAttacher) attachOne(ctx context.Context, q *sqlcdb.Client, sandboxID, executionID string, p blockMountPlan, cloned uuid.UUID) (*orchestrator.SandboxWorkspaceSlot, error) {
	if p.slot < 0 || p.slot >= maxBlockVolumeMounts {
		return nil, fmt.Errorf("volume %s: workspace slot %d outside 0..%d", p.volume.ID, p.slot, maxBlockVolumeMounts-1)
	}

	vol, err := q.GetBlockVolumeForUpdate(ctx, p.volume.ID)
	if err != nil {
		return nil, fmt.Errorf("lock volume %s: %w", p.volume.ID, err)
	}

	if vol.Lifecycle != volumeLifecycleReady || vol.DeletedAt.Valid {
		return nil, attachConflictError{fmt.Sprintf("Volume '%s' is %s", vol.Name, vol.Lifecycle)}
	}

	var incarnationID uuid.UUID

	switch p.binding {
	case bindHead:
		if vol.HeadIncarnationID == nil {
			return nil, attachConflictError{fmt.Sprintf("Volume '%s' has no image yet", vol.Name)}
		}

		incarnationID = *vol.HeadIncarnationID
	case bindExact:
		incarnationID = p.revision.IncarnationID

		latest, err := q.GetLatestIncarnationRevision(ctx, incarnationID)
		if err != nil && !dberrors.IsNotFoundError(err) {
			return nil, fmt.Errorf("read latest revision: %w", err)
		}

		advanced, err := q.IncarnationAttachedSince(ctx, queries.IncarnationAttachedSinceParams{
			IncarnationID: incarnationID,
			Since:         p.revision.CreatedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("read attachments: %w", err)
		}

		if advanced || latest.ID != p.revision.ID {
			return nil, attachConflictError{fmt.Sprintf(
				"Volume '%s' was used by another sandbox after this snapshot was taken; fork the snapshot instead of resuming it", vol.Name)}
		}
	case bindClone:
		incarnationID = cloned
	}

	inc, err := q.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: incarnationID, VolumeID: vol.ID})
	if err != nil {
		return nil, fmt.Errorf("read incarnation %s: %w", incarnationID, err)
	}

	if inc.State != volumeLifecycleReady {
		return nil, attachConflictError{fmt.Sprintf("Volume '%s' image is %s", vol.Name, inc.State)}
	}

	_, err = q.CreateVolumeAttachment(ctx, queries.CreateVolumeAttachmentParams{
		ID:            uuid.New(),
		VolumeID:      vol.ID,
		IncarnationID: inc.ID,
		SandboxID:     sandboxID,
		ExecutionID:   executionID,
		NodeID:        inc.NodeID,
		Mode:          attachmentModeRW,
		GuestPath:     p.mount.GetPath(),
	})
	if dberrors.IsUniqueConstraintViolation(err) {
		return nil, attachConflictError{fmt.Sprintf("Volume '%s' is mounted by another sandbox", vol.Name)}
	}

	if err != nil {
		return nil, fmt.Errorf("record attachment: %w", err)
	}

	return &orchestrator.SandboxWorkspaceSlot{
		Slot:          uint32(p.slot), //nolint:gosec // checked against maxBlockVolumeMounts above
		VolumeId:      vol.ID.String(),
		IncarnationId: inc.ID.String(),
		MountPath:     p.mount.GetPath(),
		FsUuid:        inc.FsUuid.String(),
		CapacityBytes: uint64(inc.CapacityBytes), //nolint:gosec // a positive tier size (CHECK constraint)
	}, nil
}

// clone runs a fork's volume.clone inline: the operation and its River job
// (delayed, as a fallback should this request die) commit first, then the
// executor runs the operation now. Repeating it for the same sandbox replays
// the same operation.
func (b *blockVolumeAttacher) clone(ctx context.Context, sandboxID string, p blockMountPlan) (uuid.UUID, *api.APIError) {
	key := fmt.Sprintf("fork:%s:volume:%s:revision:%s", sandboxID, p.volume.ID, p.revision.ID)
	incarnationID := uuid.New()

	var op queries.VolumeOperation

	err := b.a.inTx(ctx, func(q *sqlcdb.Client, tx pgx.Tx) error {
		var err error

		op, _, err = b.a.recordVolumeOperation(ctx, q, tx, newVolumeOperation{
			kind:        dboutbox.VolumeOperationClone,
			key:         key,
			hash:        key,
			volumeID:    p.volume.ID,
			scheduledAt: time.Now().Add(forkCloneFallbackDelay),
			params: dboutbox.VolumeOperationParams{
				ClusterID: b.clusterID,
				NodeID:    b.ownerNode,
				TeamID:    b.teamID,
				VolumeID:  p.volume.ID,
				Clone: &dboutbox.VolumeCloneParams{
					SourceVolumeID:    p.volume.ID,
					SourceRevisionID:  p.revision.ID,
					SourceConsistency: p.revision.Consistency,
					Mode:              p.cloneMode,
					IncarnationID:     incarnationID,
					CapacityBytes:     p.revision.SizeBytes,
				},
			},
		})

		return err
	})
	if errors.Is(err, errVolumeJobsUnavailable) {
		return uuid.Nil, &api.APIError{Code: http.StatusServiceUnavailable, ClientMsg: "Volume jobs are not running", Err: err}
	}

	if err != nil {
		return uuid.Nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when cloning a block volume for the fork", Err: err}
	}

	if b.a.volumeExecutor == nil {
		return uuid.Nil, &api.APIError{Code: http.StatusServiceUnavailable, ClientMsg: "Volume jobs are not running", Err: errVolumeJobsUnavailable}
	}

	if err := b.a.volumeExecutor.Execute(ctx, op.ID, false); err != nil {
		if errors.Is(err, outbox.ErrVolumeOperationFailed) {
			return uuid.Nil, &api.APIError{Code: http.StatusConflict, ClientMsg: fmt.Sprintf("Cloning volume '%s' for the fork failed", p.volume.Name), Err: err}
		}

		return uuid.Nil, &api.APIError{Code: http.StatusServiceUnavailable, ClientMsg: fmt.Sprintf("Cloning volume '%s' for the fork did not finish, please retry", p.volume.Name), Err: err}
	}

	// A replayed operation carries the incarnation it was first recorded with.
	var params dboutbox.VolumeOperationParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params.Clone == nil {
		return uuid.Nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when reading the fork clone", Err: err}
	}

	return params.Clone.IncarnationID, nil
}

// Release ends the execution's attachments after a start that did not
// happen, and deletes the incarnations a fork cloned for it: nothing else
// will ever bind them.
func (b *blockVolumeAttacher) Release(ctx context.Context, sandboxID, executionID string) {
	if _, err := b.a.sqlcDB.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: sandboxID, ExecutionID: executionID}); err != nil {
		logger.L().Error(ctx, "failed to release block volume attachments after a failed start", zap.Error(err), logger.WithSandboxID(sandboxID))

		// The attachments still reserve the clones; deleting them now would
		// only be refused. The volume's reconcile reports them as held.
		return
	}

	b.discardForkClones(ctx, sandboxID, b.clones)
}

// discardForkClones schedules the deletion of incarnations cloned for a fork
// whose start failed. It reuses the volume.delete job, scoped to those
// incarnations (VolumeDeleteParams), so it retries like any other volume
// operation, and the node refuses it while a holder still has the image.
// Failing to record it is logged: the incarnation is then an orphan that the
// volume's reconcile lists and its eventual delete removes.
func (b *blockVolumeAttacher) discardForkClones(ctx context.Context, sandboxID string, clones map[uuid.UUID]uuid.UUID) {
	for volumeID, incarnationID := range clones {
		key := fmt.Sprintf("fork-discard:%s:incarnation:%s", sandboxID, incarnationID)

		err := b.a.inTx(ctx, func(q *sqlcdb.Client, tx pgx.Tx) error {
			_, _, err := b.a.recordVolumeOperation(ctx, q, tx, newVolumeOperation{
				kind:     dboutbox.VolumeOperationDelete,
				key:      key,
				hash:     key,
				volumeID: volumeID,
				params: dboutbox.VolumeOperationParams{
					ClusterID: b.clusterID,
					NodeID:    b.ownerNode,
					TeamID:    b.teamID,
					VolumeID:  volumeID,
					Delete:    &dboutbox.VolumeDeleteParams{IncarnationIDs: []uuid.UUID{incarnationID}},
				},
			})

			return err
		})
		if err != nil {
			logger.L().Error(ctx, "failed to schedule deletion of a fork's cloned block volume incarnation",
				zap.Error(err), logger.WithSandboxID(sandboxID), zap.Stringer("volume_id", volumeID), zap.Stringer("incarnation_id", incarnationID))
		}
	}
}

// planResumeBlockVolumes decides how a resume or fork binds the snapshot's
// block volumes, from the revisions recorded for the snapshot build:
//
//   - a memory snapshot needs paired-memory revisions, a filesystem-only one
//     filesystem-quiesced revisions; anything else is refused;
//   - resuming without memory (memory:false) is not supported with block
//     volumes;
//   - a resume binds the exact incarnation the paused execution sealed, and is
//     refused if another execution used it since;
//   - a fork clones each revision into a new incarnation; from a memory
//     snapshot it keeps the filesystem identity and requires
//     allowSharedIdentity.
//
// It returns nil when the snapshot has no block volumes.
func (a *APIStore) planResumeBlockVolumes(
	ctx context.Context,
	teamID uuid.UUID,
	snapshotBuildID uuid.UUID,
	mounts []*orchestrator.SandboxVolumeMount,
	fork, memorySnapshot, filesystemBoot, allowSharedIdentity bool,
) (*blockVolumeAttacher, *api.APIError) {
	blockMounts := slices.DeleteFunc(slices.Clone(mounts), func(m *orchestrator.SandboxVolumeMount) bool { return m.GetBackend() != types.VolumeBackendBlock })
	if len(blockMounts) == 0 {
		return nil, nil
	}

	if filesystemBoot {
		return nil, &api.APIError{Code: http.StatusConflict, ClientMsg: "Resuming without memory (memory: false) is not supported for sandboxes with block volumes"}
	}

	refs, err := a.sqlcDB.ListSnapshotVolumeRefs(ctx, snapshotBuildID)
	if err != nil {
		return nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when reading the snapshot's volumes", Err: err}
	}

	byPath := map[string]queries.ListSnapshotVolumeRefsRow{}
	for _, r := range refs {
		byPath[r.MountPath] = r
	}

	wantConsistency := consistencyFilesystemQuiesced
	cloneMode := dboutbox.VolumeCloneColdBoot
	if memorySnapshot {
		wantConsistency = consistencyPairedMemory
		cloneMode = dboutbox.VolumeCloneMemoryRestore
	}

	attacher := &blockVolumeAttacher{a: a, teamID: teamID}

	for _, m := range blockMounts {
		ref, ok := byPath[m.GetPath()]
		if !ok || ref.Volume.ID.String() != m.GetId() {
			return nil, &api.APIError{
				Code:      http.StatusConflict,
				ClientMsg: fmt.Sprintf("The snapshot has no recorded revision of block volume '%s'", m.GetName()),
				Err:       fmt.Errorf("no snapshot_volume_refs row for build %s path %s", snapshotBuildID, m.GetPath()),
			}
		}

		if ref.VolumeRevision.Consistency != wantConsistency {
			return nil, &api.APIError{
				Code:      http.StatusConflict,
				ClientMsg: fmt.Sprintf("The recorded revision of block volume '%s' is %s, which this snapshot cannot be resumed with", m.GetName(), ref.VolumeRevision.Consistency),
			}
		}

		if fork && memorySnapshot && !allowSharedIdentity {
			return nil, &api.APIError{
				Code:      http.StatusConflict,
				ErrorCode: errCodeSharedIdentityRequired,
				ClientMsg: errMsgSharedIdentityRequired,
			}
		}

		if ref.Volume.OwnerNodeID == nil {
			return nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Block volume has no owner node"}
		}

		if attacher.ownerNode == "" {
			attacher.ownerNode = *ref.Volume.OwnerNodeID
		} else if attacher.ownerNode != *ref.Volume.OwnerNodeID {
			return nil, &api.APIError{Code: http.StatusConflict, ClientMsg: "The sandbox's block volumes are on different nodes"}
		}

		binding := bindExact
		if fork {
			binding = bindClone
		}

		attacher.plans = append(attacher.plans, blockMountPlan{
			mount:     m,
			volume:    ref.Volume,
			binding:   binding,
			slot:      int(ref.Slot),
			revision:  ref.VolumeRevision,
			cloneMode: cloneMode,
		})
	}

	return attacher, nil
}
