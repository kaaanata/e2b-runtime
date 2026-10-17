package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAPIWithBlockMounts is an API whose ledger holds two block volumes (one
// nested under the other) and an NFS volume, against a fake freezer. The
// expected freeze order is the nested block volume, its parent, then the rootfs;
// the NFS mount is never part of the set.
func newAPIWithBlockMounts(f *fakeFreezer) *API {
	api := newAPIWithFreezer(f)
	api.storeMount("/data", mountRecord{LifecycleID: "lc", SourceType: Block, Device: "/dev/vdb"})
	api.storeMount("/data/deps", mountRecord{LifecycleID: "lc", SourceType: Block, Device: "/dev/vdc"})
	api.storeMount("/shared", mountRecord{LifecycleID: "lc", SourceType: Nfs})

	return api
}

func postFreezeSet(t *testing.T, api *API, path, token string) (int, FsFreezeSetResult) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"operation_token":"`+token+`"}`))
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	switch path {
	case "/fsfreeze-set":
		api.PostFsfreezeSet(rec, req)
	case "/fsthaw-set":
		api.PostFsthawSet(rec, req)
	default:
		t.Fatalf("unknown path %s", path)
	}

	var res FsFreezeSetResult
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res), rec.Body.String())
	}

	return rec.Code, res
}

func getFreezeSet(t *testing.T, api *API, token string) (int, FsFreezeSetResult) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/fsfreeze-set?operation_token="+token, http.NoBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	api.GetFsfreezeSet(rec, req, GetFsfreezeSetParams{OperationToken: token})

	var res FsFreezeSetResult
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	}

	return rec.Code, res
}

func TestPostFsfreezeSet(t *testing.T) {
	t.Parallel()

	t.Run("freezes block volumes deepest first, then the rootfs", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)

		code, res := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"/data/deps", "/data", "/"}, f.frozen)
		assert.Equal(t, Frozen, res.State)
		assert.Equal(t, []string{"/data/deps", "/data", "/"}, res.Frozen)
		require.NotNil(t, res.Mountpoints)
		assert.Equal(t, []string{"/data/deps", "/data", "/"}, *res.Mountpoints)
		assert.Empty(t, f.thawed)
	})

	t.Run("with no block volumes the set is the rootfs alone", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithFreezer(f)

		code, res := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"/"}, f.frozen)
		assert.Equal(t, []string{"/"}, res.Frozen)
	})

	t.Run("partial failure thaws what froze in reverse and reports it", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{freezeFailOn: map[string]error{"/data": errors.New("FIFREEZE /data: io error")}}
		api := newAPIWithBlockMounts(f)

		code, res := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusInternalServerError, code)
		assert.Equal(t, Failed, res.State)
		assert.Equal(t, []string{"/data/deps"}, res.Frozen, "what had frozen before the failure")
		require.NotNil(t, res.FailedMountpoint)
		assert.Equal(t, "/data", *res.FailedMountpoint)
		require.NotNil(t, res.Error)
		assert.Contains(t, *res.Error, "io error")
		require.NotNil(t, res.Thawed)
		assert.Equal(t, []string{"/data/deps"}, *res.Thawed)
		assert.Nil(t, res.ThawErrors)
		// The kernel never saw a freeze of / : the rootfs is last, and the failure
		// stopped the walk before it.
		assert.Equal(t, []string{"/data/deps"}, f.frozen)
		assert.Equal(t, []string{"/data/deps"}, f.thawed)

		// The record is queryable afterwards.
		code, got := getFreezeSet(t, api, "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Failed, got.State)
	})

	t.Run("rollback thaw failure is reported, not hidden", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{
			freezeFailOn: map[string]error{"/": errors.New("FIFREEZE /: io error")},
			thawFailOn:   map[string]error{"/data/deps": errors.New("FITHAW /data/deps: io error")},
		}
		api := newAPIWithBlockMounts(f)

		code, res := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusInternalServerError, code)
		assert.Equal(t, Failed, res.State)
		assert.Equal(t, []string{"/data/deps", "/data"}, res.Frozen)
		// Reverse order: /data first, then /data/deps which fails.
		assert.Equal(t, []string{"/data"}, f.thawed)
		require.NotNil(t, res.ThawErrors)
		assert.Equal(t, []string{"/data/deps"}, *res.ThawErrors)
	})

	t.Run("repeating a token whose set is frozen is a no-op returning the record", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)

		code, first := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		code, second := postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, first, second)
		assert.Len(t, f.frozen, 3, "the kernel saw exactly one freeze per mountpoint")
	})

	t.Run("rejects a missing or oversized token", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)

		for _, body := range []string{`{}`, `{"operation_token":""}`, `{"operation_token":"` + strings.Repeat("x", 129) + `"}`, `{`} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/fsfreeze-set", strings.NewReader(body))
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			api.PostFsfreezeSet(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		}
		assert.Empty(t, f.frozen)
	})

	t.Run("returns 503 while the freeze lock is held", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)
		require.NoError(t, api.fsFreezeLock.Acquire(t.Context(), 1))
		defer api.fsFreezeLock.Release(1)

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // an already-cancelled request context makes the lock acquire fail at once
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/fsfreeze-set", strings.NewReader(`{"operation_token":"op-1"}`))
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		api.PostFsfreezeSet(rec, req)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Empty(t, f.frozen)
	})
}

func TestGetFsfreezeSet(t *testing.T) {
	t.Parallel()

	t.Run("unknown token is 404", func(t *testing.T) {
		t.Parallel()
		api := newAPIWithBlockMounts(&fakeFreezer{})
		code, _ := getFreezeSet(t, api, "never-ran")
		assert.Equal(t, http.StatusNotFound, code)
	})

	t.Run("a frozen set reads back frozen", func(t *testing.T) {
		t.Parallel()
		api := newAPIWithBlockMounts(&fakeFreezer{})
		_, _ = postFreezeSet(t, api, "/fsfreeze-set", "op-1")

		code, res := getFreezeSet(t, api, "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Frozen, res.State)
		assert.Equal(t, []string{"/data/deps", "/data", "/"}, res.Frozen)
	})

	t.Run("records are evicted after many later operations", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithFreezer(f)
		_, _ = postFreezeSet(t, api, "/fsfreeze-set", "old")
		for range maxFreezeOps {
			_, _ = postFreezeSet(t, api, "/fsthaw-set", "later")
			_, _ = postFreezeSet(t, api, "/fsfreeze-set", "later-"+strings.Repeat("x", len(f.frozen)))
		}
		code, _ := getFreezeSet(t, api, "old")
		assert.Equal(t, http.StatusNotFound, code)
	})
}

func TestPostFsthawSet(t *testing.T) {
	t.Parallel()

	t.Run("thaws exactly what froze, rootfs first", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)
		_, _ = postFreezeSet(t, api, "/fsfreeze-set", "op-1")

		// A block volume mounted after the freeze must not be thawed by it: the
		// thaw follows the recorded set, not the current ledger.
		api.storeMount("/late", mountRecord{LifecycleID: "lc", SourceType: Block, Device: "/dev/vdz"})

		code, res := postFreezeSet(t, api, "/fsthaw-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Thawed, res.State)
		assert.Equal(t, []string{"/", "/data", "/data/deps"}, f.thawed)
		require.NotNil(t, res.Thawed)
		assert.Equal(t, []string{"/", "/data", "/data/deps"}, *res.Thawed)
		assert.Nil(t, res.ThawErrors)

		code, got := getFreezeSet(t, api, "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Thawed, got.State)
	})

	t.Run("an unknown token thaws the whole current set", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)

		code, res := postFreezeSet(t, api, "/fsthaw-set", "lost")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Thawed, res.State)
		assert.Equal(t, []string{"/", "/data", "/data/deps"}, f.thawed)
		require.NotNil(t, res.Mountpoints)
		assert.Equal(t, []string{"/data/deps", "/data", "/"}, *res.Mountpoints)
	})

	t.Run("a thaw failure keeps the state frozen and names the mountpoint", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{thawFailOn: map[string]error{"/data": errors.New("FITHAW /data: io error")}}
		api := newAPIWithBlockMounts(f)
		_, _ = postFreezeSet(t, api, "/fsfreeze-set", "op-1")

		code, res := postFreezeSet(t, api, "/fsthaw-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Frozen, res.State, "something is still frozen; the caller must not resume")
		require.NotNil(t, res.ThawErrors)
		assert.Equal(t, []string{"/data"}, *res.ThawErrors)
		assert.Equal(t, []string{"/", "/data/deps"}, f.thawed, "every other mountpoint was still attempted")

		// A retry after the fault clears succeeds and ends thawed.
		f.mu.Lock()
		f.thawFailOn = nil
		f.mu.Unlock()
		code, res = postFreezeSet(t, api, "/fsthaw-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Thawed, res.State)
		assert.Nil(t, res.ThawErrors)
	})

	t.Run("a repeated thaw is idempotent", func(t *testing.T) {
		t.Parallel()
		f := &fakeFreezer{}
		api := newAPIWithBlockMounts(f)
		_, _ = postFreezeSet(t, api, "/fsfreeze-set", "op-1")
		_, _ = postFreezeSet(t, api, "/fsthaw-set", "op-1")
		code, res := postFreezeSet(t, api, "/fsthaw-set", "op-1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, Thawed, res.State)
		assert.Len(t, f.thawed, 6)
	})
}

// TestPostFsfreeze_RootfsOnlyUnchanged pins the original single-mountpoint
// endpoints: they keep freezing / alone whatever the ledger holds, because an
// orchestrator that only knows them is taking a rootfs-only snapshot.
func TestPostFsfreeze_RootfsOnlyUnchanged(t *testing.T) {
	t.Parallel()

	f := &fakeFreezer{}
	api := newAPIWithBlockMounts(f)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/fsfreeze", http.NoBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	api.PostFsfreeze(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{rootfsMountpoint}, f.frozen)
}
