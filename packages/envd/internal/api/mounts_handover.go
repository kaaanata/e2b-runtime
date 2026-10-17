package api

import (
	"github.com/e2b-dev/infra/packages/envd/internal/services/spec/upgrade"
)

// ExportMounts snapshots the volume mount ledger (path -> lifecycle id, source
// type, device) as typed MountEntry messages, carried across an envd
// live-upgrade. The kernel mounts survive the same-PID execve, but the ledger
// lives only in envd's heap; without it the new envd's post-upgrade /init would
// see an empty ledger, decide every volume needs (re)mounting, and force-unmount
// + remount a still-live mount — risking ESTALE for the workload and a failed
// resume. Carrying it lets /init recognize a matching-lifecycle mount and leave
// it in place, and keeps the block-volume devices the mount-set freeze and
// /volumes/resize act on.
func (a *API) ExportMounts() []*upgrade.MountEntry {
	out := make([]*upgrade.MountEntry, 0)
	a.mountedPaths.Range(func(k, _ any) bool {
		path, _ := k.(string)
		record, ok := a.loadMount(path)
		if !ok {
			return true
		}
		out = append(out, &upgrade.MountEntry{
			Path:        path,
			LifecycleId: record.LifecycleID,
			SourceType:  string(record.SourceType),
			Device:      record.Device,
		})

		return true
	})

	return out
}

// ImportMounts restores the volume mount ledger from a live-upgrade handover so
// the new envd knows which paths are already mounted (and for which lifecycle)
// before its first post-upgrade /init runs setupVolumes. An entry from an older
// envd carries no source type; it is read as nfs, the only kind that envd mounted.
func (a *API) ImportMounts(mounts []*upgrade.MountEntry) {
	for _, m := range mounts {
		sourceType := VolumeMountSourceType(m.GetSourceType())
		if sourceType == "" {
			sourceType = Nfs
		}
		a.storeMount(m.GetPath(), mountRecord{
			LifecycleID: m.GetLifecycleId(),
			SourceType:  sourceType,
			Device:      m.GetDevice(),
		})
	}
}
