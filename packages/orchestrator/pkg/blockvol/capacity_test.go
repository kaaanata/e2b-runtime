package blockvol

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture is verbatim `btrfs filesystem usage -b` (btrfs-progs 6.14) from
// a 20 GiB loop-backed filesystem on Citadel with one 64 MiB fallocated file.
func TestParseUsageFixture(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/usage-citadel-loop20g.txt")
	require.NoError(t, err)

	u, err := ParseUsage(raw)
	require.NoError(t, err)

	assert.Equal(t, uint64(21474836480), u.DeviceSize)
	assert.Equal(t, uint64(19839057920), u.DeviceUnallocated)
	assert.Equal(t, uint64(20854075392), u.FreeEstimated)
	assert.Equal(t, uint64(5767168), u.GlobalReserve)
	assert.InDelta(t, 1.0, u.DataRatio, 0.001)
	assert.InDelta(t, 2.0, u.MetadataRatio, 0.001)
	assert.Equal(t, uint64(1082130432), u.DataSize)
	assert.Equal(t, uint64(67112960), u.DataUsed)
	assert.Equal(t, uint64(268435456), u.MetadataSize)
	assert.Equal(t, uint64(163840), u.MetadataUsed)
	assert.Equal(t, uint64(8388608), u.SystemSize)
	assert.Equal(t, uint64(16384), u.SystemUsed)

	// Data slack + unallocated/ratio; must agree with the tool's own estimate
	// to within the system chunk and reserve noise.
	assert.Equal(t, uint64(1082130432-67112960+19839057920), u.DataFree())
	assert.InDelta(t, float64(u.FreeEstimated), float64(u.DataFree()), 5<<20)
	assert.Equal(t, uint64(268435456-163840+19839057920/2), u.MetadataFree())
}

func TestParseUsageRejectsGarbage(t *testing.T) {
	t.Parallel()

	_, err := ParseUsage([]byte("WARNING: cannot read detailed chunk info\n"))
	require.Error(t, err)

	_, err = ParseUsage(nil)
	require.Error(t, err)
}

func TestWatermarksCheck(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/usage-citadel-loop20g.txt")
	require.NoError(t, err)

	u, err := ParseUsage(raw)
	require.NoError(t, err)

	wm := Watermarks{MinDataFree: 2 << 30, MinMetadataFree: 512 << 20}

	require.NoError(t, wm.Check(u, 10<<30), "10 GiB fits in ~19.5 GiB free with a 2 GiB watermark")

	err = wm.Check(u, 18<<30)
	require.ErrorIs(t, err, ErrResourceExhausted, "18 GiB + 2 GiB watermark exceeds free data")

	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "data", capErr.Pool)
	assert.Equal(t, uint64(18<<30), capErr.Required)
	assert.Equal(t, u.DataFree(), capErr.Free)

	// Metadata watermark: shrink the pool until it trips. Unallocated space
	// counts towards metadata too, so drain it first.
	tight := u
	tight.DeviceUnallocated = 0
	tight.MetadataUsed = tight.MetadataSize - (100 << 20)

	err = Watermarks{MinDataFree: 0, MinMetadataFree: 512 << 20}.Check(tight, 0)
	require.ErrorIs(t, err, ErrResourceExhausted)
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "metadata", capErr.Pool)

	require.NoError(t, Watermarks{}.Check(Usage{DataRatio: 1, MetadataRatio: 1}, 0), "zero watermarks never refuse")
}

func TestParseSubvolumeShow(t *testing.T) {
	t.Parallel()

	rw, err := os.ReadFile("testdata/subvolume-show-rw.txt")
	require.NoError(t, err)

	info, err := parseSubvolumeShow(rw)
	require.NoError(t, err)
	assert.Equal(t, "1ddb5ce3-0bfc-4447-8074-65487d7734a4", info.UUID)
	assert.Empty(t, info.ParentUUID)
	assert.Equal(t, "256", info.ID)
	assert.Equal(t, "10", info.Generation)
	assert.False(t, info.ReadOnly)
	assert.Equal(t, []string{"fixture/ro"}, info.Snapshots)

	ro, err := os.ReadFile("testdata/subvolume-show-ro.txt")
	require.NoError(t, err)

	info, err = parseSubvolumeShow(ro)
	require.NoError(t, err)
	assert.Equal(t, "e623f4c2-5f65-834c-9cf3-a9e7ab1b3a14", info.UUID)
	assert.Equal(t, "1ddb5ce3-0bfc-4447-8074-65487d7734a4", info.ParentUUID)
	assert.True(t, info.ReadOnly)
	assert.Empty(t, info.Snapshots)

	_, err = parseSubvolumeShow([]byte("fixture/sv\n\tName: sv\n"))
	require.Error(t, err)
}

func TestParseStarttime(t *testing.T) {
	t.Parallel()

	// comm with spaces and parentheses must not shift the field count.
	stat := []byte("4242 (fire cracker (x)) S 1 4242 4242 0 -1 4194560 1234 0 0 0 7 3 0 0 20 0 5 0 987654321 123456789 500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0\n")

	v, err := parseStarttime(stat)
	require.NoError(t, err)
	assert.Equal(t, uint64(987654321), v)

	_, err = parseStarttime([]byte("4242 (x) S 1"))
	require.Error(t, err)

	_, err = parseStarttime([]byte("garbage"))
	require.Error(t, err)
}
