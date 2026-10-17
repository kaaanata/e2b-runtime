//go:build linux

package fc

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceDriveID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "workspace0", WorkspaceDriveID(0))
	assert.Equal(t, "workspace3", WorkspaceDriveID(3))
	assert.NotEqual(t, rootfsDriveID, WorkspaceDriveID(0), "a slot never shadows the rootfs drive")

	for i := range MaxWorkspaceSlots {
		slot, ok := workspaceSlot(WorkspaceDriveID(i))
		require.True(t, ok)
		assert.Equal(t, i, slot)
	}

	_, ok := workspaceSlot(WorkspaceDriveID(MaxWorkspaceSlots))
	assert.False(t, ok, "a slot past the reserved count is unknown")
	_, ok = workspaceSlot(rootfsDriveID)
	assert.False(t, ok)
}

func TestWorkspaceSlots(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		drives    []WorkspaceDrive
		wantSlots []int
		wantErr   string
	}{
		"none": {drives: nil, wantSlots: []int{}},
		"in order": {
			drives:    []WorkspaceDrive{{"workspace0", "/a"}, {"workspace1", "/b"}},
			wantSlots: []int{0, 1},
		},
		"any order, sparse": {
			drives:    []WorkspaceDrive{{"workspace3", "/a"}, {"workspace1", "/b"}},
			wantSlots: []int{3, 1},
		},
		"unknown id": {
			drives:  []WorkspaceDrive{{"rootfs", "/a"}},
			wantErr: "unknown workspace drive id",
		},
		"past the last slot": {
			drives:  []WorkspaceDrive{{WorkspaceDriveID(MaxWorkspaceSlots), "/a"}},
			wantErr: "unknown workspace drive id",
		},
		"duplicate": {
			drives:  []WorkspaceDrive{{"workspace0", "/a"}, {"workspace0", "/b"}},
			wantErr: "bound twice",
		},
		"relative path": {
			drives:  []WorkspaceDrive{{"workspace0", "a"}},
			wantErr: "absolute host path",
		},
		"empty path": {
			drives:  []WorkspaceDrive{{"workspace0", ""}},
			wantErr: "absolute host path",
		},
		"too many": {
			drives: []WorkspaceDrive{
				{"workspace0", "/a"}, {"workspace1", "/a"}, {"workspace2", "/a"}, {"workspace3", "/a"}, {"workspace0", "/a"},
			},
			wantErr: "exceed",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			slots, err := workspaceSlots(tt.drives)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSlots, slots)
		})
	}
}

func TestSortWorkspaceDrives(t *testing.T) {
	t.Parallel()

	in := []WorkspaceDrive{{"workspace3", "/d"}, {"bogus", "/x"}, {"workspace0", "/a"}, {"workspace2", "/c"}}
	got := sortWorkspaceDrives(in)

	assert.Equal(t, []WorkspaceDrive{{"workspace0", "/a"}, {"workspace2", "/c"}, {"workspace3", "/d"}, {"bogus", "/x"}}, got)
	assert.Equal(t, "workspace3", in[0].DriveID, "the caller's slice is not reordered")
	assert.Empty(t, sortWorkspaceDrives(nil))
}

// newDriveAPIStub serves PUT /drives/{id} on a unix socket, answering 204 the
// way Firecracker does, and records every drive body it receives by drive ID.
func newDriveAPIStub(t *testing.T) (*apiClient, map[string]map[string]any) {
	t.Helper()

	dir, err := os.MkdirTemp("", "fc") //nolint:usetesting // t.TempDir embeds the subtest name and overflows the unix sun_path limit
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := dir + "/fc.sock"
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)

	drives := map[string]map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /drives/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var drive map[string]any
		if !assert.NoError(t, err) || !assert.NoError(t, json.Unmarshal(body, &drive)) {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		drives[r.PathValue("id")] = drive
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return newApiClient(socket), drives
}

// TestSetWorkspaceDrive pins what a workspace slot looks like to Firecracker next
// to the rootfs: the same PUT with the same engine and limiter, never the root
// device. The device table of a snapshot is built from exactly these bodies.
func TestSetWorkspaceDrive(t *testing.T) {
	t.Parallel()

	c, drives := newDriveAPIStub(t)
	engine := "Sync"
	limiter := buildRateLimiter(RateLimiterConfig{
		Ops:       TokenBucketConfig{BucketSize: 1000, RefillTimeMs: 100},
		Bandwidth: TokenBucketConfig{BucketSize: -1},
	})

	require.NoError(t, c.setRootfsDrive(t.Context(), "/fc-vm/rootfs.ext4", &engine, limiter))
	require.NoError(t, c.setWorkspaceDrive(t.Context(), WorkspaceDriveID(1), "/fc-vm/workspace1.ext4", &engine, limiter))

	require.Contains(t, drives, "rootfs")
	require.Contains(t, drives, "workspace1")

	rootfs, ws := drives["rootfs"], drives["workspace1"]
	assert.Equal(t, "rootfs", rootfs["drive_id"])
	assert.Equal(t, true, rootfs["is_root_device"])

	assert.Equal(t, "workspace1", ws["drive_id"])
	assert.Equal(t, "/fc-vm/workspace1.ext4", ws["path_on_host"])
	assert.Equal(t, false, ws["is_root_device"], "a workspace slot is never the root device")
	// IsReadOnly=false is omitted on the wire (omitempty), exactly as for the rootfs.
	assert.NotEqual(t, true, ws["is_read_only"], "a workspace slot is writable")
	assert.Equal(t, rootfs["is_read_only"], ws["is_read_only"])
	assert.Equal(t, "Sync", ws["io_engine"])
	assert.Equal(t, rootfs["rate_limiter"], ws["rate_limiter"], "the slot gets the same limiter as the rootfs")
}
