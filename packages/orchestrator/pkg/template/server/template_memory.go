package server

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// resolveMemoryHotplug derives a build's virtio-mem device from its request.
//
// A build from scratch gets a device when maxMemoryMB is above memoryMB. A
// build from another template inherits that template's device: the child
// resumes the parent's snapshot, whose device table and memory file it cannot
// change, so it may only repeat the parent's maximum (or leave it unset), and
// its boot memory must be the parent's. supported reports whether the build's
// Firecracker has the device; a request for one on a build without it is
// refused rather than silently built fixed-size.
func resolveMemoryHotplug(ctx context.Context, cfg *templatemanager.TemplateConfig, parent parentMetadata, supported bool) (*metadata.MemoryHotplugLayout, error) {
	base := int64(cfg.GetMemoryMB())

	if from := cfg.GetFromTemplate(); from != nil {
		tm, err := parent(ctx, from.GetBuildID())
		switch {
		case errors.Is(err, metadata.ErrUnsupportedLayout):
			return nil, status.Errorf(codes.FailedPrecondition, "base template %s has a layout this node does not support: %v", from.GetBuildID(), err)
		case err != nil:
			// The base phase reports a missing or outdated base template
			// with the established message; nothing to inherit here.
			return nil, nil //nolint:nilerr // reported by the base phase
		}

		inherited := tm.MemoryHotplug
		if cfg.MaxMemoryMB != nil {
			requested := int64(cfg.GetMaxMemoryMB())
			inheritedMax := int64(0)
			if inherited != nil {
				inheritedMax = inherited.MaxMiB()
			}

			// Repeating the parent's maximum (or, for a parent without a
			// device, the build's own memory) is a no-op request.
			if (inherited == nil && requested != base) || (inherited != nil && requested != inheritedMax) {
				return nil, status.Errorf(codes.InvalidArgument,
					"a build from template %s keeps its maximum memory (%d MiB, 0 = fixed size); leave maxMemoryMB unset to inherit it",
					from.GetBuildID(), inheritedMax)
			}
		}

		if inherited != nil && inherited.BaseMiB != base {
			return nil, status.Errorf(codes.InvalidArgument,
				"a build from template %s keeps its boot memory of %d MiB (the snapshot's memory layout is fixed); requested %d MiB",
				from.GetBuildID(), inherited.BaseMiB, base)
		}

		return inherited.Clone(), nil
	}

	if cfg.MaxMemoryMB == nil {
		return nil, nil
	}

	layout, err := metadata.NewMemoryHotplugLayout(base, int64(cfg.GetMaxMemoryMB()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid maximum memory: %v", err)
	}

	if layout != nil && !supported {
		return nil, status.Error(codes.FailedPrecondition, "this node's Firecracker has no memory hotplug device; build without maxMemoryMB")
	}

	return layout, nil
}

// withBuildMemoryHotplug records on a successful build's metadata the maximum
// memory of its device (after inheritance), unset for a build without one.
func withBuildMemoryHotplug(meta *templatemanager.TemplateBuildMetadata, l *metadata.MemoryHotplugLayout) *templatemanager.TemplateBuildMetadata {
	if l != nil {
		meta.MaxMemoryMB = new(int32(l.MaxMiB()))
	}

	return meta
}
