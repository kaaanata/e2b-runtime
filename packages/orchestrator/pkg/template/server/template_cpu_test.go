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

func TestResolveCPUHotplug(t *testing.T) {
	t.Parallel()

	parentLayout := &metadata.CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}
	parents := func(m metadata.Template, err error) parentMetadata {
		return func(context.Context, string) (metadata.Template, error) { return m, err }
	}
	noParent := parents(metadata.Template{}, errors.New("unused"))
	hotplugParent := parents(metadata.Template{}.WithCPUHotplug(parentLayout), nil)
	fixedParent := parents(metadata.Template{}, nil)
	fromTemplate := &templatemanager.TemplateConfig_FromTemplate{FromTemplate: &templatemanager.FromTemplateConfig{BuildID: "parent"}}

	tests := map[string]struct {
		cfg      *templatemanager.TemplateConfig
		parent   parentMetadata
		want     *metadata.CPUHotplugLayout
		wantCode codes.Code
	}{
		"fixed by default":         {cfg: &templatemanager.TemplateConfig{VCpuCount: 8}, parent: noParent},
		"max equal to count fixed": {cfg: &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(8))}, parent: noParent},
		"hotplug from scratch": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(32))}, parent: noParent, want: parentLayout,
		},
		"above firecracker's ceiling": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(48))}, parent: noParent, wantCode: codes.InvalidArgument,
		},
		"max below count": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(4))}, parent: noParent, wantCode: codes.InvalidArgument,
		},
		"inherits the parent's layout": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 8, Source: fromTemplate}, parent: hotplugParent, want: parentLayout,
		},
		"may repeat the parent's max": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(32)), Source: fromTemplate}, parent: hotplugParent, want: parentLayout,
		},
		"may not change the parent's max": {
			cfg:    &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(16)), Source: fromTemplate},
			parent: hotplugParent, wantCode: codes.InvalidArgument,
		},
		"may not change the parent's count": {
			cfg: &templatemanager.TemplateConfig{VCpuCount: 4, Source: fromTemplate}, parent: hotplugParent, wantCode: codes.InvalidArgument,
		},
		"no hotplug on a fixed parent": {
			cfg:    &templatemanager.TemplateConfig{VCpuCount: 8, MaxCpuCount: new(int32(32)), Source: fromTemplate},
			parent: fixedParent, wantCode: codes.InvalidArgument,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveCPUHotplug(t.Context(), tc.cfg, tc.parent)
			if tc.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, status.Code(err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWithBuildCPUHotplug(t *testing.T) {
	t.Parallel()

	meta := withBuildCPUHotplug(&templatemanager.TemplateBuildMetadata{}, &metadata.CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32})
	require.NotNil(t, meta.MaxCpuCount) //nolint:protogetter // presence, not the value
	assert.Equal(t, int32(32), meta.GetMaxCpuCount())
	assert.Nil(t, withBuildCPUHotplug(&templatemanager.TemplateBuildMetadata{}, nil).MaxCpuCount) //nolint:protogetter // presence, not the value
}
