//go:build linux

package layer

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/config"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

// ResumeSandbox creates sandboxes for resuming existing templates
type ResumeSandbox struct {
	config         *sandbox.Config
	sandboxFactory *sandbox.Factory
	timeout        time.Duration
}

var _ SandboxCreator = (*ResumeSandbox)(nil)

func NewResumeSandbox(config *sandbox.Config, sandboxFactory *sandbox.Factory, timeout time.Duration) *ResumeSandbox {
	return &ResumeSandbox{config: config, sandboxFactory: sandboxFactory, timeout: timeout}
}

func (rs *ResumeSandbox) Sandbox(
	ctx context.Context,
	layerExecutor *LayerExecutor,
	template sbxtemplate.Template,
) (*sandbox.Sandbox, error) {
	// A cached layer of a block-layout build was snapshotted with placeholder
	// workspace drives; the resume needs files of the same size in the slots.
	workspaceDrives, releaseWorkspace, err := buildWorkspaceDrives(layerExecutor)
	if err != nil {
		return nil, err
	}
	defer releaseWorkspace()

	// The layer snapshot carries the build's hotplug device; the resume checks
	// the snapshot against it.
	rs.config.MemoryHotplug = layerExecutor.Config.MemoryHotplug.Clone()

	sbx, err := rs.sandboxFactory.ResumeSandbox(
		ctx,
		template,
		rs.config,
		sandbox.RuntimeMetadata{
			TemplateID:  layerExecutor.Config.TemplateID,
			SandboxID:   config.InstanceBuildPrefix + id.Generate(),
			ExecutionID: uuid.NewString(),
			TeamID:      layerExecutor.Config.TeamID,
			BuildID:     layerExecutor.Template.BuildID,
			SandboxType: sandbox.SandboxTypeBuild,
		},
		time.Now(),
		time.Now().Add(rs.timeout),
		nil,
		sandbox.ResumeWithWorkspaceDrives(workspaceDrives),
	)
	if err != nil {
		return nil, fmt.Errorf("resume sandbox: %w", err)
	}

	return sbx, nil
}
