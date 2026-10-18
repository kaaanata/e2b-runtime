package server

import (
	"context"
	"errors"
	"runtime"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// smtEnabled mirrors fc's machine config: SMT is on everywhere but arm64, and
// Firecracker then accepts only 1 or an even vcpu_count.
var smtEnabled = runtime.GOARCH != "arm64"

// resolveCPUHotplug derives a build's CPU hotplug layout from its request, as
// resolveMemoryHotplug does for memory. A build from scratch gets one when
// maxCpuCount is above vCpuCount. A build from another template inherits that
// template's layout (the child resumes the parent's snapshot, which holds all
// of its vCPUs), may only repeat its maximum, and must keep its CPU count.
func resolveCPUHotplug(ctx context.Context, cfg *templatemanager.TemplateConfig, parent parentMetadata) (*metadata.CPUHotplugLayout, error) {
	base := int64(cfg.GetVCpuCount())

	if from := cfg.GetFromTemplate(); from != nil {
		tm, err := parent(ctx, from.GetBuildID())
		switch {
		case errors.Is(err, metadata.ErrUnsupportedLayout):
			return nil, status.Errorf(codes.FailedPrecondition, "base template %s has a layout this node does not support: %v", from.GetBuildID(), err)
		case err != nil:
			return nil, nil //nolint:nilerr // reported by the base phase
		}

		inherited := tm.CPUHotplug
		if cfg.MaxCpuCount != nil {
			requested := int64(cfg.GetMaxCpuCount())
			inheritedMax := int64(0)
			if inherited != nil {
				inheritedMax = inherited.MaxVCPUs
			}

			if (inherited == nil && requested != base) || (inherited != nil && requested != inheritedMax) {
				return nil, status.Errorf(codes.InvalidArgument,
					"a build from template %s keeps its maximum CPU count (%d, 0 = fixed); leave maxCpuCount unset to inherit it",
					from.GetBuildID(), inheritedMax)
			}
		}

		if inherited != nil && inherited.BaseVCPUs != base {
			return nil, status.Errorf(codes.InvalidArgument,
				"a build from template %s keeps its CPU count of %d (its snapshot boots that many); requested %d",
				from.GetBuildID(), inherited.BaseVCPUs, base)
		}

		return inherited.Clone(), nil
	}

	if cfg.MaxCpuCount == nil {
		return nil, nil
	}

	layout, err := metadata.NewCPUHotplugLayout(base, int64(cfg.GetMaxCpuCount()), smtEnabled)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid maximum CPU count: %v", err)
	}

	return layout, nil
}

// withBuildCPUHotplug records on a successful build's metadata the maximum
// CPU count (after inheritance), unset for a fixed vCPU count.
func withBuildCPUHotplug(meta *templatemanager.TemplateBuildMetadata, l *metadata.CPUHotplugLayout) *templatemanager.TemplateBuildMetadata {
	if l != nil {
		meta.MaxCpuCount = new(int32(l.MaxVCPUs))
	}

	return meta
}
