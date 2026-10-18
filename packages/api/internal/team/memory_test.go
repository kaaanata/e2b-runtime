package team

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

func TestLimitMaxMemory(t *testing.T) {
	t.Parallel()

	limits := &types.TeamLimits{MaxRamMb: 65536}
	i32 := func(v int32) *int32 { return &v }

	tests := map[string]struct {
		ram     int64
		max     *int32
		want    *int64
		wantErr bool
	}{
		"unset is fixed size":        {ram: 8192},
		"equal to memory is fixed":   {ram: 8192, max: i32(8192)},
		"hotplug up to the tier":     {ram: 8192, max: i32(65536), want: new(int64(65536))},
		"one slot":                   {ram: 8192, max: i32(8192 + 128), want: new(int64(8192 + 128))},
		"below memory":               {ram: 8192, max: i32(4096), wantErr: true},
		"above the tier":             {ram: 8192, max: i32(65536 + 128), wantErr: true},
		"region not a slot multiple": {ram: 8192, max: i32(8200), wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, apiErr := LimitMaxMemory(limits, tc.ram, tc.max)
			if tc.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)

				return
			}

			require.Nil(t, apiErr)
			assert.Equal(t, tc.want, got)
		})
	}
}
