package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNFSMountOptionsForProfile(t *testing.T) {
	t.Parallel()

	t.Run("empty and unknown profiles send nothing, so envd uses its built-in set", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, nfsMountOptionsForProfile(""))
		assert.Nil(t, nfsMountOptionsForProfile("eventual"))
	})

	t.Run("strict is the built-in set plus nolock", func(t *testing.T) {
		t.Parallel()
		got := nfsMountOptionsForProfile(VolumeConsistencyStrict)
		assert.Equal(t, []string{
			"sync", "rsize=1048576", "wsize=1048576", "mountproto=tcp", "mountport=2049",
			"proto=tcp", "port=2049", "nfsvers=3", "noacl", "noac", "lookupcache=none", "nolock",
		}, got)
	})

	t.Run("cto drops the two caching knobs and adds nolock", func(t *testing.T) {
		t.Parallel()
		got := nfsMountOptionsForProfile(VolumeConsistencyCto)
		assert.Equal(t, []string{
			"sync", "rsize=1048576", "wsize=1048576", "mountproto=tcp", "mountport=2049",
			"proto=tcp", "port=2049", "nfsvers=3", "noacl", "nolock",
		}, got)
		assert.NotContains(t, got, "noac")
		assert.NotContains(t, got, "lookupcache=none")
		assert.Contains(t, got, "sync", "cto keeps sync; async is a separate, undecided profile")
	})

	t.Run("profiles return fresh slices", func(t *testing.T) {
		t.Parallel()
		a := nfsMountOptionsForProfile(VolumeConsistencyStrict)
		a[0] = "mutated"
		assert.Equal(t, "sync", nfsMountOptionsForProfile(VolumeConsistencyStrict)[0])
	})
}
