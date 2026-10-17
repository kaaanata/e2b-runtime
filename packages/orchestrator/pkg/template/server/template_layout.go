package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/workspace"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// parentMetadata reads the metadata of the template a build starts from.
type parentMetadata func(ctx context.Context, buildID string) (metadata.Template, error)

// resolveTemplateLayout derives a build's device layout from its request. A
// build from another template inherits that template's layout (the base
// snapshot's device table is lineage state every later layer restores) and
// may only repeat it, never change it. The result is validated against what
// a template can carry.
func resolveTemplateLayout(ctx context.Context, cfg *templatemanager.TemplateConfig, parent parentMetadata) (string, *metadata.WorkspaceLayout, error) {
	layout := cfg.GetLayout()

	var ws *metadata.WorkspaceLayout
	if capacities := cfg.GetWorkspaceSlotCapacityBytes(); len(capacities) > 0 {
		ws = &metadata.WorkspaceLayout{Slots: len(capacities), SlotCapacityBytes: slices.Clone(capacities)}
	}

	if from := cfg.GetFromTemplate(); from != nil {
		tm, err := parent(ctx, from.GetBuildID())
		switch {
		case errors.Is(err, metadata.ErrUnsupportedLayout):
			return "", nil, status.Errorf(codes.FailedPrecondition, "base template %s has a layout this node does not support: %v", from.GetBuildID(), err)
		case err != nil:
			// The base phase reports a missing or outdated base template;
			// building on it fails there with the established message.
			return layout, ws, validateLayout(layout, ws)
		}

		if cfg.Layout == nil && ws == nil {
			return tm.Layout, tm.Workspace, nil
		}

		if layout != tm.Layout || !sameWorkspace(ws, tm.Workspace) {
			return "", nil, status.Errorf(codes.InvalidArgument,
				"a build from template %s keeps its layout %q; leave layout unset to inherit it", from.GetBuildID(), tm.Layout)
		}
	}

	return layout, ws, validateLayout(layout, ws)
}

func validateLayout(layout string, ws *metadata.WorkspaceLayout) error {
	if err := workspace.ValidateLayout(layout, ws, fc.MaxWorkspaceSlots); err != nil {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("invalid template layout: %v", err))
	}

	return nil
}

func sameWorkspace(a, b *metadata.WorkspaceLayout) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return a.Slots == b.Slots && slices.Equal(a.SlotCapacityBytes, b.SlotCapacityBytes)
}

// withBuildLayout records on a successful build's metadata the layout the
// build was made with (after inheritance), which the API persists and
// reports. The layout is always set, empty for rootfs only, so the API can
// tell it from a template-manager that does not report one.
func withBuildLayout(meta *templatemanager.TemplateBuildMetadata, layout string, ws *metadata.WorkspaceLayout) *templatemanager.TemplateBuildMetadata {
	meta.Layout = &layout
	if ws != nil {
		meta.WorkspaceSlotCapacityBytes = slices.Clone(ws.SlotCapacityBytes)
	}

	return meta
}
