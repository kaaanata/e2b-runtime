//go:build linux

package fc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func TestStartScriptBuilder_Build(t *testing.T) {
	t.Parallel()
	config, err := cfg.ParseBuilder()
	require.NoError(t, err)

	tests := []struct {
		name                  string
		versions              Config
		files                 *storage.SandboxFiles
		rootfsPaths           RootfsPaths
		namespaceID           string
		expectedRootfsPath    string
		expectedKernelPath    string
		expectedScriptContent []string // parts that should be in the generated script
	}{
		{
			name: "basic_build_with_version_2",
			versions: Config{
				KernelVersion:      "6.1.0",
				FirecrackerVersion: "1.4.0",
			},
			files: createTestSandboxFiles("test-sandbox", "static-id"),
			rootfsPaths: RootfsPaths{
				TemplateVersion: 2,
				TemplateID:      "template-123",
				BuildID:         "build-456",
			},
			namespaceID:        "ns-789",
			expectedRootfsPath: "/fc-vm/rootfs.ext4",
			expectedKernelPath: "/fc-vm/6.1.0/vmlinux.bin",
			expectedScriptContent: []string{
				"mount --make-rprivate /",
				"mount -t tmpfs tmpfs /fc-vm -o X-mount.mkdir",
				"ln -s /orchestrator/sandbox/rootfs-test-sandbox-static-id.link /fc-vm/rootfs.ext4",
				"mkdir -p /fc-vm/6.1.0",
				"ln -s /fc-kernels/6.1.0/vmlinux.bin /fc-vm/6.1.0/vmlinux.bin",
				"ip netns exec ns-789 /fc-versions/1.4.0/firecracker --api-sock",
				"fc-test-sandbox-static-id.sock",
			},
		},
		{
			name: "build_with_version_1_backward_compatibility",
			versions: Config{
				KernelVersion:      "5.10.0",
				FirecrackerVersion: "1.3.0",
			},
			files: createTestSandboxFiles("legacy-sandbox", "legacy-id"),
			rootfsPaths: RootfsPaths{
				TemplateVersion: 1,
				TemplateID:      "legacy-template",
				BuildID:         "legacy-build",
			},
			namespaceID:        "legacy-ns",
			expectedRootfsPath: "/mnt/disks/fc-envs/v1/legacy-template/builds/legacy-build/rootfs.ext4",
			expectedKernelPath: "/fc-vm/5.10.0/vmlinux.bin",
			expectedScriptContent: []string{
				"mount --make-rprivate /",
				"mount -t tmpfs tmpfs /mnt/disks/fc-envs/v1/legacy-template/builds/legacy-build -o X-mount.mkdir",
				"ln -s /orchestrator/sandbox/rootfs-legacy-sandbox-legacy-id.link /mnt/disks/fc-envs/v1/legacy-template/builds/legacy-build/rootfs.ext4",
				"mount -t tmpfs tmpfs /fc-vm/5.10.0 -o X-mount.mkdir",
				"ln -s /fc-kernels/5.10.0/vmlinux.bin /fc-vm/5.10.0/vmlinux.bin",
				"ip netns exec legacy-ns /fc-versions/1.3.0/firecracker --api-sock",
				"fc-legacy-sandbox-legacy-id.sock",
			},
		},
		{
			name: "different_kernel_and_firecracker_versions",
			versions: Config{
				KernelVersion:      "6.2.1",
				FirecrackerVersion: "1.5.0-beta",
			},
			files: createTestSandboxFiles("custom-sandbox", "custom-id"),
			rootfsPaths: RootfsPaths{
				TemplateVersion: 2,
				TemplateID:      "custom-template",
				BuildID:         "custom-build",
			},
			namespaceID:        "custom-ns-id",
			expectedRootfsPath: "/fc-vm/rootfs.ext4",
			expectedKernelPath: "/fc-vm/6.2.1/vmlinux.bin",
			expectedScriptContent: []string{
				"mkdir -p /fc-vm/6.2.1",
				"ln -s /fc-kernels/6.2.1/vmlinux.bin /fc-vm/6.2.1/vmlinux.bin",
				"ip netns exec custom-ns-id /fc-versions/1.5.0-beta/firecracker --api-sock",
				"fc-custom-sandbox-custom-id.sock",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			builder := NewStartScriptBuilder(config)

			// Call Build function directly with the four parameters
			result, err := builder.Build(tt.versions, tt.files, tt.rootfsPaths, tt.namespaceID, nil)
			require.NoError(t, err)
			require.NotNil(t, result)

			// Test computed paths
			assert.Equal(t, tt.expectedRootfsPath, result.RootfsPath)
			assert.Equal(t, tt.expectedKernelPath, result.KernelPath)

			// Test that script is not empty
			assert.NotEmpty(t, result.Value)

			// Test that script contains expected content
			for _, expected := range tt.expectedScriptContent {
				if !strings.Contains(result.Value, expected) {
					t.Errorf("Script should contain expected content: %s\nActual script:\n%s", expected, result.Value)
				}
			}

			// Test script structure - should have all major sections
			if !strings.Contains(result.Value, "mount --make-rprivate /") {
				t.Error("Script should start with mount command")
			}
			if !strings.Contains(result.Value, "ip netns exec") {
				t.Error("Script should end with firecracker execution")
			}

			// Test that the script has proper formatting (should not have extra spaces or newlines)
			lines := strings.Split(result.Value, "\n")
			if len(lines) <= 1 {
				t.Error("Script should have multiple lines")
			}

			assert.Empty(t, result.WorkspacePaths, "no workspace drives were requested")
			assert.NotContains(t, result.Value, "workspace", "a sandbox without workspace drives gets the script it always had")
		})
	}
}

// Workspace slots are symlinks next to the rootfs in the V2 layout, at a path
// that depends on the slot alone: that path goes into the snapshot's device
// table and must resolve on whichever host later loads it.
func TestStartScriptBuilder_Build_WorkspaceDrives(t *testing.T) {
	t.Parallel()
	config, err := cfg.ParseBuilder()
	require.NoError(t, err)

	builder := NewStartScriptBuilder(config)
	versions := Config{KernelVersion: "6.1.0", FirecrackerVersion: "1.4.0"}
	files := createTestSandboxFiles("ws-sandbox", "ws-id")
	drives := []WorkspaceDrive{
		{DriveID: WorkspaceDriveID(2), PathOnHost: "/mnt/blockvol/inc-a/workspace.ext4"},
		{DriveID: WorkspaceDriveID(0), PathOnHost: "/mnt/blockvol/inc-b/workspace.ext4"},
	}

	t.Run("v2 layout links every slot", func(t *testing.T) {
		t.Parallel()

		result, err := builder.Build(versions, files, RootfsPaths{TemplateVersion: 2}, "ns", drives)
		require.NoError(t, err)

		// The script links the per-sandbox host link, never the backing file: the
		// backing file is re-pointed without regenerating the script.
		assert.Contains(t, result.Value, "ln -s /orchestrator/sandbox/workspace2-ws-sandbox-ws-id.link /fc-vm/workspace2.ext4 &&")
		assert.Contains(t, result.Value, "ln -s /orchestrator/sandbox/workspace0-ws-sandbox-ws-id.link /fc-vm/workspace0.ext4 &&")
		assert.NotContains(t, result.Value, "/mnt/blockvol")
		assert.Equal(t, []string{"/fc-vm/workspace2.ext4", "/fc-vm/workspace0.ext4"}, result.WorkspacePaths, "paths follow the input order")
		assert.Equal(t, "/fc-vm/rootfs.ext4", result.RootfsPath)

		// Still one well-formed command chain.
		for _, line := range strings.Split(strings.TrimSpace(result.Value), "\n") {
			if line == "" || strings.HasPrefix(line, "ip netns exec") {
				continue
			}
			assert.True(t, strings.HasSuffix(line, "&&"), "line %q must chain", line)
		}
	})

	t.Run("v1 layout has no room for slots", func(t *testing.T) {
		t.Parallel()

		_, err := builder.Build(versions, files, RootfsPaths{TemplateVersion: 1, TemplateID: "t", BuildID: "b"}, "ns", drives)
		require.ErrorContains(t, err, "v2 sandbox layout")
	})

	t.Run("invalid bindings are rejected before a script exists", func(t *testing.T) {
		t.Parallel()

		_, err := builder.Build(versions, files, RootfsPaths{TemplateVersion: 2}, "ns", []WorkspaceDrive{{DriveID: "rootfs", PathOnHost: "/x"}})
		require.ErrorContains(t, err, "invalid workspace drives")
	})
}

// createTestSandboxFiles creates a SandboxFiles instance for testing
func createTestSandboxFiles(sandboxID, staticID string) *storage.SandboxFiles {
	paths := storage.Paths{
		BuildID: "test-build",
	}

	cachePaths := storage.CachePaths{
		Paths:           paths,
		CacheIdentifier: "test-cache-id",
	}

	return cachePaths.NewSandboxFilesWithStaticID(sandboxID, staticID)
}
