//go:build linux

package block

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// vmFlagsOf returns the smaps VmFlags of the mapping that starts at addr.
func vmFlagsOf(t *testing.T, addr uintptr) []string {
	t.Helper()

	f, err := os.Open("/proc/self/smaps")
	require.NoError(t, err)
	defer f.Close()

	want := fmt.Sprintf("%x-", addr)
	inMapping := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.Contains(fields[0], "-") && !strings.HasSuffix(fields[0], ":") {
			inMapping = strings.HasPrefix(fields[0], want)

			continue
		}
		if inMapping && strings.HasPrefix(line, "VmFlags:") {
			return strings.Fields(strings.TrimPrefix(line, "VmFlags:"))
		}
	}
	require.NoError(t, sc.Err())
	t.Fatalf("mapping at %#x not found in /proc/self/smaps", addr)

	return nil
}

func TestNewFromFd_MapsNoReserve(t *testing.T) {
	t.Parallel()

	overcommit, err := os.ReadFile("/proc/sys/vm/overcommit_memory")
	require.NoError(t, err)
	if strings.TrimSpace(string(overcommit)) == "2" {
		// The kernel ignores MAP_NORESERVE for non-hugetlb mappings under
		// strict overcommit, so VM_NORESERVE is not observable here.
		t.Skip("vm.overcommit_memory=2")
	}

	m, _ := newTestMemfd(t, 4*int64(os.Getpagesize()))
	defer m.Close()

	assert.Contains(t, vmFlagsOf(t, uintptr(unsafe.Pointer(&m.mmap[0]))), "nr",
		"memfd view must be mapped MAP_NORESERVE")
}

func hugePagesRsvd(t *testing.T) (rsvd, free int64) {
	t.Helper()

	b, err := os.ReadFile("/proc/meminfo")
	require.NoError(t, err)
	rsvd, free = -1, -1
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, convErr := strconv.ParseInt(fields[1], 10, 64)
		if convErr != nil {
			continue
		}
		switch fields[0] {
		case "HugePages_Rsvd:":
			rsvd = v
		case "HugePages_Free:":
			free = v
		}
	}

	return rsvd, free
}

// TestNewFromFd_HugetlbReservesNothing reproduces the Citadel measurement
// (docs memhp-p0 R3): a plain MAP_SHARED view of a hugetlb memfd reserved the
// whole file. With MAP_NORESERVE the reservation must not move.
//
//nolint:paralleltest // reads the host-global HugePages_Rsvd counter
func TestNewFromFd_HugetlbReservesNothing(t *testing.T) {
	const pages = 8
	const hugePage = 2 << 20

	rsvdBefore, free := hugePagesRsvd(t)
	if rsvdBefore < 0 || free < pages {
		t.Skipf("needs %d free 2 MiB hugepages (free=%d)", pages, free)
	}

	fd, err := unix.MemfdCreate("test-hugetlb", unix.MFD_HUGETLB|unix.MFD_HUGE_2MB)
	if err != nil {
		t.Skipf("hugetlb memfd unavailable: %v", err)
	}
	require.NoError(t, unix.Ftruncate(fd, pages*hugePage))

	m, err := NewFromFd(fd)
	require.NoError(t, err)
	defer m.Close()

	rsvdAfter, _ := hugePagesRsvd(t)
	// Other processes on the host may move the counter; a plain MAP_SHARED
	// view would add all 8 pages at once.
	assert.Less(t, rsvdAfter-rsvdBefore, int64(pages), "mapping reserved the whole hugetlb file")
}

func TestMemfd_PunchHole(t *testing.T) {
	t.Parallel()

	page := int64(os.Getpagesize())
	m, data := newTestMemfd(t, 8*page)
	defer m.Close()

	var before unix.Stat_t
	require.NoError(t, unix.Fstat(m.fd, &before))

	require.NoError(t, m.PunchHole(2*page, 4*page))

	// Before reading: a read fault on a shmem hole allocates a page again.
	var after unix.Stat_t
	require.NoError(t, unix.Fstat(m.fd, &after))
	assert.Less(t, after.Blocks, before.Blocks, "the punched pages are released")

	got := make([]byte, 8*page)
	_, err := m.ReadAt(got, 0)
	require.NoError(t, err)

	assert.Equal(t, data[:2*page], got[:2*page], "before the hole is untouched")
	assert.Equal(t, make([]byte, 4*page), got[2*page:6*page], "the hole reads as zeros")
	assert.Equal(t, data[6*page:], got[6*page:], "after the hole is untouched")
	assert.Equal(t, 8*page, m.Size(), "KEEP_SIZE: the memfd keeps its length")

	require.Error(t, m.PunchHole(6*page, 4*page), "out of bounds")
	require.NoError(t, m.PunchHole(0, 0))
}

// TestMemfd_PunchHoleReleasesHugetlbPages is the host-side half of the
// virtio-mem unplug release: Firecracker keeps its own shared RW mapping of
// the hugetlb memfd and the orchestrator its read-only view, both with the
// pages mapped, and a hole punched through the orchestrator's fd must still
// return the pages to the pool (shared mappings do not pin hugetlb pages
// against PUNCH_HOLE: the kernel unmaps them from every VMA).
//
//nolint:paralleltest // reads the host-global HugePages_Free counter
func TestMemfd_PunchHoleReleasesHugetlbPages(t *testing.T) {
	const pages = 8
	const hugePage = 2 << 20

	_, free := hugePagesRsvd(t)
	if free < pages {
		t.Skipf("needs %d free 2 MiB hugepages (free=%d)", pages, free)
	}

	fd, err := unix.MemfdCreate("test-hugetlb-punch", unix.MFD_HUGETLB|unix.MFD_HUGE_2MB)
	if err != nil {
		t.Skipf("hugetlb memfd unavailable: %v", err)
	}
	require.NoError(t, unix.Ftruncate(fd, pages*hugePage))

	// The VMM's mapping: shared, writable, pages touched by the "guest".
	vmm, err := unix.Mmap(fd, 0, pages*hugePage, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_NORESERVE)
	require.NoError(t, err)
	defer func() { _ = unix.Munmap(vmm) }()
	for p := range pages {
		vmm[p*hugePage] = byte(p + 1)
	}

	dup, err := unix.Dup(fd)
	require.NoError(t, err)
	require.NoError(t, unix.Close(fd))
	m, err := NewFromFd(dup)
	require.NoError(t, err)
	defer m.Close()

	// The orchestrator's view has the pages mapped too.
	for p := range pages {
		require.Equal(t, byte(p+1), m.mmap[p*hugePage])
	}

	_, before := hugePagesRsvd(t)
	require.NoError(t, m.PunchHole(4*hugePage, 4*hugePage))
	_, after := hugePagesRsvd(t)

	// Other processes may move the counter by a page or two.
	assert.GreaterOrEqual(t, after-before, int64(3), "punching 4 touched hugepages must return them to the pool")
	assert.Equal(t, byte(1), vmm[0], "the unpunched half keeps its data")
	assert.Equal(t, byte(0), vmm[4*hugePage], "the punched half reads as zeros in the VMM's mapping")
}
