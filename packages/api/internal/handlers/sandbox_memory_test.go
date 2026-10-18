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
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

func TestValidateSandboxMemory(t *testing.T) {
	t.Parallel()

	limits := &authtypes.TeamLimits{MaxRamMb: 65536}
	maxMB := new(int64(65536))

	tests := map[string]struct {
		maxRamMB  *int64
		requested int64
		wantCode  int
		wantErr   string
	}{
		"fixed template at its size":        {nil, 8192, 0, ""},
		"fixed template other size":         {nil, 16384, http.StatusConflict, orchestrator.ErrCodeMemoryNotHotResizable},
		"hotplug at boot memory":            {maxMB, 8192, 0, ""},
		"hotplug at maximum":                {maxMB, 65536, 0, ""},
		"hotplug in between":                {maxMB, 16384, 0, ""},
		"below boot memory":                 {maxMB, 4096, http.StatusBadRequest, ""},
		"above maximum":                     {maxMB, 65536 + 2, http.StatusBadRequest, ""},
		"above the tier":                    {new(int64(131072)), 100000, http.StatusBadRequest, ""},
		"hotplugged part not block aligned": {maxMB, 8193, http.StatusBadRequest, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			apiErr := validateSandboxMemory(limits, 8192, tc.maxRamMB, tc.requested)
			if tc.wantCode == 0 {
				assert.Nil(t, apiErr)

				return
			}

			require.NotNil(t, apiErr)
			assert.Equal(t, tc.wantCode, apiErr.Code)
			assert.Equal(t, tc.wantErr, apiErr.ErrorCode)
		})
	}

	assert.Nil(t, validateSandboxMemory(nil, 8192, maxMB, 65536), "no limits: no tier check")
}

func TestMemoryDefaults(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(8192), requestedOrDefault(nil, 8192))
	assert.Equal(t, int64(16384), requestedOrDefault(new(api.MemoryMB(16384)), 8192))

	assert.Equal(t, int64(8192), snapshotMemoryDefault(8192, nil), "a snapshot predating hotplug resumes at boot memory")
	assert.Equal(t, int64(32768), snapshotMemoryDefault(8192, new(int64(32768))))

	memoryMB, maxMemoryMB := pausedMemory(8192, new(int64(65536)), new(int64(32768)))
	assert.Equal(t, api.MemoryMB(32768), memoryMB)
	require.NotNil(t, maxMemoryMB)
	assert.Equal(t, api.MaxMemoryMB(65536), *maxMemoryMB)

	memoryMB, maxMemoryMB = pausedMemory(4096, nil, nil)
	assert.Equal(t, api.MemoryMB(4096), memoryMB)
	assert.Nil(t, maxMemoryMB)
}

// resizeBackendStub answers GetSandbox with a fixed record and records the
// resize it is asked for; resizeErr, when set, is what the node answered.
type resizeBackendStub struct {
	sbx       sandbox.Sandbox
	getErr    error
	resizeErr *api.APIError
	resized   []int64
}

func (s *resizeBackendStub) GetSandbox(context.Context, uuid.UUID, string) (sandbox.Sandbox, error) {
	return s.sbx, s.getErr
}

func (s *resizeBackendStub) ResizeSandboxMemory(_ context.Context, _ uuid.UUID, _ string, memoryMB int64) (sandbox.Sandbox, *api.APIError) {
	s.resized = append(s.resized, memoryMB)
	if s.resizeErr != nil {
		return sandbox.Sandbox{}, s.resizeErr
	}

	// The node reached the request: a grow completes within a second.
	updated := s.sbx
	updated.MemoryMB = memoryMB
	updated.CurrentMemoryMB = memoryMB

	return updated, nil
}

func patchSandbox(t *testing.T, backend *resizeBackendStub, teamID uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	sandboxID := backend.sbx.SandboxID
	if sandboxID == "" {
		sandboxID = "i" + id.Generate()
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/sandboxes/"+sandboxID, bytes.NewReader(raw))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	auth.SetTeamInfoForTest(t, ginCtx, &authtypes.Team{
		Team:   &authqueries.Team{ID: teamID},
		Limits: &authtypes.TeamLimits{MaxLengthHours: 24, MaxRamMb: 65536},
	})

	store := &APIStore{resizeBackendOverride: backend}
	//nolint:contextcheck // the handler reads ctx from ginCtx.Request.Context().
	store.PatchSandboxesSandboxID(ginCtx, sandboxID)

	return recorder
}

func hotplugSandbox(teamID uuid.UUID) sandbox.Sandbox {
	return sandbox.Sandbox{
		SandboxID:       "i" + id.Generate(),
		TeamID:          teamID,
		State:           sandbox.StateRunning,
		VCpu:            8,
		RamMB:           8192,
		MemoryMB:        8192,
		CurrentMemoryMB: 8192,
		MaxMemoryMB:     new(int64(65536)),
		EnvdVersion:     "0.10.0",
	}
}

func TestPatchSandboxesSandboxID(t *testing.T) {
	t.Parallel()

	teamID := uuid.New()

	t.Run("grows a hot-resizable sandbox", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: hotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(32768))})

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []int64{32768}, backend.resized)

		var detail api.SandboxDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		assert.Equal(t, api.MemoryMB(32768), detail.MemoryMB)
		require.NotNil(t, detail.CurrentMemoryMB)
		assert.Equal(t, api.MemoryMB(32768), *detail.CurrentMemoryMB)
		require.NotNil(t, detail.MaxMemoryMB)
		assert.Equal(t, api.MaxMemoryMB(65536), *detail.MaxMemoryMB)
		assert.Equal(t, api.Running, detail.State)
	})

	t.Run("fixed-size template is refused with 409", func(t *testing.T) {
		t.Parallel()

		sbx := hotplugSandbox(teamID)
		sbx.MaxMemoryMB = nil
		backend := &resizeBackendStub{sbx: sbx}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusConflict, rec.Code)
		var apiErr api.Error
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
		require.NotNil(t, apiErr.ErrorCode)
		assert.Equal(t, orchestrator.ErrCodeMemoryNotHotResizable, *apiErr.ErrorCode)
		assert.Empty(t, backend.resized, "nothing was sent to the node")
	})

	t.Run("above the template's maximum is 400", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: hotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(65536 + 128))})

		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Empty(t, backend.resized)
	})

	t.Run("missing memoryMB is 400", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: hotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{})

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("paused sandbox is 409", func(t *testing.T) {
		t.Parallel()

		sbx := hotplugSandbox(teamID)
		sbx.State = sandbox.StatePausing
		backend := &resizeBackendStub{sbx: sbx}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Empty(t, backend.resized)
	})

	t.Run("unknown sandbox is 404", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: sandbox.Sandbox{SandboxID: "i" + id.Generate()}, getErr: sandbox.ErrNotFound}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("node refusal is passed through", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{
			sbx:       hotplugSandbox(teamID),
			resizeErr: &api.APIError{Code: http.StatusConflict, ErrorCode: orchestrator.ErrCodeMemoryResizeBusy, ClientMsg: "busy"},
		}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusConflict, rec.Code)
		var apiErr api.Error
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
		require.NotNil(t, apiErr.ErrorCode)
		assert.Equal(t, orchestrator.ErrCodeMemoryResizeBusy, *apiErr.ErrorCode)
	})
}
