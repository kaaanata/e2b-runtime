//go:build linux

package fc

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestFcPagesToMemfilePages(t *testing.T) {
	t.Parallel()

	const page = header.PageSize
	bitmap := roaring.BitmapOf

	t.Run("no hotplug: identity, same bitmap", func(t *testing.T) {
		t.Parallel()

		in := bitmap(0, 5, 7)
		got, err := fcPagesToMemfilePages([]memory.Region{{Size: 8 * page, Offset: 0, PageSize: page}}, page, in)
		require.NoError(t, err)
		assert.Same(t, in, got)
	})

	t.Run("plugged slots map to their offsets in the hotplug region", func(t *testing.T) {
		t.Parallel()

		// Boot memory of 4 pages, then plugged slots of 2 pages each at
		// memory-file pages 4 (slot 0) and 10 (slot 3): slots 1 and 2 are not
		// plugged and not listed.
		regions := []memory.Region{
			{BaseHostVirtAddr: 0x1000_0000, Size: 4 * page, Offset: 0, PageSize: page},
			{BaseHostVirtAddr: 0x9000_0000, Size: 2 * page, Offset: 4 * page, PageSize: page},
			{BaseHostVirtAddr: 0x9000_6000, Size: 2 * page, Offset: 10 * page, PageSize: page},
		}

		// FC pages: 0..3 boot, 4..5 slot 0, 6..7 slot 3.
		got, err := fcPagesToMemfilePages(regions, page, bitmap(1, 3, 4, 5, 6, 7))
		require.NoError(t, err)
		assert.Equal(t, []uint32{1, 3, 4, 5, 10, 11}, got.ToArray())

		empty, err := fcPagesToMemfilePages(regions, page, roaring.New())
		require.NoError(t, err)
		assert.True(t, empty.IsEmpty())

		_, err = fcPagesToMemfilePages(regions, page, bitmap(8))
		require.Error(t, err, "a page beyond the listed mappings")
	})

	t.Run("a run crossing mappings is split", func(t *testing.T) {
		t.Parallel()

		regions := []memory.Region{
			{Size: 2 * page, Offset: 0, PageSize: page},
			{Size: 2 * page, Offset: 100 * page, PageSize: page},
		}
		got, err := fcPagesToMemfilePages(regions, page, bitmap(1, 2, 3))
		require.NoError(t, err)
		assert.Equal(t, []uint32{1, 100, 101}, got.ToArray())
	})

	t.Run("misaligned mapping", func(t *testing.T) {
		t.Parallel()

		_, err := fcPagesToMemfilePages([]memory.Region{{Size: page + 1, Offset: 0, PageSize: page}}, page, bitmap(0))
		require.Error(t, err)
	})
}
