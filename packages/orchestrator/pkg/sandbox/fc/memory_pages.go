//go:build linux

package fc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
)

// maxMappingsReads bounds the retries of a bitmap read whose mappings changed
// underneath it.
const maxMappingsReads = 3

// errMappingsChanged reports that /memory/mappings differed before and after a
// bitmap read, so the bitmap cannot be attributed to either list.
var errMappingsChanged = errors.New("guest memory mappings changed during the read (memory hotplug in progress)")

// fcPagesToMemfilePages translates a Firecracker page bitmap into memory-file
// page indices.
//
// /memory and /memory/dirty index pages "in the order of /memory/mappings":
// the bitmap concatenates the pages of every listed mapping. With a virtio-mem
// device that list is boot memory plus one entry per PLUGGED slot (measured:
// 9 entries at 8 plugged slots, none for the region at plugged 0), so bit i is
// not page i of the memory file: it belongs to the mapping whose cumulative
// page range contains i, at that mapping's offset. Without hotplug there is
// one mapping at offset 0 and the translation is the identity, which is
// returned without copying.
//
// The mappings must have been read together with the bitmap while the VM was
// paused (see readStableMappings); they change with every plug and unplug.
func fcPagesToMemfilePages(regions []memory.Region, pageSize int64, bitmap *roaring.Bitmap) (*roaring.Bitmap, error) {
	type span struct{ fcStart, fcEnd, fileStart uint64 }

	spans := make([]span, 0, len(regions))
	identity := true

	var cum uint64
	for i, r := range regions {
		if int64(r.Size)%pageSize != 0 || int64(r.Offset)%pageSize != 0 {
			return nil, fmt.Errorf("memory mapping %d (offset %d, size %d) is not aligned to the %d page size", i, r.Offset, r.Size, pageSize)
		}

		n := uint64(int64(r.Size) / pageSize)
		fileStart := uint64(int64(r.Offset) / pageSize)
		if fileStart != cum {
			identity = false
		}

		spans = append(spans, span{fcStart: cum, fcEnd: cum + n, fileStart: fileStart})
		cum += n
	}

	// Unchanged for every VM without plugged hotplug memory, exactly as the
	// bitmap was used before mappings were consulted.
	if identity {
		return bitmap, nil
	}

	if !bitmap.IsEmpty() && uint64(bitmap.Maximum()) >= cum {
		return nil, fmt.Errorf("page bitmap has page %d set, the mappings cover %d pages", bitmap.Maximum(), cum)
	}

	out := roaring.New()
	si := 0
	for start, end := range bitmap.Ranges() {
		s := uint64(start)
		for s < end {
			for spans[si].fcEnd <= s {
				si++
			}

			sp := spans[si]
			segEnd := min(end, sp.fcEnd)
			out.AddRange(sp.fileStart+(s-sp.fcStart), sp.fileStart+(segEnd-sp.fcStart))
			s = segEnd
		}
	}

	return out, nil
}

// readStableMappings runs read between two reads of /memory/mappings and
// returns the mappings when both agree, retrying a bounded number of times.
// The VM is paused for every caller (FC refuses /memory/dirty otherwise, and
// the pause paths read /memory while paused); resizes are serialized against
// pauses, so a retry is only a guard, but a bitmap paired with the wrong
// mapping list would export pages at the wrong offsets.
func (c *apiClient) readStableMappings(ctx context.Context, read func() error) ([]memory.Region, error) {
	for range maxMappingsReads {
		before, err := c.memoryMapping(ctx)
		if err != nil {
			return nil, err
		}

		if err := read(); err != nil {
			return nil, err
		}

		after, err := c.memoryMapping(ctx)
		if err != nil {
			return nil, err
		}

		if slices.Equal(before.Regions, after.Regions) {
			return before.Regions, nil
		}
	}

	return nil, errMappingsChanged
}
