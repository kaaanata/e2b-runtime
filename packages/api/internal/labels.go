package internal

func MakeVolumeTypeLabel(volumeType string) string {
	return "persistent-volume-type=" + volumeType
}

// BlockVolumeNodeLabel is the label an orchestrator advertises when it serves
// workspace block volumes (BLOCK_VOLUME_ENABLED); only such nodes are chosen to
// own new block volumes. The orchestrator sets the same literal in
// packages/orchestrator/pkg/cfg (released separately); keep them in sync.
const BlockVolumeNodeLabel = "volume-backend=block"
