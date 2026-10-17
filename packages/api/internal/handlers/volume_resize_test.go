package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/cfg"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

func TestVolumeResizes(t *testing.T) {
	t.Parallel()

	store := &APIStore{config: cfg.Config{BlockVolumeCapacityTiersGiB: []int64{10, 20}}}

	got, apiErr := store.volumeResizes(nil)
	require.Nil(t, apiErr)
	assert.Nil(t, got)

	got, apiErr = store.volumeResizes(&[]api.ResumeVolumeMount{{Name: "ws", CapacityGiB: 20}})
	require.Nil(t, apiErr)
	assert.Equal(t, map[string]int64{"ws": 20 << 30}, got)

	for name, mounts := range map[string][]api.ResumeVolumeMount{
		"not a tier": {{Name: "ws", CapacityGiB: 15}},
		"no name":    {{CapacityGiB: 20}},
		"duplicate":  {{Name: "ws", CapacityGiB: 20}, {Name: "ws", CapacityGiB: 20}},
	} {
		_, apiErr = store.volumeResizes(&mounts)
		require.NotNil(t, apiErr, name)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code, name)
	}
}

func TestBlockVolumeResizeOnResume(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)

	// pausedVolume is a ready 10 GiB volume whose paused execution sealed a
	// paired-memory revision referenced by a snapshot build.
	pausedVolume := func(t *testing.T) (blockAPIFixture, queries.Volume, queries.VolumeIncarnation, uuid.UUID, []*orchestrator.SandboxVolumeMount) {
		t.Helper()

		f := newBlockAPIFixture(t, db)
		vol, inc, _ := f.readyVolume(t)
		buildID := testutils.CreateTestBuild(t, t.Context(), f.db, testutils.CreateTestTemplate(t, f.db, f.team.ID), "success")
		sealRevision(t, f, vol, inc, consistencyPairedMemory, buildID)

		return f, vol, inc, buildID, []*orchestrator.SandboxVolumeMount{{Id: vol.ID.String(), Name: "ws", Path: "/w", Backend: volumeBackendBlock}}
	}

	t.Run("a grow reaches the node and is recorded once the sandbox runs", func(t *testing.T) {
		t.Parallel()

		f, vol, inc, buildID, mounts := pausedVolume(t)
		ctx := t.Context()

		resume, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false, map[string]int64{"ws": 20 << 30})
		require.Nil(t, apiErr)
		assert.Equal(t, int64(20<<30), resume.plans[0].resizeToBytes)

		bound, apiErr := resume.Attach(ctx, uuid.New(), "sbx", "e2")
		require.Nil(t, apiErr)
		require.Len(t, bound, 1)
		assert.EqualValues(t, 10<<30, bound[0].GetCapacityBytes(), "the node checks the image against the recorded size")
		assert.EqualValues(t, 20<<30, bound[0].GetResizeToBytes())

		before, err := f.db.SqlcClient.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: inc.ID, VolumeID: vol.ID})
		require.NoError(t, err)
		assert.Equal(t, int64(10<<30), before.CapacityBytes, "nothing is recorded before the start succeeds")

		resume.Started(ctx, "sbx", "e2")

		after, err := f.db.SqlcClient.GetVolumeIncarnation(ctx, queries.GetVolumeIncarnationParams{ID: inc.ID, VolumeID: vol.ID})
		require.NoError(t, err)
		assert.Equal(t, int64(20<<30), after.CapacityBytes)

		v, err := f.db.SqlcClient.GetBlockVolumeForUpdate(ctx, vol.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(20<<30), *v.CapacityBytes, "the head's growth is the volume's")
	})

	t.Run("the same capacity is a no-op", func(t *testing.T) {
		t.Parallel()

		f, _, _, buildID, mounts := pausedVolume(t)

		resume, apiErr := f.store.planResumeBlockVolumes(t.Context(), f.team.ID, buildID, mounts, false, true, false, false, map[string]int64{"ws": 10 << 30})
		require.Nil(t, apiErr)
		assert.Zero(t, resume.plans[0].resizeToBytes)
	})

	t.Run("invalid resizes are refused", func(t *testing.T) {
		t.Parallel()

		f, _, _, buildID, mounts := pausedVolume(t)
		ctx := t.Context()

		_, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false, map[string]int64{"ws": 5 << 30})
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code, "volumes only grow")
		assert.Equal(t, errCodeVolumeResizeInvalid, apiErr.ErrorCode)

		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false, map[string]int64{"other": 20 << 30})
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code, "only the snapshot's block volumes")

		nfs := []*orchestrator.SandboxVolumeMount{{Id: uuid.NewString(), Name: "shared", Path: "/s"}}
		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, nfs, false, true, false, false, map[string]int64{"shared": 20 << 30})
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code, "shared volumes are not resized")

		_, apiErr = f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, true, true, false, true, map[string]int64{"ws": 20 << 30})
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code, "forks do not resize")
	})

	t.Run("an incarnation that grew since the snapshot cannot shrink back", func(t *testing.T) {
		t.Parallel()

		f, vol, inc, buildID, mounts := pausedVolume(t)
		ctx := t.Context()
		require.NoError(t, f.db.SqlcClient.GrowVolumeIncarnationCapacity(ctx, queries.GrowVolumeIncarnationCapacityParams{ID: inc.ID, VolumeID: vol.ID, CapacityBytes: 30 << 30}))

		resume, apiErr := f.store.planResumeBlockVolumes(ctx, f.team.ID, buildID, mounts, false, true, false, false, map[string]int64{"ws": 20 << 30})
		require.Nil(t, apiErr)

		_, apiErr = resume.Attach(ctx, uuid.New(), "sbx", "e2")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
		assert.Equal(t, errCodeVolumeResizeInvalid, apiErr.ErrorCode)
	})
}

// A connect that asks to resize volumes of a running sandbox is refused
// before its timeout is extended.
func TestConnect_ResizeOfRunningSandboxRefused(t *testing.T) {
	t.Parallel()

	teamID := uuid.New()
	sandboxID := "i" + id.Generate()
	stub := &connectStub{
		sbx:    sandbox.Sandbox{SandboxID: sandboxID, TeamID: teamID, State: sandbox.StateRunning, StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)},
		waited: true,
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/sandboxes/"+sandboxID+"/connect",
		strings.NewReader(`{"timeout":60,"volumeMounts":[{"name":"ws","capacityGiB":20}]}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	auth.SetTeamInfoForTest(t, ginCtx, &authtypes.Team{
		Team:   &authqueries.Team{ID: teamID, Slug: "test-team"},
		Limits: &authtypes.TeamLimits{MaxLengthHours: 24},
	})

	store := &APIStore{connectBackendOverride: stub, config: cfg.Config{BlockVolumeCapacityTiersGiB: []int64{10, 20}}}
	//nolint:contextcheck // the handler reads ctx from ginCtx.Request.Context().
	store.PostSandboxesSandboxIDConnect(ginCtx, sandboxID)

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.Equal(t, errCodeVolumeResizeInvalid, errorCode(t, recorder))
	assert.Zero(t, stub.keepAlives, "the timeout is not extended")
}
