package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestCreateVolumeMountModelsFromAPI_Backends(t *testing.T) {
	t.Parallel()

	const id = "8b9f0e53-6f43-4c6e-9c47-55f1b3f4e0a1"

	mounts, err := createVolumeMountModelsFromAPI([]*orchestrator.SandboxVolumeMount{
		{Id: id, Path: "/legacy", Type: "local", Name: "legacy"},
		{Id: id, Path: "/shared", Type: "local", Name: "shared", Backend: "nfs", ConsistencyProfile: "cto"},
	})
	require.NoError(t, err)
	require.Len(t, mounts, 2)
	assert.Equal(t, "cto", mounts[1].ConsistencyProfile)

	_, err = createVolumeMountModelsFromAPI([]*orchestrator.SandboxVolumeMount{
		{Id: id, Path: "/workspace", Type: "block", Name: "ws", Backend: "block"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a block mount in volume_mounts is refused, never mounted as NFS")
}
