package orchestrator

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
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

	// What the node reports for the pause (SandboxPauseResponse field 20),
	// converted the way pauseSandbox does it.
	reported := &orchestrator.SandboxWorkspaceRevision{
		Slot: 3, VolumeId: vol.ID.String(), SourceIncarnationId: inc.ID.String(), RevisionId: uuid.NewString(),
		OperationId: uuid.NewString(), SubvolumeUuid: uuid.NewString(), FsUuid: inc.FsUuid.String(),
		SizeBytes: 1 << 30, Consistency: "paired-memory",
	}
	converted, err := sealedRevisionsFromPause([]*orchestrator.SandboxWorkspaceRevision{reported}, []*types.SandboxVolumeMountConfig{
		{ID: uuid.NewString(), Path: "/shared", Type: "local"},
		{ID: vol.ID.String(), Path: "/workspace", Type: "block", Backend: types.VolumeBackendBlock},
	})
	require.NoError(t, err)
	require.Len(t, converted, 1)
	sealed := converted[0]
	assert.Equal(t, "/workspace", sealed.MountPath)
	assert.EqualValues(t, 3, sealed.Slot)

	require.NoError(t, RecordSealedWorkspaceRevisions(ctx, q, teamID, buildID, []SealedWorkspaceRevision{sealed}))
	require.NoError(t, RecordSealedWorkspaceRevisions(ctx, q, teamID, buildID, []SealedWorkspaceRevision{sealed}), "a repeat is a no-op")

	refs, err := q.ListSnapshotVolumeRefs(ctx, buildID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, sealed.RevisionID, refs[0].VolumeRevision.ID)
	assert.EqualValues(t, 3, refs[0].Slot, "resume and fork bind the slot the pause sealed")
	assert.Equal(t, "/workspace", refs[0].MountPath)
	assert.Equal(t, "paired-memory", refs[0].VolumeRevision.Consistency)
	require.NotNil(t, refs[0].VolumeRevision.ParentID)
	assert.Equal(t, rev0.ID, *refs[0].VolumeRevision.ParentID)

	op, err := q.GetVolumeOperationByKey(ctx, queries.GetVolumeOperationByKeyParams{Kind: "seal", IdempotencyKey: "sealed:" + sealed.RevisionID.String()})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", op.State)

	err = RecordSealedWorkspaceRevisions(ctx, q, uuid.New(), buildID, []SealedWorkspaceRevision{sealed})
	require.Error(t, err, "another team's volume is refused")
}

func TestSealedRevisionsFromPause_Refusals(t *testing.T) {
	t.Parallel()

	vol := uuid.NewString()
	mounts := []*types.SandboxVolumeMountConfig{{ID: vol, Path: "/w", Backend: types.VolumeBackendBlock}}
	valid := func() *orchestrator.SandboxWorkspaceRevision {
		return &orchestrator.SandboxWorkspaceRevision{
			VolumeId: vol, SourceIncarnationId: uuid.NewString(), RevisionId: uuid.NewString(),
			SubvolumeUuid: uuid.NewString(), FsUuid: uuid.NewString(), SizeBytes: 1 << 30, Consistency: "paired-memory",
		}
	}

	got, err := sealedRevisionsFromPause(nil, mounts)
	require.NoError(t, err)
	assert.Nil(t, got, "a pause without workspace volumes records nothing")

	_, err = sealedRevisionsFromPause([]*orchestrator.SandboxWorkspaceRevision{valid()}, nil)
	require.Error(t, err, "a revision of a volume the sandbox does not mount")

	nfsOnly := []*types.SandboxVolumeMountConfig{{ID: vol, Path: "/w"}}
	_, err = sealedRevisionsFromPause([]*orchestrator.SandboxWorkspaceRevision{valid()}, nfsOnly)
	require.Error(t, err, "an NFS mount of the same id is not a block mount")

	bad := valid()
	bad.FsUuid = "not-a-uuid"
	_, err = sealedRevisionsFromPause([]*orchestrator.SandboxWorkspaceRevision{bad}, mounts)
	require.Error(t, err)
}

func TestWorkspaceSlotTransport(t *testing.T) {
	t.Parallel()

	mounts := []*orchestrator.SandboxVolumeMount{
		{Id: "nfs", Path: "/shared", Type: "local", ConsistencyProfile: "cto"},
		{Id: "ws", Path: "/workspace", Type: "block", Backend: types.VolumeBackendBlock},
	}
	slots := []*orchestrator.SandboxWorkspaceSlot{{Slot: 0, VolumeId: "ws", IncarnationId: "inc", MountPath: "/workspace", FsUuid: "fs", CapacityBytes: 1 << 30}}

	sent := nfsVolumeMounts(mounts)
	require.Len(t, sent, 1, "block mounts never travel in volume_mounts")
	assert.Equal(t, "nfs", sent[0].GetId())

	configs := sandboxMountConfigs(mounts, slots)
	require.Len(t, configs, 2, "the sandbox record keeps every mount")
	assert.Empty(t, configs[0].IncarnationID)
	assert.True(t, configs[1].IsBlock())
	assert.Equal(t, "inc", configs[1].IncarnationID)
	assert.Equal(t, "fs", configs[1].FsUUID)
	assert.EqualValues(t, 1<<30, configs[1].CapacityBytes)
}
