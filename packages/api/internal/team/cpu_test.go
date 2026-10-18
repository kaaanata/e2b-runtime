package team

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

func TestLimitMaxCPU(t *testing.T) {
	t.Parallel()

	limits := &types.TeamLimits{MaxVcpu: 16}
	i32 := func(v int32) *int32 { return &v }

	tests := map[string]struct {
		cpu     int64
		max     *int32
		want    *int64
		wantErr bool
	}{
		"unset is fixed":          {cpu: 8},
		"equal to count is fixed": {cpu: 8, max: i32(8)},
		"up to the tier":          {cpu: 8, max: i32(16), want: new(int64(16))},
		"below count":             {cpu: 8, max: i32(4), wantErr: true},
		"above the tier":          {cpu: 8, max: i32(17), wantErr: true},
		"above firecracker":       {cpu: 8, max: i32(48), wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, apiErr := LimitMaxCPU(limits, tc.cpu, tc.max)
			if tc.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)

				return
			}

			require.Nil(t, apiErr)
			assert.Equal(t, tc.want, got)
		})
	}

	_, apiErr := LimitMaxCPU(&types.TeamLimits{MaxVcpu: 64}, 8, i32(33))
	require.NotNil(t, apiErr, "Firecracker's ceiling applies whatever the tier")
}
