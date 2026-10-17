package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal"
	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/cfg"
	apidb "github.com/e2b-dev/infra/packages/api/internal/db"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	dbmodule "github.com/e2b-dev/infra/packages/db"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
	dboutbox "github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// riverEnqueuer inserts volume jobs with an insert-only River client, so the
// tests see the real job rows (and their rollback with the transaction).
type riverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

func (r riverEnqueuer) EnqueueVolumeOperation(ctx context.Context, tx pgx.Tx, kind string, operationID uuid.UUID, _ time.Time) error {
	args, err := dboutbox.VolumeJobArgs(kind, operationID)
	if err != nil {
		return err
	}

	_, err = r.client.InsertTx(ctx, tx, args, nil)

	return err
}

// staticNodes is a fixed cluster view.
type staticNodes struct{ nodes []*nodemanager.Node }

func (s staticNodes) GetClusterNodes(uuid.UUID) []*nodemanager.Node { return s.nodes }

func (s staticNodes) GetNode(_ uuid.UUID, id string) *nodemanager.Node {
	for _, n := range s.nodes {
		if n.ID == id {
			return n
		}
	}

	return nil
}

type blockAPIFixture struct {
	db    *testutils.Database
	store *APIStore
	team  *authtypes.Team
}

func newBlockAPIFixture(t *testing.T, db *testutils.Database, nodes ...*nodemanager.Node) blockAPIFixture {
	t.Helper()

	td := ldtestdata.DataSource()
	td.Update(td.Flag(featureflags.PersistentVolumesFlag.Key()).VariationForAll(true))
	ff, err := featureflags.NewClientWithDatasource(td)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ff.Close(context.WithoutCancel(t.Context()))) })

	client, err := river.NewClient(riverpgxv5.New(db.SqlcClient.Pool()), &river.Config{Schema: dbmodule.RiverSchema})
	require.NoError(t, err)

	if nodes == nil {
		nodes = []*nodemanager.Node{nodemanager.NewTestNode("node-a", api.NodeStatusReady, 0, 4, nodemanager.WithLabels([]string{internal.BlockVolumeNodeLabel}))}
	}

	teamID := testutils.CreateTestTeam(t, db)

	return blockAPIFixture{
		db: db,
		store: &APIStore{
			featureFlags: ff,
			sqlcDB:       db.SqlcClient,
			volumeNodes:  staticNodes{nodes: nodes},
			volumeJobs:   riverEnqueuer{client: client},
			config:       cfg.Config{BlockVolumeCapacityTiersGiB: []int64{10, 20}},
		},
		team: &authtypes.Team{Team: &authqueries.Team{ID: teamID}},
	}
}

func (f blockAPIFixture) request(t *testing.T, method, path string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	auth.SetTeamInfoForTest(t, c, f.team)

	return c, recorder
}

func (f blockAPIFixture) create(t *testing.T, body api.NewVolume, key *string) (int, api.VolumeOperationAccepted) {
	t.Helper()

	c, rec := f.request(t, http.MethodPost, "/volumes", body)
	f.store.PostVolumes(c, api.PostVolumesParams{IdempotencyKey: key})

	var accepted api.VolumeOperationAccepted
	if rec.Code == http.StatusAccepted {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
	}

	return rec.Code, accepted
}

func (f blockAPIFixture) jobs(t *testing.T, operationID string) []string {
	t.Helper()

	rows, err := f.db.SqlcClient.Pool().Query(t.Context(), `SELECT kind FROM river.river_job WHERE args->>'operation_id' = $1`, operationID)
	require.NoError(t, err)

	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)

	return kinds
}

// readyVolume creates a block volume and materialises it the way a
// succeeded volume.create does.
func (f blockAPIFixture) readyVolume(t *testing.T) (queries.Volume, queries.VolumeIncarnation, queries.VolumeRevision) {
	t.Helper()

	code, accepted := f.create(t, api.NewVolume{Name: "ws", Backend: new(api.Block), CapacityGiB: new(int64(10))}, nil)
	require.Equal(t, http.StatusAccepted, code)

	ctx := t.Context()
	q := f.db.SqlcClient
	opID := uuid.MustParse(accepted.Operation.OperationID)
	volID := uuid.MustParse(accepted.Volume.VolumeID)

	inc, err := q.InsertVolumeIncarnation(ctx, queries.InsertVolumeIncarnationParams{
		ID: uuid.New(), VolumeID: volID, NodeID: "node-a", SubvolPath: "inc", SubvolUuid: uuid.New(),
		FsUuid: uuid.New(), CapacityBytes: 10 << 30, OperationID: opID,
	})
	require.NoError(t, err)

	rev, err := q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID: uuid.New(), VolumeID: volID, IncarnationID: inc.ID, SubvolUuid: uuid.New(), FsUuid: inc.FsUuid,
		SizeBytes: 10 << 30, Consistency: consistencyFilesystemQuiesced, OperationID: opID,
	})
	require.NoError(t, err)
	require.NoError(t, q.SetBlockVolumeReady(ctx, queries.SetBlockVolumeReadyParams{VolumeID: volID, HeadIncarnationID: &inc.ID}))
	require.NoError(t, q.CompleteVolumeOperation(ctx, queries.CompleteVolumeOperationParams{ID: opID, Result: []byte(`{}`)}))

	vol, err := q.GetBlockVolumeForUpdate(ctx, volID)
	require.NoError(t, err)

	return vol, inc, rev
}

func TestPostVolumes_Block(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)

	t.Run("records the volume, operation and job together and replays by key", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		key := "create-1"
		body := api.NewVolume{Name: "ws", Backend: new(api.Block), CapacityGiB: new(int64(10))}

		code, accepted := f.create(t, body, &key)
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, api.VolumeStateCreating, *accepted.Volume.State)
		assert.Equal(t, api.Block, *accepted.Volume.Backend)
		assert.Equal(t, int64(10), *accepted.Volume.CapacityGiB)
		assert.Equal(t, "node-a", *accepted.Volume.OwnerNode)
		assert.Equal(t, dboutbox.VolumeOperationCreate, accepted.Operation.Kind)
		assert.Equal(t, dboutbox.VolumeOperationPending, accepted.Operation.State)
		assert.Equal(t, []string{"volume.create"}, f.jobs(t, accepted.Operation.OperationID))

		var params dboutbox.VolumeOperationParams
		op, err := f.db.SqlcClient.GetVolumeOperation(t.Context(), queries.GetVolumeOperationParams{ID: uuid.MustParse(accepted.Operation.OperationID), TeamID: f.team.ID})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(op.Params, &params))
		assert.Equal(t, int64(10<<30), params.Create.CapacityBytes)
		assert.Equal(t, "node-a", params.NodeID)

		code, replay := f.create(t, body, &key)
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, accepted.Operation.OperationID, replay.Operation.OperationID)
		assert.Equal(t, accepted.Volume.VolumeID, replay.Volume.VolumeID)
		assert.Len(t, f.jobs(t, accepted.Operation.OperationID), 1, "a replay enqueues nothing")

		other := api.NewVolume{Name: "ws", Backend: new(api.Block), CapacityGiB: new(int64(20))}
		code, _ = f.create(t, other, &key)
		assert.Equal(t, http.StatusConflict, code, "the key is bound to its first request")

		code, _ = f.create(t, body, nil)
		assert.Equal(t, http.StatusBadRequest, code, "the name is taken")
	})

	t.Run("a consistencyProfile in the body is ignored: profiles are per mount", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		c, rec := f.request(t, http.MethodPost, "/volumes", map[string]any{
			"name": "ws", "backend": "block", "capacityGiB": 10, "consistencyProfile": "strict",
		})
		f.store.PostVolumes(c, api.PostVolumesParams{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	})

	t.Run("validates capacity and placement before recording anything", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)

		code, _ := f.create(t, api.NewVolume{Name: "a", Backend: new(api.Block)}, nil)
		assert.Equal(t, http.StatusBadRequest, code, "capacity is required")

		code, _ = f.create(t, api.NewVolume{Name: "a", Backend: new(api.Block), CapacityGiB: new(int64(7))}, nil)
		assert.Equal(t, http.StatusBadRequest, code, "capacity must be a tier")

		code, _ = f.create(t, api.NewVolume{Name: "a", CapacityGiB: new(int64(10))}, nil)
		assert.Equal(t, http.StatusBadRequest, code, "capacity is for block volumes")

		f.store.config.BlockVolumeCapacityTiersGiB = nil
		code, _ = f.create(t, api.NewVolume{Name: "a", Backend: new(api.Block), CapacityGiB: new(int64(10))}, nil)
		assert.Equal(t, http.StatusNotImplemented, code)

		unlabeled := newBlockAPIFixture(t, db, nodemanager.NewTestNode("node-b", api.NodeStatusReady, 0, 4))
		code, _ = unlabeled.create(t, api.NewVolume{Name: "a", Backend: new(api.Block), CapacityGiB: new(int64(10))}, nil)
		assert.Equal(t, http.StatusServiceUnavailable, code, "only nodes advertising block volumes own them")

		vols, err := f.db.SqlcClient.FindVolumesByTeamID(t.Context(), f.team.ID)
		require.NoError(t, err)
		assert.Empty(t, vols)
	})
}

func TestBlockVolumeLifecycleEndpoints(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)

	t.Run("delete refuses while attached and then tombstones through a job", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, inc, _ := f.readyVolume(t)

		att, err := f.db.SqlcClient.CreateVolumeAttachment(ctx, queries.CreateVolumeAttachmentParams{
			ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, SandboxID: "sbx", ExecutionID: "e", NodeID: "node-a", Mode: "rw", GuestPath: "/w",
		})
		require.NoError(t, err)

		c, rec := f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, errCodeVolumeAttached, errorCode(t, rec))

		require.NoError(t, f.db.SqlcClient.ReleaseVolumeAttachment(ctx, att.ID))

		c, rec = f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		var accepted api.VolumeOperationAccepted
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
		assert.Equal(t, api.VolumeStateDeleting, *accepted.Volume.State)
		assert.Equal(t, dboutbox.VolumeOperationDelete, accepted.Operation.Kind)
		assert.Equal(t, []string{"volume.delete"}, f.jobs(t, accepted.Operation.OperationID))

		c, rec = f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		require.Equal(t, http.StatusAccepted, rec.Code)

		var again api.VolumeOperationAccepted
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &again))
		assert.Equal(t, accepted.Operation.OperationID, again.Operation.OperationID, "a repeated delete reports the same operation")

		// While the images are being removed the volume is still reported.
		c, rec = f.request(t, http.MethodGet, "/volumes/"+vol.ID.String(), nil)
		f.store.GetVolumesVolumeID(c, vol.ID.String())
		require.Equal(t, http.StatusOK, rec.Code)

		var deleting api.VolumeAndToken
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &deleting))
		assert.Equal(t, api.VolumeStateDeleting, *deleting.State)

		// A deleting volume cannot be attached.
		_, mounts, err := convertAPIVolumesToOrchestratorVolumes(ctx, f.db.SqlcClient, f.store.featureFlags, f.team.ID,
			[]api.SandboxVolumeMount{{Name: "ws", Path: "/w"}}, &queries.EnvBuild{EnvdVersion: new(minEnvdVersionForBlockVolumes)})
		require.Error(t, err)
		assert.Nil(t, mounts)
	})

	t.Run("delete refuses while a snapshot references a revision", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, _, rev := f.readyVolume(t)

		buildID := testutils.CreateTestBuild(t, ctx, f.db, testutils.CreateTestTemplate(t, f.db, f.team.ID), "success")
		require.NoError(t, f.db.SqlcClient.InsertSnapshotVolumeRef(ctx, queries.InsertSnapshotVolumeRefParams{
			SnapshotBuildID: buildID, RevisionID: rev.ID, VolumeID: vol.ID, MountPath: "/w",
		}))

		c, rec := f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, errCodeVolumeReferencedBySnapshot, errorCode(t, rec))
	})

	t.Run("killing the paused sandbox releases its snapshot's references", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, inc, _ := f.readyVolume(t)

		// volume -> sandbox -> pause: the pause's snapshot build references a
		// paired-memory revision of the volume.
		base := testutils.CreateTestTemplate(t, f.db, f.team.ID)
		sandboxID := "sbx-" + uuid.NewString()
		snap := testutils.UpsertTestSnapshot(t, ctx, f.db, "snap-"+uuid.NewString(), sandboxID, f.team.ID, base)
		sealRevision(t, f, vol, inc, consistencyPairedMemory, snap.BuildID)

		c, rec := f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, errCodeVolumeReferencedBySnapshot, errorCode(t, rec))

		// kill: DELETE /sandboxes/{id} of a paused sandbox soft-deletes its
		// snapshot template (deleteSnapshot -> softDeleteTemplate).
		builds, err := apidb.GetSnapshotBuilds(ctx, f.db.SqlcClient, f.team.ID, sandboxID)
		require.NoError(t, err)
		_, err = f.store.softDeleteTemplate(ctx, f.team.ID, builds.TemplateID)
		require.NoError(t, err)

		refs, err := f.db.SqlcClient.CountVolumeSnapshotRefs(ctx, vol.ID)
		require.NoError(t, err)
		assert.Zero(t, refs)

		c, rec = f.request(t, http.MethodDelete, "/volumes/"+vol.ID.String(), nil)
		f.store.DeleteVolumesVolumeID(c, vol.ID.String())
		assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	})

	t.Run("delete refuses while the volume is being created", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		code, accepted := f.create(t, api.NewVolume{Name: "new", Backend: new(api.Block), CapacityGiB: new(int64(10))}, nil)
		require.Equal(t, http.StatusAccepted, code)

		c, rec := f.request(t, http.MethodDelete, "/volumes/"+accepted.Volume.VolumeID, nil)
		f.store.DeleteVolumesVolumeID(c, accepted.Volume.VolumeID)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, errCodeVolumeCreating, errorCode(t, rec))
	})

	t.Run("a tombstoned volume is gone from GET and deleted in replays", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		key := "tombstone-1"
		body := api.NewVolume{Name: "gone", Backend: new(api.Block), CapacityGiB: new(int64(10))}

		code, accepted := f.create(t, body, &key)
		require.Equal(t, http.StatusAccepted, code)

		volID := uuid.MustParse(accepted.Volume.VolumeID)
		require.NoError(t, f.db.SqlcClient.SetBlockVolumeFailed(ctx, volID))
		_, err := f.db.SqlcClient.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: volID, TeamID: f.team.ID})
		require.NoError(t, err)
		require.NoError(t, f.db.SqlcClient.TombstoneBlockVolume(ctx, volID))

		c, rec := f.request(t, http.MethodGet, "/volumes/"+volID.String(), nil)
		f.store.GetVolumesVolumeID(c, volID.String())
		assert.Equal(t, http.StatusNotFound, rec.Code)

		code, replay := f.create(t, body, &key)
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, api.VolumeStateDeleted, *replay.Volume.State)
		assert.Equal(t, accepted.Operation.OperationID, replay.Operation.OperationID)
	})

	t.Run("clone starts from the newest quiesced revision into a new volume", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		vol, _, rev := f.readyVolume(t)

		c, rec := f.request(t, http.MethodPost, "/volumes/"+vol.ID.String()+"/clone", api.CloneVolume{Name: "ws-copy"})
		f.store.PostVolumesVolumeIDClone(c, vol.ID.String(), api.PostVolumesVolumeIDCloneParams{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		var accepted api.VolumeOperationAccepted
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
		assert.Equal(t, "ws-copy", accepted.Volume.Name)
		assert.Equal(t, api.VolumeStateCreating, *accepted.Volume.State)
		assert.Equal(t, "node-a", *accepted.Volume.OwnerNode, "a clone stays on its source's node")
		assert.Equal(t, []string{"volume.clone"}, f.jobs(t, accepted.Operation.OperationID))

		op, err := f.db.SqlcClient.GetVolumeOperation(t.Context(), queries.GetVolumeOperationParams{ID: uuid.MustParse(accepted.Operation.OperationID), TeamID: f.team.ID})
		require.NoError(t, err)

		var params dboutbox.VolumeOperationParams
		require.NoError(t, json.Unmarshal(op.Params, &params))
		assert.Equal(t, rev.ID, params.Clone.SourceRevisionID)
		assert.Equal(t, vol.ID, params.Clone.SourceVolumeID)
		assert.Equal(t, dboutbox.VolumeCloneColdBoot, params.Clone.Mode)
		assert.True(t, params.Clone.NewVolume)

		// GET /operations/{id} is team-scoped.
		c, rec = f.request(t, http.MethodGet, "/operations/"+accepted.Operation.OperationID, nil)
		f.store.GetOperationsOperationID(c, accepted.Operation.OperationID)
		require.Equal(t, http.StatusOK, rec.Code)

		other := newBlockAPIFixture(t, db)
		c, rec = other.request(t, http.MethodGet, "/operations/"+accepted.Operation.OperationID, nil)
		other.store.GetOperationsOperationID(c, accepted.Operation.OperationID)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("a paired-memory revision is not cloneable into a new volume", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, inc, rev := f.readyVolume(t)

		paired, err := f.db.SqlcClient.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
			ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, ParentID: &rev.ID, SubvolUuid: uuid.New(), FsUuid: inc.FsUuid,
			SizeBytes: 10 << 30, Consistency: consistencyPairedMemory, OperationID: rev.OperationID,
		})
		require.NoError(t, err)

		id := paired.ID.String()
		c, rec := f.request(t, http.MethodPost, "/volumes/"+vol.ID.String()+"/clone", api.CloneVolume{Name: "x", RevisionID: &id})
		f.store.PostVolumesVolumeIDClone(c, vol.ID.String(), api.PostVolumesVolumeIDCloneParams{})
		assert.Equal(t, http.StatusConflict, rec.Code)

		c, rec = f.request(t, http.MethodGet, "/volumes/"+vol.ID.String()+"/revisions", nil)
		f.store.GetVolumesVolumeIDRevisions(c, vol.ID.String())
		require.Equal(t, http.StatusOK, rec.Code)

		var revs []api.VolumeRevision
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &revs))
		require.Len(t, revs, 2)
		assert.Equal(t, api.PairedMemory, revs[0].Consistency, "newest first")
		assert.Equal(t, rev.ID.String(), *revs[0].ParentRevisionID)
	})

	t.Run("get reports a block volume without a content token", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		vol, _, _ := f.readyVolume(t)

		c, rec := f.request(t, http.MethodGet, "/volumes/"+vol.ID.String(), nil)
		f.store.GetVolumesVolumeID(c, vol.ID.String())
		require.Equal(t, http.StatusOK, rec.Code)

		var got api.VolumeAndToken
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Empty(t, got.Token)
		assert.Equal(t, api.Unsupported, *got.ContentApi)
		assert.Equal(t, api.VolumeStateReady, *got.State)
	})
}

func TestBlockVolumeAttach(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	newEnvd := &queries.EnvBuild{EnvdVersion: new(minEnvdVersionForBlockVolumes)}

	t.Run("a new sandbox attaches the head as the single writer", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		_, inc, _ := f.readyVolume(t)

		mounts, blocks, err := convertAPIVolumesToOrchestratorVolumes(ctx, f.db.SqlcClient, f.store.featureFlags, f.team.ID,
			[]api.SandboxVolumeMount{{Name: "ws", Path: "/workspace"}}, newEnvd)
		require.NoError(t, err)
		require.NotNil(t, blocks)
		assert.Equal(t, "node-a", blocks.ownerNode)
		require.Len(t, mounts, 1)
		assert.Equal(t, volumeBackendBlock, mounts[0].GetBackend())

		attacher := &blockVolumeAttacher{a: f.store, teamID: f.team.ID, ownerNode: blocks.ownerNode, plans: blocks.plans}
		bound, apiErr := attacher.Attach(ctx, uuid.New(), "sbx-1", "exec-1")
		require.Nil(t, apiErr)
		require.Len(t, bound, 1)
		assert.EqualValues(t, 0, bound[0].GetSlot(), "a new sandbox takes the slots in mount order")
		assert.Equal(t, inc.VolumeID.String(), bound[0].GetVolumeId())
		assert.Equal(t, "/workspace", bound[0].GetMountPath())
		assert.Equal(t, inc.ID.String(), bound[0].GetIncarnationId())
		assert.Equal(t, inc.FsUuid.String(), bound[0].GetFsUuid())
		assert.EqualValues(t, 10<<30, bound[0].GetCapacityBytes())

		second, _, err := convertAPIVolumesToOrchestratorVolumes(ctx, f.db.SqlcClient, f.store.featureFlags, f.team.ID,
			[]api.SandboxVolumeMount{{Name: "ws", Path: "/workspace"}}, newEnvd)
		require.NoError(t, err)

		other := &blockVolumeAttacher{a: f.store, teamID: f.team.ID, ownerNode: "node-a", plans: []blockMountPlan{{mount: second[0], volume: attacher.plans[0].volume, binding: bindHead}}}
		_, apiErr = other.Attach(ctx, uuid.New(), "sbx-2", "exec-2")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
		assert.Equal(t, errCodeVolumeAttached, apiErr.ErrorCode, "a second writer is told the volume is attached")

		attacher.Release(ctx, "sbx-1", "exec-1")
		_, apiErr = other.Attach(ctx, uuid.New(), "sbx-2", "exec-2")
		assert.Nil(t, apiErr, "the head is free once the writer is released")
	})

	t.Run("block mounts are validated", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		f.readyVolume(t)
		code, _ := f.create(t, api.NewVolume{Name: "pending", Backend: new(api.Block), CapacityGiB: new(int64(10))}, nil)
		require.Equal(t, http.StatusAccepted, code)

		convert := func(mounts []api.SandboxVolumeMount, build *queries.EnvBuild) error {
			_, _, err := convertAPIVolumesToOrchestratorVolumes(ctx, f.db.SqlcClient, f.store.featureFlags, f.team.ID, mounts, build)

			return err
		}

		require.ErrorIs(t, convert([]api.SandboxVolumeMount{{Name: "ws", Path: "/w"}}, &queries.EnvBuild{EnvdVersion: new("0.9.0")}), errBlockVolumesNotSupported)
		require.ErrorAs(t, convert([]api.SandboxVolumeMount{{Name: "ws", Path: "/w", ConsistencyProfile: new(api.Cto)}}, newEnvd), &InvalidVolumeMountsError{})
		require.ErrorAs(t, convert([]api.SandboxVolumeMount{{Name: "pending", Path: "/w"}}, newEnvd), &InvalidVolumeMountsError{})
		require.ErrorAs(t, convert([]api.SandboxVolumeMount{{Name: "ws", Path: "/a"}, {Name: "ws", Path: "/b"}}, newEnvd), &InvalidVolumeMountsError{})
	})

	t.Run("resume binds the exact incarnation unless another execution used it", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, inc, _ := f.readyVolume(t)
		q := f.db.SqlcClient

		// The paused execution's attachment, then its pause seal.
		_, err := q.CreateVolumeAttachment(ctx, queries.CreateVolumeAttachmentParams{
			ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, SandboxID: "sbx", ExecutionID: "e1", NodeID: "node-a", Mode: "rw", GuestPath: "/w",
		})
		require.NoError(t, err)
		_, err = q.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: "sbx", ExecutionID: "e1"})
		require.NoError(t, err)

		buildID := testutils.CreateTestBuild(t, ctx, f.db, testutils.CreateTestTemplate(t, f.db, f.team.ID), "success")
		sealed := sealRevision(t, f, vol, inc, consistencyPairedMemory, buildID)

		mounts := []*orchestrator.SandboxVolumeMount{{Id: vol.ID.String(), Name: "ws", Path: "/w", Backend: volumeBackendBlock}}

		_, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, true, false)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code, "resume without memory is refused")

		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, false, false, false)
		require.NotNil(t, apiErr, "a filesystem-only snapshot needs a quiesced revision")

		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, uuid.New(), mounts, false, true, false, false)
		require.NotNil(t, apiErr, "a snapshot without recorded revisions is refused")

		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, true, true, false, false)
		require.NotNil(t, apiErr)
		assert.Contains(t, apiErr.ClientMsg, "allowSharedIdentity")

		fork, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, true, true, false, true)
		require.Nil(t, apiErr)
		assert.Equal(t, bindClone, fork.plans[0].binding)
		assert.Equal(t, dboutbox.VolumeCloneMemoryRestore, fork.plans[0].cloneMode)
		assert.Equal(t, sealed.ID, fork.plans[0].revision.ID)

		resume, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false)
		require.Nil(t, apiErr)
		require.Equal(t, bindExact, resume.plans[0].binding)

		// Another sandbox took the head (same incarnation) after the pause.
		_, err = q.CreateVolumeAttachment(ctx, queries.CreateVolumeAttachmentParams{
			ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, SandboxID: "other", ExecutionID: "e2", NodeID: "node-a", Mode: "rw", GuestPath: "/w",
		})
		require.NoError(t, err)
		_, err = q.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: "other", ExecutionID: "e2"})
		require.NoError(t, err)

		_, apiErr = resume.Attach(ctx, uuid.New(), "sbx", "e3")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
		assert.Contains(t, apiErr.ClientMsg, "fork the snapshot")
	})

	t.Run("resume of an untouched incarnation attaches it", func(t *testing.T) {
		t.Parallel()

		f := newBlockAPIFixture(t, db)
		ctx := t.Context()
		vol, inc, _ := f.readyVolume(t)
		buildID := testutils.CreateTestBuild(t, ctx, f.db, testutils.CreateTestTemplate(t, f.db, f.team.ID), "success")
		sealRevision(t, f, vol, inc, consistencyPairedMemory, buildID)

		mounts := []*orchestrator.SandboxVolumeMount{{Id: vol.ID.String(), Name: "ws", Path: "/w", Backend: volumeBackendBlock}}
		resume, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false)
		require.Nil(t, apiErr)

		bound, apiErr := resume.Attach(ctx, uuid.New(), "sbx", "e2")
		require.Nil(t, apiErr)
		require.Len(t, bound, 1)
		assert.Equal(t, inc.ID.String(), bound[0].GetIncarnationId())
		assert.EqualValues(t, sealedSlot, bound[0].GetSlot(), "a resume binds the slot the snapshot recorded, not the mount position")
	})
}

// sealRevision records a revision of inc referenced by a snapshot build, as a
// pause would.
func sealRevision(t *testing.T, f blockAPIFixture, vol queries.Volume, inc queries.VolumeIncarnation, consistency string, buildID uuid.UUID) queries.VolumeRevision {
	t.Helper()

	ctx := t.Context()
	q := f.db.SqlcClient

	op, err := q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID: uuid.New(), TeamID: f.team.ID, VolumeID: &vol.ID, Kind: "seal", IdempotencyKey: uuid.NewString(), ParamsHash: "h", Params: []byte(`{}`),
	})
	require.NoError(t, err)

	// created_at has microsecond resolution: keep the seal strictly after any
	// attachment made just before it.
	time.Sleep(2 * time.Millisecond)

	rev, err := q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, SubvolUuid: uuid.New(), FsUuid: inc.FsUuid,
		SizeBytes: inc.CapacityBytes, Consistency: consistency, OperationID: op.ID,
	})
	require.NoError(t, err)
	require.NoError(t, q.InsertSnapshotVolumeRef(ctx, queries.InsertSnapshotVolumeRefParams{
		SnapshotBuildID: buildID, RevisionID: rev.ID, VolumeID: vol.ID, MountPath: "/w", Slot: sealedSlot,
	}))

	return rev
}

// sealedSlot is the workspace slot sealRevision records; deliberately not 0
// so a test can tell the recorded slot from the mount position.
const sealedSlot = 2

// errorCode is the error_code of an error response ("" when absent).
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var body api.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())

	if body.ErrorCode == nil {
		return ""
	}

	return *body.ErrorCode
}
