//go:build linux

package fc

import (
	"bytes"
	"fmt"
	"path/filepath"
	txtTemplate "text/template"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/artifact"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// startScriptArgs represents the arguments for the start script template
type startScriptArgs struct {
	SandboxDir string

	HostKernelPath    string
	SandboxKernelDir  string
	SandboxKernelFile string

	HostRootfsPath             string
	DeprecatedSandboxRootfsDir string
	SandboxRootfsFile          string

	// WorkspaceLinks are the workspace block slots this process binds, one
	// symlink each next to the rootfs. Empty for every sandbox without them.
	WorkspaceLinks []workspaceLink

	NamespaceID       string
	FirecrackerPath   string
	FirecrackerSocket string
}

// workspaceLink is one workspace slot's symlink in the sandbox mount namespace:
// SandboxFile (under SandboxDir) points at HostLinkPath, the per-sandbox host
// link that is re-pointed at the real backing file before the drive is used.
type workspaceLink struct {
	HostLinkPath string
	SandboxFile  string
}

// StartScriptResult contains the generated script and computed paths
type StartScriptResult struct {
	// Value is the generated firecracker start script
	Value string

	// RootfsPath is the computed rootfs path (with backward compatibility)
	RootfsPath string

	// KernelPath is the computed kernel path
	KernelPath string

	// WorkspacePaths are the in-namespace paths of the workspace drives passed to
	// Build, in the same order. These are the paths Firecracker records in the
	// snapshot, so they depend only on the slot, never on the host file.
	WorkspacePaths []string
}

const startScriptV1 = `mount --make-rprivate / &&

mount -t tmpfs tmpfs {{ .DeprecatedSandboxRootfsDir }} -o X-mount.mkdir &&
ln -s {{ .HostRootfsPath }} {{ .DeprecatedSandboxRootfsDir }}/{{ .SandboxRootfsFile }} &&

mount -t tmpfs tmpfs {{ .SandboxDir }}/{{ .SandboxKernelDir }} -o X-mount.mkdir &&
ln -s {{ .HostKernelPath }} {{ .SandboxDir }}/{{ .SandboxKernelDir }}/{{ .SandboxKernelFile }} &&

ip netns exec {{ .NamespaceID }} {{ .FirecrackerPath }} --api-sock {{ .FirecrackerSocket }}`

const startScriptV2 = `mount --make-rprivate / &&
mount -t tmpfs tmpfs {{ .SandboxDir }} -o X-mount.mkdir &&

ln -s {{ .HostRootfsPath }} {{ .SandboxDir }}/{{ .SandboxRootfsFile }} &&
{{- range .WorkspaceLinks }}
ln -s {{ .HostLinkPath }} {{ $.SandboxDir }}/{{ .SandboxFile }} &&
{{- end }}

mkdir -p {{ .SandboxDir }}/{{ .SandboxKernelDir }} &&
ln -s {{ .HostKernelPath }} {{ .SandboxDir }}/{{ .SandboxKernelDir }}/{{ .SandboxKernelFile }} &&

ip netns exec {{ .NamespaceID }} {{ .FirecrackerPath }} --api-sock {{ .FirecrackerSocket }}`

// StartScriptBuilder handles the creation and execution of firecracker start scripts
type StartScriptBuilder struct {
	builderConfig cfg.BuilderConfig
	templateV1    *txtTemplate.Template
	templateV2    *txtTemplate.Template
}

// NewStartScriptBuilder creates a new StartScriptBuilder instance
func NewStartScriptBuilder(builderConfig cfg.BuilderConfig) *StartScriptBuilder {
	templateV1 := txtTemplate.Must(txtTemplate.New("fc-start-v1").Parse(startScriptV1))
	templateV2 := txtTemplate.Must(txtTemplate.New("fc-start-v2").Parse(startScriptV2))

	return &StartScriptBuilder{
		builderConfig: builderConfig,
		templateV1:    templateV1,
		templateV2:    templateV2,
	}
}

// buildArgs prepares the arguments for the start script template
func (sb *StartScriptBuilder) buildArgs(
	versions Config,
	files *storage.SandboxFiles,
	rootfsPaths RootfsPaths,
	namespaceID string,
	workspaceSlots []int,
) startScriptArgs {
	links := make([]workspaceLink, 0, len(workspaceSlots))
	for _, slot := range workspaceSlots {
		links = append(links, workspaceLink{
			HostLinkPath: files.SandboxCacheWorkspaceLinkPath(sb.builderConfig.StorageConfig, slot),
			SandboxFile:  workspaceFileName(slot),
		})
	}

	return startScriptArgs{
		// General
		SandboxDir: sb.builderConfig.SandboxDir,

		// Kernel
		HostKernelPath:    versions.HostKernelPath(sb.builderConfig),
		SandboxKernelDir:  versions.SandboxKernelDir(),
		SandboxKernelFile: artifact.KernelFileName,

		// Rootfs
		HostRootfsPath:             files.SandboxCacheRootfsLinkPath(sb.builderConfig.StorageConfig),
		DeprecatedSandboxRootfsDir: rootfsPaths.DeprecatedSandboxRootfsDir(),
		SandboxRootfsFile:          artifact.RootfsFileName,

		// Workspace block slots
		WorkspaceLinks: links,

		// FC
		NamespaceID:       namespaceID,
		FirecrackerPath:   versions.FirecrackerPath(sb.builderConfig),
		FirecrackerSocket: files.SandboxFirecrackerSocketPath(),
	}
}

// GenerateScript builds and executes the start script template with the provided arguments
func (sb *StartScriptBuilder) GenerateScript(args startScriptArgs, rootfsPaths RootfsPaths) (string, error) {
	var scriptBuffer bytes.Buffer

	// Choose the appropriate template based on the rootfs version
	var template *txtTemplate.Template
	if rootfsPaths.TemplateVersion <= 1 {
		template = sb.templateV1
	} else {
		template = sb.templateV2
	}

	err := template.Execute(&scriptBuffer, args)
	if err != nil {
		return "", fmt.Errorf("error executing fc start script template: %w", err)
	}

	return scriptBuffer.String(), nil
}

// Build creates a complete StartScriptResult with script, args, and computed paths.
//
// workspaceDrives are the workspace block slots the process binds; they are
// validated here (distinct known slots, absolute host paths) and need the V2
// layout, whose SandboxDir is a writable tmpfs the slot links can live in. The
// V1 layout only mounts the deprecated per-build rootfs directory, and no
// template that reserves workspace slots was ever built on it.
func (sb *StartScriptBuilder) Build(
	versions Config,
	files *storage.SandboxFiles,
	rootfsPaths RootfsPaths,
	namespaceID string,
	workspaceDrives []WorkspaceDrive,
) (*StartScriptResult, error) {
	slots, err := workspaceSlots(workspaceDrives)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace drives: %w", err)
	}

	if len(slots) > 0 && rootfsPaths.TemplateVersion <= 1 {
		return nil, fmt.Errorf("workspace drives need the v2 sandbox layout, template version %d has none", rootfsPaths.TemplateVersion)
	}

	args := sb.buildArgs(versions, files, rootfsPaths, namespaceID, slots)

	script, err := sb.GenerateScript(args, rootfsPaths)
	if err != nil {
		return nil, err
	}

	rootfsPath := sb.getRootfsPath(args, rootfsPaths)
	kernelPath := sb.getKernelPath(args)

	workspacePaths := make([]string, 0, len(args.WorkspaceLinks))
	for _, link := range args.WorkspaceLinks {
		workspacePaths = append(workspacePaths, filepath.Join(args.SandboxDir, link.SandboxFile))
	}

	return &StartScriptResult{
		Value:          script,
		RootfsPath:     rootfsPath,
		KernelPath:     kernelPath,
		WorkspacePaths: workspacePaths,
	}, nil
}

// getRootfsPath returns the rootfs path based on the script args, with backward compatibility
func (sb *StartScriptBuilder) getRootfsPath(args startScriptArgs, rootfsPaths RootfsPaths) string {
	rootfsPath := filepath.Join(args.SandboxDir, args.SandboxRootfsFile)
	if rootfsPaths.TemplateVersion <= 1 {
		rootfsPath = filepath.Join(args.DeprecatedSandboxRootfsDir, args.SandboxRootfsFile)
	}

	return rootfsPath
}

// getKernelPath returns the kernel path based on the script args
func (sb *StartScriptBuilder) getKernelPath(args startScriptArgs) string {
	return filepath.Join(args.SandboxDir, args.SandboxKernelDir, args.SandboxKernelFile)
}
