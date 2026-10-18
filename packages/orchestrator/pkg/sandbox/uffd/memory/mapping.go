//go:build linux

package memory

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
)

type AddressNotFoundError struct {
	hostVirtAddr uintptr
}

func (e AddressNotFoundError) Error() string {
	return fmt.Sprintf("host virtual address %d not found in any mapping", e.hostVirtAddr)
}

type OffsetNotFoundError struct {
	offset int64
}

func (e OffsetNotFoundError) Error() string {
	return fmt.Sprintf("offset %d not found in any mapping", e.offset)
}

type Mapping struct {
	Regions []Region
}

func NewMapping(regions []Region) *Mapping {
	return &Mapping{Regions: regions}
}

func NewMappingFromFc(regions []*models.GuestMemoryRegionMapping) (*Mapping, error) {
	r := make([]Region, len(regions))

	for i, infoRegion := range regions {
		if infoRegion.BaseHostVirtAddr == nil || infoRegion.Size == nil || infoRegion.Offset == nil || infoRegion.PageSize == nil {
			return nil, fmt.Errorf("missing required fields for memory region %d", i)
		}

		r[i] = Region{
			BaseHostVirtAddr: uintptr(*infoRegion.BaseHostVirtAddr),
			Size:             uintptr(*infoRegion.Size),
			Offset:           uintptr(*infoRegion.Offset),
			PageSize:         uintptr(*infoRegion.PageSize),
		}
	}

	return NewMapping(r), nil
}

// GetOffset returns the relative offset of the mapped range for a given address.
func (m *Mapping) GetOffset(hostVirtAddr uintptr) (int64, error) {
	for _, r := range m.Regions {
		if hostVirtAddr >= r.BaseHostVirtAddr && hostVirtAddr < r.endHostVirtAddr() {
			return r.shiftedOffset(hostVirtAddr), nil
		}
	}

	return 0, AddressNotFoundError{hostVirtAddr: hostVirtAddr}
}

// GetHostVirtRanges returns the host virtual addresses and sizes (ranges) that cover exactly the given [offset, offset+length) range in the host virtual address space.
func (m *Mapping) GetHostVirtRanges(off int64, size int64) (hostVirtRanges []block.Range, err error) {
	for n := int64(0); n < size; {
		currentOff := off + n

		region, err := m.getHostVirtRegion(currentOff)
		if err != nil {
			return nil, err
		}

		start := region.shiftedHostVirtAddr(currentOff)
		remainingSize := min(int64(region.endHostVirtAddr()-start), size-n)

		r := block.NewRange(int64(start), remainingSize)

		hostVirtRanges = append(hostVirtRanges, r)

		n += r.Size
	}

	return hostVirtRanges, nil
}

// getHostVirtRegion returns the region that contains the given offset.
func (m *Mapping) getHostVirtRegion(off int64) (*Region, error) {
	for _, r := range m.Regions {
		if off >= int64(r.Offset) && off < r.endOffset() {
			return &r, nil
		}
	}

	return nil, OffsetNotFoundError{offset: off}
}

// GetHostVirtAddr returns the host virtual address for the given offset.
func (m *Mapping) GetHostVirtAddr(off int64) (uintptr, error) {
	region, err := m.getHostVirtRegion(off)
	if err != nil {
		return 0, err
	}

	return region.shiftedHostVirtAddr(off), nil
}

// GetOffsetRanges translates the host virtual range [start, end) into the
// memory-file offset ranges it covers, one per region it intersects, in
// address order. A UFFD REMOVE is reported as one host range and may span
// regions (boot memory and the hotplug region are adjacent mappings only by
// accident of the allocator), so it must not be translated from its start
// alone. Parts of the range outside every region are returned as unmapped
// bytes for the caller to report.
func (m *Mapping) GetOffsetRanges(start, end uintptr) (ranges []block.Range, unmapped uintptr) {
	if end <= start {
		return nil, 0
	}

	covered := uintptr(0)
	for i := range m.Regions {
		r := &m.Regions[i]

		lo := max(start, r.BaseHostVirtAddr)
		hi := min(end, r.endHostVirtAddr())
		if lo >= hi {
			continue
		}

		ranges = append(ranges, block.NewRange(r.shiftedOffset(lo), int64(hi-lo)))
		covered += hi - lo
	}

	slices.SortFunc(ranges, func(a, b block.Range) int { return cmp.Compare(a.Start, b.Start) })

	return ranges, (end - start) - covered
}
