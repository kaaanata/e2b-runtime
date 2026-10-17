//go:build linux

package fc

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
)

// WorkspaceDrive binds one workspace block slot of a sandbox to a file on the
// host for the lifetime of one Firecracker process.
//
// DriveID is the slot's fixed Firecracker drive ID (WorkspaceDriveID). PathOnHost
// is an already-resolved host path: the sandbox's own backing file for that slot
// on a sandbox start or resume, or a placeholder of the slot's capacity on a
// template build. Resolving a volume to that path (binding, cloning, holders) is
// the caller's job; the process only points the slot at it.
type WorkspaceDrive struct {
	DriveID    string
	PathOnHost string
}

// workspaceSlot returns the slot index a workspace drive ID names.
func workspaceSlot(driveID string) (int, bool) {
	for i := range MaxWorkspaceSlots {
		if WorkspaceDriveID(i) == driveID {
			return i, true
		}
	}

	return 0, false
}

// workspaceFileName is the file name of a workspace slot inside the sandbox
// mount namespace (next to the rootfs). Like the drive ID it is derived from the
// slot alone, so the path recorded in a snapshot is the same on every host.
func workspaceFileName(slot int) string {
	return fmt.Sprintf("%s%d.ext4", workspaceDriveIDPrefix, slot)
}

// workspaceSlots validates a set of workspace drive bindings and returns the
// slot index of each, in input order. Bindings must name distinct, known slots
// and non-empty host paths; order is otherwise free.
func workspaceSlots(drives []WorkspaceDrive) ([]int, error) {
	if len(drives) > MaxWorkspaceSlots {
		return nil, fmt.Errorf("%d workspace drives exceed the %d slots", len(drives), MaxWorkspaceSlots)
	}

	slots := make([]int, 0, len(drives))
	seen := make(map[int]struct{}, len(drives))
	for _, d := range drives {
		slot, ok := workspaceSlot(d.DriveID)
		if !ok {
			return nil, fmt.Errorf("unknown workspace drive id %q", d.DriveID)
		}

		if _, dup := seen[slot]; dup {
			return nil, fmt.Errorf("workspace drive id %q bound twice", d.DriveID)
		}
		seen[slot] = struct{}{}

		if d.PathOnHost == "" || !filepath.IsAbs(d.PathOnHost) {
			return nil, fmt.Errorf("workspace drive %q needs an absolute host path, got %q", d.DriveID, d.PathOnHost)
		}

		slots = append(slots, slot)
	}

	return slots, nil
}

// sortWorkspaceDrives returns a copy of drives ordered by slot index. Drives
// naming no known slot sort last, in input order; workspaceSlots rejects them.
func sortWorkspaceDrives(drives []WorkspaceDrive) []WorkspaceDrive {
	sorted := slices.Clone(drives)
	slices.SortStableFunc(sorted, func(a, b WorkspaceDrive) int {
		sa, okA := workspaceSlot(a.DriveID)
		sb, okB := workspaceSlot(b.DriveID)
		if !okA {
			sa = MaxWorkspaceSlots
		}
		if !okB {
			sb = MaxWorkspaceSlots
		}

		return cmp.Compare(sa, sb)
	})

	return sorted
}
