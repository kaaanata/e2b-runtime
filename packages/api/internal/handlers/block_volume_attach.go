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
	// mount is the request's mount; Attach fills in the binding.
	mount   *orchestrator.SandboxVolumeMount
	volume  queries.Volume
	binding blockMountBinding
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
	mounts    []*orchestrator.SandboxVolumeMount
	plans     []blockMountPlan
}

type attachConflictError struct{ msg string }

func (e attachConflictError) Error() string { return e.msg }

// Attach records one RW attachment per block mount, in one transaction that
// locks the volumes (in id order) against deletion and against each other.
// A fork's clones run first, synchronously, through the operation ledger.
func (b *blockVolumeAttacher) Attach(ctx context.Context, clusterID uuid.UUID, sandboxID, executionID string) ([]*orchestrator.SandboxVolumeMount, *api.APIError) {
	b.clusterID = clusterID

	clones := map[uuid.UUID]uuid.UUID{} // volume -> cloned incarnation

	for _, p := range b.plans {
		if p.binding != bindClone {
			continue
		}

		inc, apiErr := b.clone(ctx, sandboxID, p)
		if apiErr != nil {
			return nil, apiErr
		}

		clones[p.volume.ID] = inc
	}

	plans := slices.Clone(b.plans)
	slices.SortFunc(plans, func(x, y blockMountPlan) int { return strings.Compare(x.volume.ID.String(), y.volume.ID.String()) })

	err := b.a.inTx(ctx, func(q *sqlcdb.Client, _ pgx.Tx) error {
		for _, p := range plans {
			if err := b.attachOne(ctx, q, sandboxID, executionID, p, clones[p.volume.ID]); err != nil {
				return err
			}
		}

		return nil
	})

	var conflict attachConflictError

	switch {
	case errors.As(err, &conflict):
		return nil, &api.APIError{Code: http.StatusConflict, ClientMsg: conflict.msg, Err: err}
	case err != nil:
		return nil, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when attaching block volumes", Err: err}
	}

	return b.mounts, nil
}

func (b *blockVolumeAttacher) attachOne(ctx context.Context, q *sqlcdb.Client, sandboxID, executionID string, p blockMountPlan, cloned uuid.UUID) error {
	vol, err := q.GetBlockVolumeForUpdate(ctx, p.volume.ID)
	if err != nil {
		return fmt.Errorf("lock volume %s: %w", p.volume.ID, err)
	}

	if vol.Lifecycle != volumeLifecycleReady || vol.DeletedAt.Valid {
		return attachConflictError{fmt.Sprintf("Volume '%s' is %s", vol.Name, vol.Lifecycle)}
	}

	var incarnationID uuid.UUID

	switch p.binding {
	case bindHead:
		if vol.HeadIncarnationID == nil {
			return attachConflictError{fmt.Sprintf("Volume '%s' has no image yet", vol.Name)}
		}

		incarnationID = *vol.HeadIncarnationID
	case bindExact:
		incarnationID = p.revision.IncarnationID

		latest, err := q.GetLatestIncarnationRevision(ctx, incarnationID)
		if err != nil && !dberrors.IsNotFoundError(err) {
			return fmt.Errorf("read latest revision: %w", err)
		}

		advanced, err := q.IncarnationAttachedSince(ctx, queries.IncarnationAttachedSinceParams{
			IncarnationID: incarnationID,
			Since:         p.revision.CreatedAt,
		})
		if err != nil {
			return fmt.Errorf("read attachments: %w", err)
		}

		if advanced || latest.ID != p.revision.ID {
			return attachConflictError{fmt.Sprintf(
				"Volume '%s' was used by another sandbox after this snapshot was taken; fork the snapshot instead of resuming it", vol.Name)}
		}
	case bindClone:
		incarnationID = cloned
	}

	inc, err := q.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: incarnationID, VolumeID: vol.ID})
	if err != nil {
		return fmt.Errorf("read incarnation %s: %w", incarnationID, err)
	}

	if inc.State != volumeLifecycleReady {
		return attachConflictError{fmt.Sprintf("Volume '%s' image is %s", vol.Name, inc.State)}
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
		return attachConflictError{fmt.Sprintf("Volume '%s' is mounted by another sandbox", vol.Name)}
	}

	if err != nil {
		return fmt.Errorf("record attachment: %w", err)
	}

	p.mount.IncarnationId = inc.ID.String()
	p.mount.FsUuid = inc.FsUuid.String()
	p.mount.CapacityBytes = uint64(inc.CapacityBytes)

	return nil
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
// happen.
func (b *blockVolumeAttacher) Release(ctx context.Context, sandboxID, executionID string) {
	if _, err := b.a.sqlcDB.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: sandboxID, ExecutionID: executionID}); err != nil {
		logger.L().Error(ctx, "failed to release block volume attachments after a failed start", zap.Error(err), logger.WithSandboxID(sandboxID))
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

	attacher := &blockVolumeAttacher{a: a, teamID: teamID, mounts: mounts}

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
				Code: http.StatusConflict,
				ClientMsg: "Forking a sandbox with block volumes resumes the same memory and filesystem identity in every fork; " +
					"set allowSharedIdentity to true to accept that",
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
			revision:  ref.VolumeRevision,
			cloneMode: cloneMode,
		})
	}

	return attacher, nil
}
