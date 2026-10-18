//go:build linux

package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func TestMemoryRefusalStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, codes.FailedPrecondition, status.Code(memoryRefusalStatus(sandbox.ErrMemoryNotHotResizable)))
	assert.Equal(t, codes.FailedPrecondition, status.Code(memoryRefusalStatus(metadata.ErrMemorySizeOutOfRange)))
	assert.Equal(t, codes.Unavailable, status.Code(memoryRefusalStatus(sandbox.ErrMemoryResizeBusy)))
	assert.NoError(t, memoryRefusalStatus(errors.New("something else")))
	assert.NoError(t, memoryRefusalStatus(nil))
}
