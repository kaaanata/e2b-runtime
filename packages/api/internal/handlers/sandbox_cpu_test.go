package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
)

func TestValidateSandboxCPU(t *testing.T) {
	t.Parallel()

	limits := &authtypes.TeamLimits{MaxVcpu: 16}
	maxVCPU := new(int64(32))

	tests := map[string]struct {
		max       *int64
		requested int64
		wantCode  int
		wantError string
	}{
		"fixed at its count":       {requested: 8},
		"fixed, another count":     {requested: 16, wantCode: http.StatusConflict, wantError: orchestrator.ErrCodeCPUNotHotResizable},
		"hot, the base":            {max: maxVCPU, requested: 8},
		"hot, up to the tier":      {max: maxVCPU, requested: 16},
		"hot, odd counts are fine": {max: maxVCPU, requested: 11},
		"hot, below the base":      {max: maxVCPU, requested: 4, wantCode: http.StatusBadRequest},
		"hot, above the tier":      {max: maxVCPU, requested: 20, wantCode: http.StatusBadRequest},
		"hot, above the maximum":   {max: new(int64(12)), requested: 14, wantCode: http.StatusBadRequest},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			apiErr := validateSandboxCPU(limits, 8, tc.max, tc.requested)
			if tc.wantCode == 0 {
				assert.Nil(t, apiErr)

				return
			}

			require.NotNil(t, apiErr)
			assert.Equal(t, tc.wantCode, apiErr.Code)
			assert.Equal(t, tc.wantError, apiErr.ErrorCode)
		})
	}

	assert.Nil(t, validateSandboxCPU(nil, 8, maxVCPU, 32), "no limits (internal callers): only the template bounds")
}

func TestCPUDefaults(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(8), cpuRequestedOrDefault(nil, 8))
	assert.Equal(t, int64(12), cpuRequestedOrDefault(new(api.CPUCount(12)), 8))
	assert.Equal(t, int64(8), snapshotCPUDefault(8, nil))
	assert.Equal(t, int64(8), snapshotCPUDefault(8, new(int64(0))))
	assert.Equal(t, int64(16), snapshotCPUDefault(8, new(int64(16))))

	count, maxCount := pausedCPU(8, new(int64(32)), new(int64(16)))
	assert.Equal(t, api.CPUCount(16), count)
	require.NotNil(t, maxCount)
	assert.Equal(t, api.MaxCPUCount(32), *maxCount)

	count, maxCount = pausedCPU(4, nil, nil)
	assert.Equal(t, api.CPUCount(4), count)
	assert.Nil(t, maxCount)
}

func cpuHotplugSandbox(teamID uuid.UUID) sandbox.Sandbox {
	sbx := hotplugSandbox(teamID)
	sbx.CPUCount = 8
	sbx.CurrentCPUCount = 8
	sbx.MaxCPUCount = new(int64(32))

	return sbx
}

func TestPatchSandboxesSandboxID_CPU(t *testing.T) {
	t.Parallel()

	teamID := uuid.New()

	t.Run("grows the vCPU count", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: cpuHotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{CpuCount: new(api.CPUCount(16))})

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []int64{16}, backend.resizedCPU)
		assert.Empty(t, backend.resized, "memory untouched")

		var detail api.SandboxDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		assert.Equal(t, api.CPUCount(16), detail.CpuCount)
		require.NotNil(t, detail.CurrentCpuCount)
		assert.Equal(t, api.CPUCount(16), *detail.CurrentCpuCount)
		require.NotNil(t, detail.MaxCpuCount)
		assert.Equal(t, api.MaxCPUCount(32), *detail.MaxCpuCount)
	})

	t.Run("memory and vCPUs in one call, memory first", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: cpuHotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{CpuCount: new(api.CPUCount(12)), MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"memory", "cpu"}, backend.order)

		var detail api.SandboxDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		assert.Equal(t, api.CPUCount(12), detail.CpuCount)
		assert.Equal(t, api.MemoryMB(16384), detail.MemoryMB)
	})

	t.Run("fixed vCPU count is 409 cpu_not_hot_resizable", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: hotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{CpuCount: new(api.CPUCount(16))})

		require.Equal(t, http.StatusConflict, rec.Code)
		var apiErr api.Error
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
		require.NotNil(t, apiErr.ErrorCode)
		assert.Equal(t, orchestrator.ErrCodeCPUNotHotResizable, *apiErr.ErrorCode)
		assert.Empty(t, backend.resizedCPU)
	})

	t.Run("an invalid vCPU count rejects the memory part too", func(t *testing.T) {
		t.Parallel()

		backend := &resizeBackendStub{sbx: cpuHotplugSandbox(teamID)}
		rec := patchSandbox(t, backend, teamID, api.SandboxUpdate{CpuCount: new(api.CPUCount(20)), MemoryMB: new(api.MemoryMB(16384))})

		require.Equal(t, http.StatusBadRequest, rec.Code, "above the tier's 16")
		assert.Empty(t, backend.order, "validated before anything is applied")
	})

	t.Run("a fixed sandbox renders no CPU hot resize fields", func(t *testing.T) {
		t.Parallel()

		detail := runningSandboxDetail(hotplugSandbox(teamID), nil)
		assert.Equal(t, api.CPUCount(8), detail.CpuCount)
		assert.Nil(t, detail.CurrentCpuCount)
		assert.Nil(t, detail.MaxCpuCount)
	})
}
