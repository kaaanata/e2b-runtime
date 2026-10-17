//go:build linux

package server

import (
	"errors"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// workspaceVolumesFromRequest converts the workspace slot bindings of a
// create/resume request. Field validation (ids, slots, paths) happens in the
// sandbox package against the template layout.
func workspaceVolumesFromRequest(slots []*orchestrator.SandboxWorkspaceSlot) []sandbox.WorkspaceVolume {
	if len(slots) == 0 {
		return nil
	}

	out := make([]sandbox.WorkspaceVolume, 0, len(slots))
	for _, s := range slots {
		slot := math.MaxInt32
		if s.GetSlot() <= math.MaxInt32 {
			slot = int(s.GetSlot())
		}

		out = append(out, sandbox.WorkspaceVolume{
			Slot:                  slot,
			VolumeID:              s.GetVolumeId(),
			IncarnationID:         s.GetIncarnationId(),
			MountPath:             s.GetMountPath(),
			ExpectedFsUUID:        s.GetFsUuid(),
			ExpectedCapacityBytes: s.GetCapacityBytes(),
			ResizeToBytes:         s.GetResizeToBytes(),
		})
	}

	return out
}

// workspaceRevisionsToProto converts a pause's seals for the response.
func workspaceRevisionsToProto(seals []sandbox.SealedWorkspace) []*orchestrator.SandboxWorkspaceRevision {
	if len(seals) == 0 {
		return nil
	}

	out := make([]*orchestrator.SandboxWorkspaceRevision, 0, len(seals))
	for _, s := range seals {
		size := uint64(0)
		if s.Revision.SizeBytes > 0 {
			size = uint64(s.Revision.SizeBytes)
		}

		out = append(out, &orchestrator.SandboxWorkspaceRevision{
			Slot:                uint32(s.Slot), //nolint:gosec // slots are 0..fc.MaxWorkspaceSlots
			VolumeId:            s.VolumeID,
			SourceIncarnationId: s.IncarnationID,
			RevisionId:          s.Revision.RevisionID,
			OperationId:         s.OperationID,
			SubvolumeUuid:       s.Revision.SubvolUUID,
			FsUuid:              s.Revision.FsUUID,
			SizeBytes:           size,
			Consistency:         s.Revision.Consistency,
		})
	}

	return out
}

// isWorkspaceRefusal reports whether err is a workspace volume refusal: the
// request cannot be honoured for this sandbox/template/node and nothing was
// changed. The API maps the resulting FailedPrecondition to 409.
func isWorkspaceRefusal(err error) bool {
	return errors.Is(err, sandbox.ErrCapabilityUnsupported) ||
		errors.Is(err, sandbox.ErrWorkspaceLayout) ||
		errors.Is(err, sandbox.ErrWorkspaceRestore)
}

// workspaceRefusalStatus maps a workspace refusal to FailedPrecondition, or
// returns nil when err is not one.
func workspaceRefusalStatus(err error) error {
	if !isWorkspaceRefusal(err) {
		return nil
	}

	return status.Error(codes.FailedPrecondition, err.Error())
}
