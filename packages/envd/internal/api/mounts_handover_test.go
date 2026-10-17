package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/envd/internal/services/spec/upgrade"
)

// TestMountLedger_HandoverRoundTrip verifies the NFS mount ledger survives an
// Export -> Import cycle, so the new envd knows which paths were mounted (and for
// which lifecycle) after a live-upgrade.
func TestMountLedger_HandoverRoundTrip(t *testing.T) {
	t.Parallel()

	a := &API{}
	a.ImportMounts([]*upgrade.MountEntry{
		{Path: "/mnt/a", LifecycleId: "lc-1"},
		{Path: "/mnt/b", LifecycleId: "lc-2", SourceType: "nfs"},
		{Path: "/mnt/c", LifecycleId: "lc-3", SourceType: "block", Device: "/dev/vdb"},
	})

	got := map[string]*upgrade.MountEntry{}
	for _, m := range a.ExportMounts() {
		got[m.GetPath()] = m
	}
	require.Len(t, got, 3)
	// An entry from an envd predating source_type is exported as nfs: that is the
	// only kind it could have mounted, and the new envd must not treat it as a
	// block volume (which would put it in the freeze set).
	assert.Equal(t, "lc-1", got["/mnt/a"].GetLifecycleId())
	assert.Equal(t, "nfs", got["/mnt/a"].GetSourceType())
	assert.Empty(t, got["/mnt/a"].GetDevice())
	assert.Equal(t, "nfs", got["/mnt/b"].GetSourceType())
	assert.Equal(t, "lc-3", got["/mnt/c"].GetLifecycleId())
	assert.Equal(t, "block", got["/mnt/c"].GetSourceType())
	assert.Equal(t, "/dev/vdb", got["/mnt/c"].GetDevice())

	// Only the block mount is in the freeze set.
	assert.Equal(t, []string{"/mnt/c"}, a.blockMountpoints())
}

// TestImportedMountLedger_SkipsRemountForSameLifecycle is the point of carrying
// the ledger: setupVolumes consults it (loadMount + shouldRemountNFS), and a
// path carried across the upgrade with an unchanged lifecycle must NOT be
// remounted — leaving the still-live kernel mount in place (no ESTALE). A changed
// lifecycle still triggers a remount.
func TestImportedMountLedger_SkipsRemountForSameLifecycle(t *testing.T) {
	t.Parallel()

	a := &API{}
	a.ImportMounts([]*upgrade.MountEntry{{Path: "/mnt/a", LifecycleId: "lc-1"}})

	record, ok := a.loadMount("/mnt/a")
	require.True(t, ok, "carried mount must be present in the ledger")
	mountedLifecycle := record.LifecycleID

	assert.False(t, shouldRemountNFS(true, mountedLifecycle, "lc-1"),
		"a live mount carried across the upgrade must not be remounted for the same lifecycle")
	assert.True(t, shouldRemountNFS(true, mountedLifecycle, "lc-2"),
		"a lifecycle change must still trigger a remount")
}
