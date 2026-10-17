// Package workspace prepares the workspace block slots of a block-layout
// template build: the placeholder files every build VM boots with so that the
// template's snapshots reserve the slots. It creates nothing but those files.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

// ValidateLayout checks a build's layout request against what a template can
// carry: a legacy build has no workspace layout, a block-v1 build has 1..maxSlots
// slots each with a positive capacity.
func ValidateLayout(layout string, ws *metadata.WorkspaceLayout, maxSlots int) error {
	switch layout {
	case metadata.LayoutLegacy:
		if ws != nil {
			return fmt.Errorf("workspace layout given for a %q layout build", layout)
		}

		return nil
	case metadata.LayoutBlockV1:
		if ws == nil {
			return fmt.Errorf("layout %q needs a workspace layout", layout)
		}

		if ws.Slots < 1 || ws.Slots > maxSlots {
			return fmt.Errorf("layout %q needs 1..%d workspace slots, got %d", layout, maxSlots, ws.Slots)
		}

		if len(ws.SlotCapacityBytes) != ws.Slots {
			return fmt.Errorf("layout %q has %d slots but %d slot capacities", layout, ws.Slots, len(ws.SlotCapacityBytes))
		}

		for i, c := range ws.SlotCapacityBytes {
			if c <= 0 {
				return fmt.Errorf("workspace slot %d needs a positive capacity, got %d", i, c)
			}
		}

		return nil
	default:
		return fmt.Errorf("unknown template layout %q", layout)
	}
}

// Placeholders creates one sparse, unformatted file per workspace slot under
// dir, each of the slot's capacity, and returns their paths in slot order. The
// files only give the build VM a device of the right size in each slot; the
// guest never formats or mounts them, and at runtime a volume of the same
// capacity takes the slot. On error nothing is left behind.
func Placeholders(dir string, ws *metadata.WorkspaceLayout) (paths []string, err error) {
	if ws == nil {
		return nil, nil
	}

	defer func() {
		if err != nil {
			for _, p := range paths {
				_ = os.Remove(p)
			}
			paths = nil
		}
	}()

	for slot, capacity := range ws.SlotCapacityBytes {
		path := filepath.Join(dir, fmt.Sprintf("workspace%d.img", slot))

		f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return paths, fmt.Errorf("create placeholder for workspace slot %d: %w", slot, createErr)
		}
		paths = append(paths, path)

		truncErr := f.Truncate(capacity)
		closeErr := f.Close()
		if truncErr != nil || closeErr != nil {
			return paths, fmt.Errorf("size placeholder for workspace slot %d to %d bytes: %w", slot, capacity, errors.Join(truncErr, closeErr))
		}
	}

	return paths, nil
}
