//go:build linux

package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestWorkspaceVolumesFromRequest(t *testing.T) {
	t.Parallel()

	assert.Nil(t, workspaceVolumesFromRequest(nil))

	got := workspaceVolumesFromRequest([]*orchestrator.SandboxWorkspaceSlot{{
		Slot: 2, VolumeId: "v", IncarnationId: "i", MountPath: "/w", FsUuid: "f", CapacityBytes: 1, ResizeToBytes: 2,
	}, {Slot: 1 << 31}})

	require.Len(t, got, 2)
	assert.Equal(t, sandbox.WorkspaceVolume{
		Slot: 2, VolumeID: "v", IncarnationID: "i", MountPath: "/w", ExpectedFsUUID: "f", ExpectedCapacityBytes: 1, ResizeToBytes: 2,
	}, got[0])
	assert.Positive(t, got[1].Slot, "an out-of-range slot stays out of range (refused by the layout gate), it does not wrap to 0")
}

func TestWorkspaceRevisionsToProto(t *testing.T) {
	t.Parallel()

	assert.Nil(t, workspaceRevisionsToProto(nil))

	got := workspaceRevisionsToProto([]sandbox.SealedWorkspace{{
		Slot: 1, VolumeID: "v", IncarnationID: "i", OperationID: "op",
		Revision: metadata.WorkspaceRevisionRef{Slot: 1, RevisionID: "r", SubvolUUID: "s", FsUUID: "f", SizeBytes: 1 << 30, Consistency: "paired-memory"},
	}})

	require.Len(t, got, 1)
	assert.Equal(t, uint32(1), got[0].GetSlot())
	assert.Equal(t, "v", got[0].GetVolumeId())
	assert.Equal(t, "i", got[0].GetSourceIncarnationId())
	assert.Equal(t, "r", got[0].GetRevisionId())
	assert.Equal(t, "op", got[0].GetOperationId())
	assert.Equal(t, "s", got[0].GetSubvolumeUuid())
	assert.Equal(t, "f", got[0].GetFsUuid())
	assert.Equal(t, uint64(1<<30), got[0].GetSizeBytes())
	assert.Equal(t, "paired-memory", got[0].GetConsistency())
}

func TestWorkspaceRefusalStatus(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []error{sandbox.ErrCapabilityUnsupported, sandbox.ErrWorkspaceLayout, sandbox.ErrWorkspaceRestore} {
		st := workspaceRefusalStatus(fmt.Errorf("resume: %w", sentinel))
		require.Error(t, st)
		assert.Equal(t, codes.FailedPrecondition, status.Code(st))
	}

	assert.NoError(t, workspaceRefusalStatus(errors.New("disk on fire")))
}
