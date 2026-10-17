package volumes

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
)

type blockFixture struct {
	db     *testutils.Database
	q      *queries.Queries
	teamID uuid.UUID
}

func newBlockFixture(t *testing.T) blockFixture {
	t.Helper()

	db := testutils.SetupDatabase(t)

	return blockFixture{db: db, q: db.SqlcClient.Queries, teamID: testutils.CreateTestTeam(t, db)}
}

func (f blockFixture) createVolume(t *testing.T) queries.Volume {
	t.Helper()

	vol, err := f.q.CreateBlockVolume(t.Context(), queries.CreateBlockVolumeParams{
		ID:            uuid.New(),
		TeamID:        f.teamID,
		VolumeType:    "block",
		Name:          "ws",
		OwnerNodeID:   new("node-a"),
		CapacityBytes: new(int64(10 << 30)),
	})
	require.NoError(t, err)

	return vol
}

func (f blockFixture) createOperation(t *testing.T, volumeID uuid.UUID, kind, key string) queries.VolumeOperation {
	t.Helper()

	op, err := f.q.CreateVolumeOperation(t.Context(), queries.CreateVolumeOperationParams{
		ID:             uuid.New(),
		TeamID:         f.teamID,
		VolumeID:       &volumeID,
		Kind:           kind,
		IdempotencyKey: key,
		ParamsHash:     "h-" + key,
		Params:         json.RawMessage(`{"k":"` + key + `"}`),
	})
	require.NoError(t, err)

	return op
}

// materialise records what a successful volume.create leaves behind: the
// head incarnation and its revision 0.
func (f blockFixture) materialise(t *testing.T, vol queries.Volume, op queries.VolumeOperation) (queries.VolumeIncarnation, queries.VolumeRevision) {
	t.Helper()
	ctx := t.Context()

	inc, err := f.q.InsertVolumeIncarnation(ctx, queries.InsertVolumeIncarnationParams{
		ID:            uuid.New(),
		VolumeID:      vol.ID,
		NodeID:        "node-a",
		SubvolPath:    "teams/t/v/inc/i",
		SubvolUuid:    uuid.New(),
		FsUuid:        uuid.New(),
		CapacityBytes: 10 << 30,
		OperationID:   op.ID,
	})
	require.NoError(t, err)

	rev, err := f.q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID:            uuid.New(),
		VolumeID:      vol.ID,
		IncarnationID: inc.ID,
		SubvolUuid:    uuid.New(),
		FsUuid:        inc.FsUuid,
		SizeBytes:     10 << 30,
		Consistency:   "filesystem-quiesced",
		OperationID:   op.ID,
	})
	require.NoError(t, err)

	require.NoError(t, f.q.SetBlockVolumeReady(ctx, queries.SetBlockVolumeReadyParams{VolumeID: vol.ID, HeadIncarnationID: &inc.ID}))

	return inc, rev
}

func (f blockFixture) attach(t *testing.T, vol queries.Volume, inc queries.VolumeIncarnation, sandboxID, executionID string) (queries.VolumeAttachment, error) {
	t.Helper()

	return f.q.CreateVolumeAttachment(t.Context(), queries.CreateVolumeAttachmentParams{
		ID:            uuid.New(),
		VolumeID:      vol.ID,
		IncarnationID: inc.ID,
		SandboxID:     sandboxID,
		ExecutionID:   executionID,
		NodeID:        "node-a",
		Mode:          "rw",
		GuestPath:     "/workspace",
	})
}

func TestBlockVolumes_CreateAndLifecycle(t *testing.T) {
	t.Parallel()

	f := newBlockFixture(t)
	ctx := t.Context()

	vol := f.createVolume(t)
	assert.Equal(t, "block", vol.Backend)
	assert.Equal(t, "creating", vol.Lifecycle)
	assert.Nil(t, vol.HeadIncarnationID)

	// A creating volume is visible to its team (the API reports it as pending).
	got, err := f.q.GetVolume(ctx, queries.GetVolumeParams{VolumeID: vol.ID, TeamID: f.teamID})
	require.NoError(t, err)
	assert.Equal(t, vol.ID, got.ID)

	op := f.createOperation(t, vol.ID, "create", vol.ID.String())
	inc, rev := f.materialise(t, vol, op)

	ready, err := f.q.GetBlockVolumeForUpdate(ctx, vol.ID)
	require.NoError(t, err)
	assert.Equal(t, "ready", ready.Lifecycle)
	require.NotNil(t, ready.HeadIncarnationID)
	assert.Equal(t, inc.ID, *ready.HeadIncarnationID)

	latest, err := f.q.GetLatestIncarnationRevision(ctx, inc.ID)
	require.NoError(t, err)
	assert.Equal(t, rev.ID, latest.ID)

	revs, err := f.q.ListVolumeRevisions(ctx, vol.ID)
	require.NoError(t, err)
	require.Len(t, revs, 1)

	// Replaying the materialisation is a no-op rather than a duplicate.
	_, err = f.q.InsertVolumeIncarnation(ctx, queries.InsertVolumeIncarnationParams{
		ID: inc.ID, VolumeID: vol.ID, NodeID: "node-a", SubvolPath: "x",
		SubvolUuid: uuid.New(), FsUuid: uuid.New(), CapacityBytes: 1, OperationID: op.ID,
	})
	require.True(t, dberrors.IsNotFoundError(err), "ON CONFLICT DO NOTHING returns no row: %v", err)

	// Block shape is enforced by the schema.
	_, err = f.q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
		ID: uuid.New(), TeamID: f.teamID, VolumeType: "block", Name: "no-node", CapacityBytes: new(int64(1)),
	})
	require.True(t, dberrors.IsCheckViolation(err), "%v", err)
}

func TestBlockVolumes_OperationIdempotency(t *testing.T) {
	t.Parallel()

	f := newBlockFixture(t)
	ctx := t.Context()
	vol := f.createVolume(t)

	op := f.createOperation(t, vol.ID, "create", "key-1")
	assert.Equal(t, "pending", op.State)

	_, err := f.q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID: uuid.New(), TeamID: f.teamID, VolumeID: &vol.ID, Kind: "create",
		IdempotencyKey: "key-1", ParamsHash: "other", Params: json.RawMessage(`{}`),
	})
	require.True(t, dberrors.IsNotFoundError(err), "a replayed key inserts nothing")

	existing, err := f.q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: "create", IdempotencyKey: "key-1"})
	require.NoError(t, err)
	assert.Equal(t, op.ID, existing.ID)
	assert.JSONEq(t, `{"k":"key-1"}`, string(existing.Params))

	// The same key under another kind is a different operation.
	other := f.createOperation(t, vol.ID, "delete", "key-1")
	assert.NotEqual(t, op.ID, other.ID)

	require.NoError(t, f.q.MarkVolumeOperationRunning(ctx, op.ID))
	require.NoError(t, f.q.MarkVolumeOperationUnknown(ctx, queries.MarkVolumeOperationUnknownParams{ID: op.ID, Error: new("deadline exceeded")}))
	require.NoError(t, f.q.MarkVolumeOperationRunning(ctx, op.ID))
	require.NoError(t, f.q.CompleteVolumeOperation(ctx, queries.CompleteVolumeOperationParams{ID: op.ID, Result: json.RawMessage(`{"ok":true}`)}))

	done, err := f.q.GetVolumeOperation(ctx, queries.GetVolumeOperationParams{ID: op.ID, TeamID: f.teamID})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", done.State)
	assert.EqualValues(t, 2, done.Attempts)
	assert.Nil(t, done.Error)
	assert.JSONEq(t, `{"ok":true}`, string(done.Result))
	assert.True(t, done.FinishedAt.Valid)

	// A finished operation is terminal.
	require.NoError(t, f.q.FailVolumeOperation(ctx, queries.FailVolumeOperationParams{ID: op.ID, Error: new("late")}))
	done, err = f.q.GetVolumeOperation(ctx, queries.GetVolumeOperationParams{ID: op.ID, TeamID: f.teamID})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", done.State)

	// Operations are team-scoped on read.
	_, err = f.q.GetVolumeOperation(ctx, queries.GetVolumeOperationParams{ID: op.ID, TeamID: uuid.New()})
	require.True(t, dberrors.IsNotFoundError(err))
}

func TestBlockVolumes_SingleWriter(t *testing.T) {
	t.Parallel()

	f := newBlockFixture(t)
	ctx := t.Context()
	vol := f.createVolume(t)
	inc, rev := f.materialise(t, vol, f.createOperation(t, vol.ID, "create", vol.ID.String()))

	first, err := f.attach(t, vol, inc, "sbx-1", "exec-1")
	require.NoError(t, err)
	assert.Equal(t, "attaching", first.DesiredState)

	_, err = f.attach(t, vol, inc, "sbx-2", "exec-2")
	require.True(t, dberrors.IsUniqueConstraintViolation(err), "a second writer is refused: %v", err)

	writer, err := f.q.GetActiveIncarnationWriter(ctx, inc.ID)
	require.NoError(t, err)
	assert.Equal(t, "sbx-1", writer.SandboxID)

	count, err := f.q.CountActiveVolumeAttachments(ctx, vol.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	attached, err := f.q.IncarnationAttachedSince(ctx, queries.IncarnationAttachedSinceParams{IncarnationID: inc.ID, Since: rev.CreatedAt})
	require.NoError(t, err)
	assert.True(t, attached, "the attachment was created after revision 0")

	released, err := f.q.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: "sbx-1", ExecutionID: "exec-1"})
	require.NoError(t, err)
	require.Len(t, released, 1)
	assert.Equal(t, "released", released[0].DesiredState)

	again, err := f.q.ReleaseSandboxAttachments(ctx, queries.ReleaseSandboxAttachmentsParams{SandboxID: "sbx-1", ExecutionID: "exec-1"})
	require.NoError(t, err)
	assert.Empty(t, again, "release is idempotent")

	_, err = f.attach(t, vol, inc, "sbx-2", "exec-2")
	require.NoError(t, err, "the incarnation is free once the writer released it")

	active, err := f.q.ListActiveSandboxAttachments(ctx, "sbx-2")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NoError(t, f.q.ReleaseVolumeAttachment(ctx, active[0].ID))

	attached, err = f.q.IncarnationAttachedSince(ctx, queries.IncarnationAttachedSinceParams{
		IncarnationID: inc.ID, Since: pgtype.Timestamptz{Time: active[0].CreatedAt.Time, Valid: true},
	})
	require.NoError(t, err)
	assert.False(t, attached)
}

func TestBlockVolumes_SnapshotRefsAndTombstone(t *testing.T) {
	t.Parallel()

	f := newBlockFixture(t)
	ctx := t.Context()
	vol := f.createVolume(t)
	_, rev := f.materialise(t, vol, f.createOperation(t, vol.ID, "create", vol.ID.String()))

	templateID := testutils.CreateTestTemplate(t, f.db, f.teamID)
	buildID := testutils.CreateTestBuild(t, ctx, f.db, templateID, "success")

	ref := queries.InsertSnapshotVolumeRefParams{SnapshotBuildID: buildID, RevisionID: rev.ID, VolumeID: vol.ID, MountPath: "/workspace"}
	require.NoError(t, f.q.InsertSnapshotVolumeRef(ctx, ref))
	require.NoError(t, f.q.InsertSnapshotVolumeRef(ctx, ref), "recording a ref twice is a no-op")

	refs, err := f.q.ListSnapshotVolumeRefs(ctx, buildID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, rev.ID, refs[0].VolumeRevision.ID)
	assert.Equal(t, vol.ID, refs[0].Volume.ID)
	assert.Equal(t, "/workspace", refs[0].MountPath)

	n, err := f.q.CountVolumeSnapshotRefs(ctx, vol.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	// A hard delete never removes a block volume row.
	require.NoError(t, f.q.DeleteVolume(ctx, queries.DeleteVolumeParams{TeamID: f.teamID, VolumeID: vol.ID}))
	_, err = f.q.GetVolume(ctx, queries.GetVolumeParams{VolumeID: vol.ID, TeamID: f.teamID})
	require.NoError(t, err)

	// Deleting the snapshot build drops the reference with it.
	require.NoError(t, f.db.SqlcClient.TestsRawSQL(ctx, `DELETE FROM env_builds WHERE id = $1`, buildID))
	n, err = f.q.CountVolumeSnapshotRefs(ctx, vol.ID)
	require.NoError(t, err)
	assert.Zero(t, n)

	deleting, err := f.q.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: vol.ID, TeamID: f.teamID})
	require.NoError(t, err)
	assert.Equal(t, "deleting", deleting.Lifecycle)

	_, err = f.q.MarkBlockVolumeDeleting(ctx, queries.MarkBlockVolumeDeletingParams{VolumeID: vol.ID, TeamID: f.teamID})
	require.True(t, dberrors.IsNotFoundError(err), "a deleting volume is not deletable again")

	// The name stays reserved while the volume is deleting.
	_, err = f.q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
		ID: uuid.New(), TeamID: f.teamID, VolumeType: "block", Name: "ws",
		OwnerNodeID: new("node-a"), CapacityBytes: new(int64(1)),
	})
	require.True(t, dberrors.IsUniqueConstraintViolation(err))

	require.NoError(t, f.q.TombstoneBlockVolume(ctx, vol.ID))

	_, err = f.q.GetVolume(ctx, queries.GetVolumeParams{VolumeID: vol.ID, TeamID: f.teamID})
	require.True(t, dberrors.IsNotFoundError(err), "a tombstone is invisible")

	list, err := f.q.FindVolumesByTeamID(ctx, f.teamID)
	require.NoError(t, err)
	assert.Empty(t, list)

	byName, err := f.q.GetVolumesByName(ctx, queries.GetVolumesByNameParams{TeamID: f.teamID, VolumeNames: []string{"ws"}})
	require.NoError(t, err)
	assert.Empty(t, byName)

	// ...and frees the name.
	reused := f.createVolume(t)
	assert.NotEqual(t, vol.ID, reused.ID)
}

func TestBlockVolumes_RevisionsReferencedBySnapshotsCannotBeRemoved(t *testing.T) {
	t.Parallel()

	f := newBlockFixture(t)
	ctx := t.Context()
	vol := f.createVolume(t)
	_, rev := f.materialise(t, vol, f.createOperation(t, vol.ID, "create", vol.ID.String()))

	templateID := testutils.CreateTestTemplate(t, f.db, f.teamID)
	buildID := testutils.CreateTestBuild(t, ctx, f.db, templateID, "success")
	require.NoError(t, f.q.InsertSnapshotVolumeRef(ctx, queries.InsertSnapshotVolumeRefParams{
		SnapshotBuildID: buildID, RevisionID: rev.ID, VolumeID: vol.ID, MountPath: "/workspace",
	}))

	err := f.db.SqlcClient.TestsRawSQL(ctx, `DELETE FROM volume_revisions WHERE id = $1`, rev.ID)
	require.True(t, dberrors.IsForeignKeyViolation(err), "%v", err)
}
