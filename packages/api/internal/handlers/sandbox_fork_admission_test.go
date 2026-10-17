package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

// forkBackendStub serves a fixed sandbox record and counts checkpoints; the
// checkpoint answers with an invalid state transition so the handler stops
// before starting any fork.
type forkBackendStub struct {
	sbx         sandbox.Sandbox
	checkpoints int
}

func (s *forkBackendStub) GetSandbox(context.Context, uuid.UUID, string) (sandbox.Sandbox, error) {
	return s.sbx, nil
}

func (s *forkBackendStub) CheckpointSandbox(context.Context, uuid.UUID, string) error {
	s.checkpoints++

	return &sandbox.InvalidStateTransitionError{CurrentState: sandbox.StatePausing, TargetState: sandbox.StateRunning}
}

func postFork(t *testing.T, backend *forkBackendStub, body api.SandboxForkRequest) *httptest.ResponseRecorder {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	sandboxID := "i" + id.Generate()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/sandboxes/"+sandboxID+"/fork", bytes.NewReader(raw))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	auth.SetTeamInfoForTest(t, ginCtx, &authtypes.Team{
		Team:   &authqueries.Team{ID: uuid.New()},
		Limits: &authtypes.TeamLimits{MaxLengthHours: 24, SandboxConcurrency: 10},
	})

	store := &APIStore{forkBackendOverride: backend}
	//nolint:contextcheck // the handler reads ctx from ginCtx.Request.Context().
	store.PostSandboxesSandboxIDFork(ginCtx, sandboxID)

	return recorder
}

func blockSandbox() sandbox.Sandbox {
	return sandbox.Sandbox{
		EnvdVersion: minEnvdVersionForBlockVolumes,
		VolumeMounts: []*types.SandboxVolumeMountConfig{
			{ID: uuid.NewString(), Name: "shared", Path: "/shared"},
			{ID: uuid.NewString(), Name: "ws", Path: "/workspace", Backend: types.VolumeBackendBlock},
		},
	}
}

// A fork of a sandbox with block volumes and without allowSharedIdentity is
// refused as a whole, before the in-place checkpoint seals any revision.
func TestSandboxFork_SharedIdentityRefusedBeforeCheckpoint(t *testing.T) {
	t.Parallel()

	backend := &forkBackendStub{sbx: blockSandbox()}
	recorder := postFork(t, backend, api.SandboxForkRequest{Count: new(int32(2))})

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.Zero(t, backend.checkpoints, "no checkpoint for a refused fork")

	var apiErr api.Error
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &apiErr))
	assert.Contains(t, apiErr.Message, "allowSharedIdentity")
	require.NotNil(t, apiErr.ErrorCode)
	assert.Equal(t, errCodeSharedIdentityRequired, *apiErr.ErrorCode)

	backend = &forkBackendStub{sbx: blockSandbox()}
	recorder = postFork(t, backend, api.SandboxForkRequest{AllowSharedIdentity: new(false)})
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Zero(t, backend.checkpoints, "an explicit false is refused the same way")
}

// With allowSharedIdentity, or without block volumes, the fork is admitted
// and checkpoints.
func TestSandboxFork_AdmittedForksCheckpoint(t *testing.T) {
	t.Parallel()

	backend := &forkBackendStub{sbx: blockSandbox()}
	recorder := postFork(t, backend, api.SandboxForkRequest{AllowSharedIdentity: new(true)})
	assert.Equal(t, 1, backend.checkpoints)
	assert.Equal(t, http.StatusConflict, recorder.Code, "the stub's checkpoint refuses with a state transition")
	assert.Contains(t, recorder.Body.String(), "cannot be forked while in")

	nfsOnly := blockSandbox()
	nfsOnly.VolumeMounts = nfsOnly.VolumeMounts[:1]
	backend = &forkBackendStub{sbx: nfsOnly}
	postFork(t, backend, api.SandboxForkRequest{})
	assert.Equal(t, 1, backend.checkpoints, "sandboxes without block volumes need no allowSharedIdentity")
}
