package orchestrator

import (
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// nfsVolumeMounts returns the mounts a node receives as volume_mounts: every
// mount except block volumes, which are bound through workspace_slots
// (SandboxMetadata.WorkspaceSlots) and refused by the node if sent here.
func nfsVolumeMounts(mounts []*orchestrator.SandboxVolumeMount) []*orchestrator.SandboxVolumeMount {
	out := make([]*orchestrator.SandboxVolumeMount, 0, len(mounts))
	for _, m := range mounts {
		if m.GetBackend() == types.VolumeBackendBlock {
			continue
		}

		out = append(out, m)
	}

	return out
}

// sandboxMountConfigs is the sandbox record's view of its mounts: every mount
// in request order, block mounts completed with the incarnation their slot
// was bound to for this execution (matched by mount path).
func sandboxMountConfigs(mounts []*orchestrator.SandboxVolumeMount, slots []*orchestrator.SandboxWorkspaceSlot) []*types.SandboxVolumeMountConfig {
	configs := nodemanager.ConvertOrchestratorMountsToDatabaseMounts(mounts)
	if len(slots) == 0 {
		return configs
	}

	byPath := make(map[string]*orchestrator.SandboxWorkspaceSlot, len(slots))
	for _, s := range slots {
		byPath[s.GetMountPath()] = s
	}

	for _, c := range configs {
		if !c.IsBlock() {
			continue
		}

		if s, ok := byPath[c.Path]; ok {
			c.IncarnationID = s.GetIncarnationId()
			c.FsUUID = s.GetFsUuid()
			c.CapacityBytes = s.GetCapacityBytes()
		}
	}

	return configs
}
