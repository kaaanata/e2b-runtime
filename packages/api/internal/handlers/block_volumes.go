package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/e2b-dev/infra/packages/api/internal"
	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/outbox"
	"github.com/e2b-dev/infra/packages/auth/pkg/types"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	dboutbox "github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/apierrors"
	clustershared "github.com/e2b-dev/infra/packages/shared/pkg/clusters"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	orchestratorgrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const (
	volumeBackendNFS   = "nfs"
	volumeBackendBlock = "block"

	// blockVolumeType is the volume_type of block volumes. It names no NFS
	// persistent volume type, so the NFS content paths never resolve it.
	blockVolumeType = "block"

	volumeLifecycleCreating = "creating"
	volumeLifecycleReady    = "ready"
	volumeLifecycleDeleting = "deleting"

	consistencyFilesystemQuiesced = "filesystem-quiesced"
	consistencyPairedMemory       = "paired-memory"
)

var (
	errBlockVolumeNodeUnavailable = errors.New("the node holding the block volume is unavailable")
	errIdempotencyKeyReused       = errors.New("idempotency key was used with a different request")
	errVolumeJobsUnavailable      = errors.New("volume jobs are not running")
)

// blockVolumeNodeSource is the slice of the orchestrator block volumes need
// for placement: block volumes live on one node and never fall back to another.
type blockVolumeNodeSource interface {
	GetClusterNodes(clusterID uuid.UUID) []*nodemanager.Node
	GetNode(clusterID uuid.UUID, nodeID string) *nodemanager.Node
}

// volumeJobEnqueuer inserts the River job of a volume operation in the
// caller's transaction.
type volumeJobEnqueuer interface {
	EnqueueVolumeOperation(ctx context.Context, tx pgx.Tx, kind string, operationID uuid.UUID, scheduledAt time.Time) error
}

// BlockVolumeClient resolves the BlockVolumeService of a block volume's owner
// node (outbox.VolumeNodes).
func (a *APIStore) BlockVolumeClient(_ context.Context, clusterID uuid.UUID, nodeID string) (orchestratorgrpc.BlockVolumeServiceClient, error) {
	node, err := a.blockVolumeNode(clusterID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", outbox.ErrVolumeNodeUnavailable, err)
	}

	client, _ := node.GetClient(context.Background())

	return client.BlockVolumes, nil
}

// blockVolumeNode returns the owner node if it can take requests.
func (a *APIStore) blockVolumeNode(clusterID uuid.UUID, nodeID string) (*nodemanager.Node, error) {
	if a.volumeNodes == nil {
		return nil, errBlockVolumeNodeUnavailable
	}

	node := a.volumeNodes.GetNode(clusterID, nodeID)
	if node == nil || !node.CanAcceptNewRequests() {
		return nil, fmt.Errorf("%w: %s", errBlockVolumeNodeUnavailable, nodeID)
	}

	return node, nil
}

// pickBlockVolumeOwner chooses the node a new block volume lives on among the
// ready nodes advertising block volume support.
func (a *APIStore) pickBlockVolumeOwner(clusterID uuid.UUID) (*nodemanager.Node, error) {
	if a.volumeNodes == nil {
		return nil, errBlockVolumeNodeUnavailable
	}

	var candidates []*nodemanager.Node
	for _, node := range a.volumeNodes.GetClusterNodes(clusterID) {
		if !node.CanAcceptNewRequests() {
			continue
		}

		if _, ok := node.Labels()[internal.BlockVolumeNodeLabel]; ok {
			candidates = append(candidates, node)
		}
	}

	if len(candidates) == 0 {
		return nil, errBlockVolumeNodeUnavailable
	}

	return candidates[rand.IntN(len(candidates))], nil
}

// operationKey scopes an idempotency key: a client key to its team, an
// internal one to what it acts on.
func operationKey(teamID uuid.UUID, clientKey *string, internalKey string) string {
	if clientKey != nil && *clientKey != "" {
		return "team:" + teamID.String() + ":" + *clientKey
	}

	return internalKey
}

// requestHash fingerprints the client-visible request an idempotency key is
// bound to (not the identifiers the server chose for it).
func requestHash(request any) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}

// newVolumeOperation is an operation about to be recorded.
type newVolumeOperation struct {
	kind     string
	key      string
	hash     string
	params   dboutbox.VolumeOperationParams
	volumeID uuid.UUID
	// scheduledAt delays the River job; zero runs it right away.
	scheduledAt time.Time
}

// recordVolumeOperation records an operation and enqueues its job in tx. When
// the key was already used it returns that operation and replayed=true, or
// errIdempotencyKeyReused if it was for a different request. It must be the
// first write of the transaction: on a replay the caller rolls back.
func (a *APIStore) recordVolumeOperation(ctx context.Context, q *sqlcdb.Client, tx pgx.Tx, op newVolumeOperation) (queries.VolumeOperation, bool, error) {
	if a.volumeJobs == nil {
		return queries.VolumeOperation{}, false, errVolumeJobsUnavailable
	}

	params, err := json.Marshal(op.params)
	if err != nil {
		return queries.VolumeOperation{}, false, fmt.Errorf("encode operation params: %w", err)
	}

	volumeID := op.volumeID
	created, err := q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID:             uuid.New(),
		TeamID:         op.params.TeamID,
		VolumeID:       &volumeID,
		Kind:           op.kind,
		IdempotencyKey: op.key,
		ParamsHash:     op.hash,
		Params:         params,
	})

	switch {
	case dberrors.IsNotFoundError(err):
		existing, err := q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: op.kind, IdempotencyKey: op.key})
		if err != nil {
			return queries.VolumeOperation{}, false, fmt.Errorf("read replayed operation: %w", err)
		}

		if existing.ParamsHash != op.hash || existing.TeamID != op.params.TeamID {
			return queries.VolumeOperation{}, false, errIdempotencyKeyReused
		}

		return existing, true, nil
	case err != nil:
		return queries.VolumeOperation{}, false, fmt.Errorf("record operation: %w", err)
	}

	if err := a.volumeJobs.EnqueueVolumeOperation(ctx, tx, op.kind, created.ID, op.scheduledAt); err != nil {
		return queries.VolumeOperation{}, false, err
	}

	return created, false, nil
}

func (a *APIStore) inTx(ctx context.Context, fn func(q *sqlcdb.Client, tx pgx.Tx) error) error {
	q, tx, err := a.sqlcDB.WithTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(q, tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// errReplay ends a transaction whose request turned out to be a replay.
var errReplay = errors.New("replayed request")

// postBlockVolume creates a block volume: the volume row (creating), its
// operation and the volume.create job commit together, and the node builds
// the image in the background.
func (a *APIStore) postBlockVolume(c *gin.Context, team *types.Team, body api.NewVolume, params api.PostVolumesParams) {
	ctx := c.Request.Context()

	if len(a.config.BlockVolumeCapacityTiersGiB) == 0 {
		a.sendAPIStoreError(c, http.StatusNotImplemented, "Block volumes are not enabled in this deployment")

		return
	}

	if body.CapacityGiB == nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "capacityGiB is required for block volumes")

		return
	}

	if !slices.Contains(a.config.BlockVolumeCapacityTiersGiB, *body.CapacityGiB) {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("capacityGiB must be one of %v", a.config.BlockVolumeCapacityTiersGiB))

		return
	}

	clusterID := clustershared.WithClusterFallback(team.ClusterID)

	hash, err := requestHash(struct {
		Name        string `json:"name"`
		Backend     string `json:"backend"`
		CapacityGiB int64  `json:"capacityGiB"`
	}{body.Name, volumeBackendBlock, *body.CapacityGiB})
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when creating volume")
		telemetry.ReportCriticalError(ctx, "error hashing volume request", err)

		return
	}

	node, err := a.pickBlockVolumeOwner(clusterID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "No node can hold block volumes right now")
		telemetry.ReportError(ctx, "no block volume node", err)

		return
	}

	volumeID := uuid.New()
	capacityBytes := *body.CapacityGiB << 30

	var (
		volume queries.Volume
		op     queries.VolumeOperation
	)

	err = a.inTx(ctx, func(q *sqlcdb.Client, tx pgx.Tx) error {
		var replayed bool

		op, replayed, err = a.recordVolumeOperation(ctx, q, tx, newVolumeOperation{
			kind:     dboutbox.VolumeOperationCreate,
			key:      operationKey(team.ID, params.IdempotencyKey, "volume:"+volumeID.String()),
			hash:     hash,
			volumeID: volumeID,
			params: dboutbox.VolumeOperationParams{
				ClusterID: clusterID,
				NodeID:    node.ID,
				TeamID:    team.ID,
				VolumeID:  volumeID,
				Create: &dboutbox.VolumeCreateParams{
					IncarnationID: uuid.New(),
					RevisionID:    uuid.New(),
					CapacityBytes: capacityBytes,
				},
			},
		})
		if err != nil {
			return err
		}

		if replayed {
			return errReplay
		}

		volume, err = q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
			ID:            volumeID,
			TeamID:        team.ID,
			VolumeType:    blockVolumeType,
			Name:          body.Name,
			OwnerNodeID:   &node.ID,
			CapacityBytes: &capacityBytes,
		})

		return err
	})

	switch {
	case errors.Is(err, errReplay):
		a.sendVolumeOperationReplay(c, team, op)

		return
	case errors.Is(err, errIdempotencyKeyReused):
		a.sendAPIStoreError(c, http.StatusConflict, err.Error())

		return
	case dberrors.IsUniqueConstraintViolation(err):
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Volume with name '%s' already exists", body.Name))

		return
	case errors.Is(err, errVolumeJobsUnavailable):
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "Volume jobs are not running")

		return
	case err != nil:
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when creating volume")
		telemetry.ReportCriticalError(ctx, "error creating block volume", err)

		return
	}

	c.JSON(http.StatusAccepted, api.VolumeOperationAccepted{Volume: volumeToAPI(volume), Operation: volumeOperationToAPI(op)})
}

// sendVolumeOperationReplay answers a replayed request with the operation it
// started and the volume as it is now.
func (a *APIStore) sendVolumeOperationReplay(c *gin.Context, team *types.Team, op queries.VolumeOperation) {
	ctx := c.Request.Context()

	if op.VolumeID == nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when reading the operation")

		return
	}

	volume, err := a.sqlcDB.GetVolume(ctx, queries.GetVolumeParams{VolumeID: *op.VolumeID, TeamID: team.ID})
	switch {
	case dberrors.IsNotFoundError(err):
		// The volume has since been deleted; the operation still stands.
		volume = queries.Volume{ID: *op.VolumeID, TeamID: team.ID, Backend: volumeBackendBlock, Lifecycle: "deleted"}
	case err != nil:
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when reading the volume")
		telemetry.ReportCriticalError(ctx, "error reading replayed volume", err)

		return
	}

	c.JSON(http.StatusAccepted, api.VolumeOperationAccepted{Volume: volumeToAPI(volume), Operation: volumeOperationToAPI(op)})
}

// deleteBlockVolume tombstones a block volume: no sandbox may be attached and
// no paused snapshot may reference one of its revisions. The row lock
// serialises this with attaching.
func (a *APIStore) deleteBlockVolume(c *gin.Context, team *types.Team, volume queries.Volume) {
	ctx := c.Request.Context()

	if volume.OwnerNodeID == nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Block volume has no owner node")

		return
	}

	var (
		op      queries.VolumeOperation
		current queries.Volume
	)

	err := a.inTx(ctx, func(q *sqlcdb.Client, tx pgx.Tx) error {
		var err error

		current, err = q.GetBlockVolumeForUpdate(ctx, volume.ID)
		if err != nil {
			return err
		}

		key := "volume:" + volume.ID.String()

		if current.Lifecycle == volumeLifecycleDeleting {
			op, err = q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: dboutbox.VolumeOperationDelete, IdempotencyKey: key})

			return err
		}

		if current.Lifecycle == volumeLifecycleCreating {
			return blockVolumeBusyError{reason: "it is still being created", code: errCodeVolumeCreating}
		}

		attached, err := q.CountActiveVolumeAttachments(ctx, volume.ID)
		if err != nil {
			return err
		}

		if attached > 0 {
			return blockVolumeBusyError{reason: "it is mounted by a sandbox", code: errCodeVolumeAttached}
		}

		refs, err := q.CountVolumeSnapshotRefs(ctx, volume.ID)
		if err != nil {
			return err
		}

		if refs > 0 {
			return blockVolumeBusyError{reason: "a paused sandbox snapshot needs it; kill the paused sandbox first", code: errCodeVolumeReferencedBySnapshot}
		}

		op, _, err = a.recordVolumeOperation(ctx, q, tx, newVolumeOperation{
			kind:     dboutbox.VolumeOperationDelete,
			key:      key,
			hash:     key,
			volumeID: volume.ID,
			params: dboutbox.VolumeOperationParams{
				ClusterID: clustershared.WithClusterFallback(team.ClusterID),
				NodeID:    *volume.OwnerNodeID,
				TeamID:    team.ID,
				VolumeID:  volume.ID,
			},
		})
		if err != nil {
			return err
		}

		current, err = q.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: volume.ID, TeamID: team.ID})

		return err
	})

	var busy blockVolumeBusyError

	switch {
	case errors.As(err, &busy):
		apierrors.SendAPIError(c, &api.APIError{
			Code:      http.StatusConflict,
			ErrorCode: busy.code,
			ClientMsg: fmt.Sprintf("Volume '%s' cannot be deleted: %s", volume.Name, busy.reason),
		})

		return
	case errors.Is(err, errVolumeJobsUnavailable):
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "Volume jobs are not running")

		return
	case err != nil:
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when deleting volume")
		telemetry.ReportCriticalError(ctx, "error deleting block volume", err)

		return
	}

	c.JSON(http.StatusAccepted, api.VolumeOperationAccepted{Volume: volumeToAPI(current), Operation: volumeOperationToAPI(op)})
}

// blockVolumeBusyError refuses a delete; code is the response's error_code.
type blockVolumeBusyError struct{ reason, code string }

func (e blockVolumeBusyError) Error() string { return "block volume is busy: " + e.reason }

// PostVolumesVolumeIDClone creates a new block volume from a cold-bootable
// revision of an existing one, on the same node.
func (a *APIStore) PostVolumesVolumeIDClone(c *gin.Context, volumeID api.VolumeID, params api.PostVolumesVolumeIDCloneParams) {
	ctx := c.Request.Context()

	source, team, ok := a.getVolume(c, volumeID)
	if !ok {
		return
	}

	body, err := ginutils.ParseBody[api.PostVolumesVolumeIDCloneJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	if !isValidVolumeName(body.Name) {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid volume name")

		return
	}

	if source.Backend != volumeBackendBlock {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Only block volumes can be cloned")

		return
	}

	if source.Lifecycle != volumeLifecycleReady || source.OwnerNodeID == nil || source.CapacityBytes == nil {
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("Volume '%s' is %s and cannot be cloned", source.Name, source.Lifecycle))

		return
	}

	revision, apiErr := a.cloneSourceRevision(ctx, source, body.RevisionID)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	clusterID := clustershared.WithClusterFallback(team.ClusterID)
	if _, err := a.blockVolumeNode(clusterID, *source.OwnerNodeID); err != nil {
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "The node holding the volume is unavailable")

		return
	}

	hash, err := requestHash(struct {
		Source   string `json:"source"`
		Name     string `json:"name"`
		Revision string `json:"revision"`
	}{source.ID.String(), body.Name, revision.ID.String()})
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when cloning volume")

		return
	}

	targetID := uuid.New()

	var (
		volume queries.Volume
		op     queries.VolumeOperation
	)

	err = a.inTx(ctx, func(q *sqlcdb.Client, tx pgx.Tx) error {
		var replayed bool

		op, replayed, err = a.recordVolumeOperation(ctx, q, tx, newVolumeOperation{
			kind:     dboutbox.VolumeOperationClone,
			key:      operationKey(team.ID, params.IdempotencyKey, "volume:"+targetID.String()),
			hash:     hash,
			volumeID: targetID,
			params: dboutbox.VolumeOperationParams{
				ClusterID: clusterID,
				NodeID:    *source.OwnerNodeID,
				TeamID:    team.ID,
				VolumeID:  targetID,
				Clone: &dboutbox.VolumeCloneParams{
					SourceVolumeID:    source.ID,
					SourceRevisionID:  revision.ID,
					SourceConsistency: revision.Consistency,
					Mode:              dboutbox.VolumeCloneColdBoot,
					IncarnationID:     uuid.New(),
					CapacityBytes:     *source.CapacityBytes,
					NewVolume:         true,
				},
			},
		})
		if err != nil {
			return err
		}

		if replayed {
			return errReplay
		}

		volume, err = q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
			ID:            targetID,
			TeamID:        team.ID,
			VolumeType:    blockVolumeType,
			Name:          body.Name,
			OwnerNodeID:   source.OwnerNodeID,
			CapacityBytes: source.CapacityBytes,
		})

		return err
	})

	switch {
	case errors.Is(err, errReplay):
		a.sendVolumeOperationReplay(c, team, op)

		return
	case errors.Is(err, errIdempotencyKeyReused):
		a.sendAPIStoreError(c, http.StatusConflict, err.Error())

		return
	case dberrors.IsUniqueConstraintViolation(err):
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Volume with name '%s' already exists", body.Name))

		return
	case errors.Is(err, errVolumeJobsUnavailable):
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "Volume jobs are not running")

		return
	case err != nil:
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when cloning volume")
		telemetry.ReportCriticalError(ctx, "error cloning block volume", err)

		return
	}

	c.JSON(http.StatusAccepted, api.VolumeOperationAccepted{Volume: volumeToAPI(volume), Operation: volumeOperationToAPI(op)})
}

// cloneSourceRevision resolves the revision a volume clone starts from. Only
// filesystem-quiesced revisions are cloneable into a new volume: a
// paired-memory image is consistent only together with its memory snapshot.
func (a *APIStore) cloneSourceRevision(ctx context.Context, source queries.Volume, revisionID *string) (queries.VolumeRevision, *api.APIError) {
	if revisionID != nil {
		id, err := uuid.Parse(*revisionID)
		if err != nil {
			return queries.VolumeRevision{}, &api.APIError{Code: http.StatusBadRequest, ClientMsg: "Invalid revision ID"}
		}

		rev, err := a.sqlcDB.GetVolumeRevision(ctx, queries.GetVolumeRevisionParams{ID: id, VolumeID: source.ID})
		if dberrors.IsNotFoundError(err) || (err == nil && rev.State != volumeLifecycleReady) {
			return queries.VolumeRevision{}, &api.APIError{Code: http.StatusNotFound, ClientMsg: "Revision not found"}
		}

		if err != nil {
			return queries.VolumeRevision{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when reading the revision", Err: err}
		}

		if rev.Consistency != consistencyFilesystemQuiesced {
			return queries.VolumeRevision{}, &api.APIError{
				Code:      http.StatusConflict,
				ClientMsg: "The revision belongs to a memory snapshot and can only be used by resuming or forking that snapshot",
			}
		}

		return rev, nil
	}

	revs, err := a.sqlcDB.ListVolumeRevisions(ctx, source.ID)
	if err != nil {
		return queries.VolumeRevision{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error when listing revisions", Err: err}
	}

	for _, rev := range revs {
		if rev.Consistency == consistencyFilesystemQuiesced {
			return rev, nil
		}
	}

	return queries.VolumeRevision{}, &api.APIError{Code: http.StatusConflict, ClientMsg: "The volume has no revision that can be cloned"}
}

// GetVolumesVolumeIDRevisions lists a block volume's sealed revisions.
func (a *APIStore) GetVolumesVolumeIDRevisions(c *gin.Context, volumeID api.VolumeID) {
	ctx := c.Request.Context()

	volume, _, ok := a.getVolume(c, volumeID)
	if !ok {
		return
	}

	if volume.Backend != volumeBackendBlock {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Only block volumes have revisions")

		return
	}

	revs, err := a.sqlcDB.ListVolumeRevisions(ctx, volume.ID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when listing revisions")
		telemetry.ReportCriticalError(ctx, "error listing volume revisions", err)

		return
	}

	result := make([]api.VolumeRevision, 0, len(revs))
	for _, rev := range revs {
		result = append(result, volumeRevisionToAPI(rev))
	}

	c.JSON(http.StatusOK, result)
}

// GetOperationsOperationID reports a background volume operation.
func (a *APIStore) GetOperationsOperationID(c *gin.Context, operationID api.OperationID) {
	ctx := c.Request.Context()

	team, apiErr := a.GetTeam(ctx, c, nil)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		telemetry.ReportCriticalError(ctx, "error when getting team", apiErr.Err)

		return
	}

	id, err := uuid.Parse(operationID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid operation ID")

		return
	}

	op, err := a.sqlcDB.GetVolumeOperation(ctx, queries.GetVolumeOperationParams{ID: id, TeamID: team.ID})
	switch {
	case dberrors.IsNotFoundError(err):
		a.sendAPIStoreError(c, http.StatusNotFound, "Operation not found")

		return
	case err != nil:
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when getting operation")
		telemetry.ReportCriticalError(ctx, "error getting volume operation", err)

		return
	}

	c.JSON(http.StatusOK, volumeOperationToAPI(op))
}

func volumeToAPI(v queries.Volume) api.Volume {
	out := api.Volume{
		VolumeID: v.ID.String(),
		Name:     v.Name,
		Backend:  new(volumeBackendToAPI(v.Backend)),
		State:    new(api.VolumeState(v.Lifecycle)),
	}

	if v.Backend == volumeBackendBlock {
		out.OwnerNode = v.OwnerNodeID
		if v.CapacityBytes != nil {
			out.CapacityGiB = new(*v.CapacityBytes >> 30)
		}
	}

	return out
}

func volumeBackendToAPI(backend string) api.VolumeBackend {
	if backend == volumeBackendBlock {
		return api.Block
	}

	return api.Nfs
}

func volumeOperationToAPI(op queries.VolumeOperation) api.VolumeOperation {
	out := api.VolumeOperation{
		OperationID: op.ID.String(),
		Kind:        op.Kind,
		State:       op.State,
		Error:       op.Error,
		Attempts:    op.Attempts,
		CreatedAt:   op.CreatedAt.Time,
		UpdatedAt:   op.UpdatedAt.Time,
	}

	if op.VolumeID != nil {
		out.VolumeID = new(op.VolumeID.String())
	}

	if op.FinishedAt.Valid {
		out.FinishedAt = &op.FinishedAt.Time
	}

	return out
}

func volumeRevisionToAPI(rev queries.VolumeRevision) api.VolumeRevision {
	out := api.VolumeRevision{
		RevisionID:    rev.ID.String(),
		VolumeID:      rev.VolumeID.String(),
		IncarnationID: rev.IncarnationID.String(),
		Consistency:   api.VolumeRevisionConsistency(rev.Consistency),
		SizeBytes:     rev.SizeBytes,
		CreatedAt:     rev.CreatedAt.Time,
	}

	if rev.ParentID != nil {
		out.ParentRevisionID = new(rev.ParentID.String())
	}

	return out
}
