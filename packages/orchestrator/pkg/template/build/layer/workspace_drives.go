//go:build linux

package layer

import (
	"fmt"
	"os"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/workspace"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

// buildWorkspaceDrives prepares the workspace drive bindings a build VM boots or
// resumes with. For a legacy layout it returns nothing. For a block-v1 layout it
// creates one placeholder file per slot under the builder's templates
// directory, so the layer's snapshot records the slots at their capacity.
//
// The returned release removes the placeholders. Call it as soon as the sandbox
// factory has returned: Firecracker opens every drive file while the VM is
// configured (Create) or the snapshot is loaded (Resume), and an open, unlinked
// file lives exactly as long as that process — which is the lifetime the
// placeholder needs and nothing a later phase has to remember to clean up.
func buildWorkspaceDrives(layerExecutor *LayerExecutor) (drives []fc.WorkspaceDrive, release func(), err error) {
	cfg := layerExecutor.Config
	if err := workspace.ValidateLayout(cfg.Layout, cfg.Workspace, fc.MaxWorkspaceSlots); err != nil {
		return nil, nil, fmt.Errorf("workspace layout: %w", err)
	}

	if cfg.Layout != metadata.LayoutBlockV1 {
		return nil, func() {}, nil
	}

	dir, err := os.MkdirTemp(layerExecutor.BuilderConfig.TemplatesDir, "workspace-placeholders-"+layerExecutor.Template.BuildID+"-")
	if err != nil {
		return nil, nil, fmt.Errorf("create workspace placeholder directory: %w", err)
	}
	release = func() { _ = os.RemoveAll(dir) }

	paths, err := workspace.Placeholders(dir, cfg.Workspace)
	if err != nil {
		release()

		return nil, nil, err
	}

	for slot, path := range paths {
		drives = append(drives, fc.WorkspaceDrive{
			DriveID:    fc.WorkspaceDriveID(slot),
			PathOnHost: path,
		})
	}

	return drives, release, nil
}
