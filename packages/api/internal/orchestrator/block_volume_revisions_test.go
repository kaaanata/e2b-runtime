package orchestrator

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
)

func TestRecordSealedWorkspaceRevisions(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	ctx := t.Context()
	q := db.SqlcClient
	teamID := testutils.CreateTestTeam(t, db)

	vol, err := q.CreateBlockVolume(ctx, queries.CreateBlockVolumeParams{
		ID: uuid.New(), TeamID: teamID, VolumeType: "block", Name: "ws",
		OwnerNodeID: new("node-a"), CapacityBytes: new(int64(1 << 30)),
	})
	require.NoError(t, err)

	createOp, err := q.CreateVolumeOperation(ctx, queries.CreateVolumeOperationParams{
		ID: uuid.New(), TeamID: teamID, VolumeID: &vol.ID, Kind: "create", IdempotencyKey: vol.ID.String(), ParamsHash: "h", Params: []byte(`{}`),
	})
	require.NoError(t, err)

	inc, err := q.InsertVolumeIncarnation(ctx, queries.InsertVolumeIncarnationParams{
		ID: uuid.New(), VolumeID: vol.ID, NodeID: "node-a", SubvolPath: "inc", SubvolUuid: uuid.New(),
		FsUuid: uuid.New(), CapacityBytes: 1 << 30, OperationID: createOp.ID,
	})
	require.NoError(t, err)

	rev0, err := q.InsertVolumeRevision(ctx, queries.InsertVolumeRevisionParams{
		ID: uuid.New(), VolumeID: vol.ID, IncarnationID: inc.ID, SubvolUuid: uuid.New(), FsUuid: inc.FsUuid,
		SizeBytes: 1 << 30, Consistency: "filesystem-quiesced", OperationID: createOp.ID,
	})
	require.NoError(t, err)

	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "success")

	sealed := SealedWorkspaceRevision{
		VolumeID: vol.ID, IncarnationID: inc.ID, RevisionID: uuid.New(), SubvolUUID: uuid.New(), FsUUID: inc.FsUuid,
		SizeBytes: 1 << 30, Consistency: "paired-memory", MountPath: "/workspace",
	}

	require.NoError(t, RecordSealedWorkspaceRevisions(ctx, q, teamID, buildID, []SealedWorkspaceRevision{sealed}))
	require.NoError(t, RecordSealedWorkspaceRevisions(ctx, q, teamID, buildID, []SealedWorkspaceRevision{sealed}), "a repeat is a no-op")

	refs, err := q.ListSnapshotVolumeRefs(ctx, buildID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, sealed.RevisionID, refs[0].VolumeRevision.ID)
	assert.Equal(t, "paired-memory", refs[0].VolumeRevision.Consistency)
	require.NotNil(t, refs[0].VolumeRevision.ParentID)
	assert.Equal(t, rev0.ID, *refs[0].VolumeRevision.ParentID)

	op, err := q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: "seal", IdempotencyKey: "sealed:" + sealed.RevisionID.String()})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", op.State)

	err = RecordSealedWorkspaceRevisions(ctx, q, uuid.New(), buildID, []SealedWorkspaceRevision{sealed})
	require.Error(t, err, "another team's volume is refused")
}
