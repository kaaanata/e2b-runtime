package orchestrator

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResizeMemoryError(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err      error
		wantCode int
		wantErr  string
	}{
		"not found":           {status.Error(codes.NotFound, "sandbox not found"), http.StatusNotFound, ""},
		"fixed-size template": {status.Error(codes.FailedPrecondition, "not hot-resizable"), http.StatusConflict, ErrCodeMemoryNotHotResizable},
		"checkpoint running":  {status.Error(codes.Unavailable, "checkpoint in progress"), http.StatusConflict, ErrCodeMemoryResizeBusy},
		"bad request":         {status.Error(codes.InvalidArgument, "must be positive"), http.StatusBadRequest, ""},
		"other":               {errors.New("boom"), http.StatusInternalServerError, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := resizeMemoryError("sbx", tc.err)
			assert.Equal(t, tc.wantCode, got.Code)
			assert.Equal(t, tc.wantErr, got.ErrorCode)
		})
	}
}
