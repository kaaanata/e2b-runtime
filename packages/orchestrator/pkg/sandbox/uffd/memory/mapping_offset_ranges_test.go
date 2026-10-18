//go:build linux

package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// The handshake of a hotplug VM: boot memory at offset 0 and the hotplug
// region right after it in the memory file, at an unrelated host address.
func hotplugMapping() *Mapping {
	return NewMapping([]Region{
		{BaseHostVirtAddr: 0x10_0000, Size: 0x4_0000, Offset: 0, PageSize: header.PageSize},
		{BaseHostVirtAddr: 0x80_0000, Size: 0x8_0000, Offset: 0x4_0000, PageSize: header.PageSize},
	})
}

func TestMapping_GetOffsetRanges(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mapping      *Mapping
		start, end   uintptr
		want         []block.Range
		wantUnmapped uintptr
	}{
		"inside the hotplug region": {
			mapping: hotplugMapping(), start: 0x80_1000, end: 0x80_3000,
			want: []block.Range{{Start: 0x4_1000, Size: 0x2000}},
		},
		"a whole slot-sized remove": {
			mapping: hotplugMapping(), start: 0x80_0000, end: 0x88_0000,
			want: []block.Range{{Start: 0x4_0000, Size: 0x8_0000}},
		},
		"boot memory": {
			mapping: hotplugMapping(), start: 0x10_0000, end: 0x10_1000,
			want: []block.Range{{Start: 0, Size: 0x1000}},
		},
		"straddles two adjacent regions": {
			mapping: NewMapping([]Region{
				{BaseHostVirtAddr: 0x1000, Size: 0x2000, Offset: 0, PageSize: header.PageSize},
				{BaseHostVirtAddr: 0x3000, Size: 0x2000, Offset: 0x2000, PageSize: header.PageSize},
			}),
			start: 0x2000, end: 0x4000,
			want: []block.Range{{Start: 0x1000, Size: 0x1000}, {Start: 0x2000, Size: 0x1000}},
		},
		"partly outside every region": {
			mapping: hotplugMapping(), start: 0x87_f000, end: 0x88_1000,
			want: []block.Range{{Start: 0xb_f000, Size: 0x1000}}, wantUnmapped: 0x1000,
		},
		"outside every region": {
			mapping: hotplugMapping(), start: 0x20_0000, end: 0x20_1000,
			wantUnmapped: 0x1000,
		},
		"empty range": {
			mapping: hotplugMapping(), start: 0x80_0000, end: 0x80_0000,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, unmapped := tc.mapping.GetOffsetRanges(tc.start, tc.end)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantUnmapped, unmapped)
		})
	}
}
