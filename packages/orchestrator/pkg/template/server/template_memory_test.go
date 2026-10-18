package server

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

func TestResolveMemoryHotplug(t *testing.T) {
	t.Parallel()

	parentLayout := &metadata.MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2}
	parents := func(m metadata.Template, err error) parentMetadata {
		return func(context.Context, string) (metadata.Template, error) { return m, err }
	}
	noParent := parents(metadata.Template{}, errors.New("unused"))
	hotplugParent := parents(metadata.Template{}.WithMemoryHotplug(parentLayout), nil)
	fixedParent := parents(metadata.Template{}, nil)
	fromTemplate := &templatemanager.TemplateConfig_FromTemplate{FromTemplate: &templatemanager.FromTemplateConfig{BuildID: "parent"}}

	tests := map[string]struct {
		cfg       *templatemanager.TemplateConfig
		parent    parentMetadata
		supported bool
		want      *metadata.MemoryHotplugLayout
		wantCode  codes.Code
	}{
		"fixed size by default": {
			cfg: &templatemanager.TemplateConfig{MemoryMB: 8192}, parent: noParent, supported: true,
		},
		"max equal to memory is fixed size": {
			cfg: &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(8192))}, parent: noParent, supported: true,
		},
		"hotplug from scratch": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(65536))},
			parent: noParent, supported: true, want: parentLayout,
		},
		"region not a slot multiple": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(8200))},
			parent: noParent, supported: true, wantCode: codes.InvalidArgument,
		},
		"max below memory": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(4096))},
			parent: noParent, supported: true, wantCode: codes.InvalidArgument,
		},
		"firecracker without the device": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(65536))},
			parent: noParent, supported: false, wantCode: codes.FailedPrecondition,
		},
		"inherits the parent's device": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, Source: fromTemplate},
			parent: hotplugParent, supported: true, want: parentLayout,
		},
		"may repeat the parent's maximum": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(65536)), Source: fromTemplate},
			parent: hotplugParent, supported: true, want: parentLayout,
		},
		"may not change the parent's maximum": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(32768)), Source: fromTemplate},
			parent: hotplugParent, supported: true, wantCode: codes.InvalidArgument,
		},
		"may not add a device to a fixed-size parent": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(65536)), Source: fromTemplate},
			parent: fixedParent, supported: true, wantCode: codes.InvalidArgument,
		},
		"a fixed-size parent stays fixed": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 4096, Source: fromTemplate},
			parent: fixedParent, supported: true,
		},
		"may not change a hotplug parent's boot memory": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 4096, Source: fromTemplate},
			parent: hotplugParent, supported: true, wantCode: codes.InvalidArgument,
		},
		"unsupported parent layout": {
			cfg:       &templatemanager.TemplateConfig{MemoryMB: 8192, Source: fromTemplate},
			parent:    parents(metadata.Template{}, &metadata.UnsupportedLayoutError{Capabilities: []string{"x"}}),
			supported: true, wantCode: codes.FailedPrecondition,
		},
		"missing parent is left to the base phase": {
			cfg:    &templatemanager.TemplateConfig{MemoryMB: 8192, MaxMemoryMB: new(int32(65536)), Source: fromTemplate},
			parent: noParent, supported: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveMemoryHotplug(t.Context(), tc.cfg, tc.parent, tc.supported)
			if tc.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, status.Code(err), err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWithBuildMemoryHotplug(t *testing.T) {
	t.Parallel()

	meta := withBuildMemoryHotplug(&templatemanager.TemplateBuildMetadata{}, nil)
	assert.Nil(t, meta.MaxMemoryMB, "no device: unset")

	meta = withBuildMemoryHotplug(&templatemanager.TemplateBuildMetadata{},
		&metadata.MemoryHotplugLayout{BaseMiB: 8192, TotalMiB: 57344, SlotMiB: 128, BlockMiB: 2})
	require.NotNil(t, meta.MaxMemoryMB)
	assert.Equal(t, int32(65536), meta.GetMaxMemoryMB())
}
