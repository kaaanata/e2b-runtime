//go:build linux

package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
)

// fakeFreezeSetEnvd serves /fsfreeze-set, /fsthaw-set and /volumes/resize the
// way envd does, with a scripted freeze outcome, and records every call.
type fakeFreezeSetEnvd struct {
	mu     sync.Mutex
	calls  []string
	freeze func(token string) (int, envd.FsFreezeSetResult)
	resize int
}

func (f *fakeFreezeSetEnvd) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		writeResult := func(code int, res envd.FsFreezeSetResult) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(res)
		}

		switch r.Method + " " + r.URL.Path {
		case "POST /fsfreeze-set":
			var req envd.FsFreezeSetRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if f.freeze == nil {
				http.NotFound(w, r)

				return
			}
			code, res := f.freeze(req.OperationToken)
			writeResult(code, res)
		case "GET /fsfreeze-set":
			_, res := f.freeze(r.URL.Query().Get("operation_token"))
			writeResult(http.StatusOK, res)
		case "POST /fsthaw-set":
			var req envd.FsFreezeSetRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			writeResult(http.StatusOK, envd.FsFreezeSetResult{OperationToken: req.OperationToken, State: envd.Thawed, Frozen: []string{}})
		case "POST /volumes/resize":
			var req envd.VolumeResizeRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			f.mu.Lock()
			code := f.resize
			f.mu.Unlock()
			if code == 0 {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(envd.VolumeResizeResult{Path: req.Path, Device: "/dev/vdb"})
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeFreezeSetEnvd) setResize(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resize = code
}

func (f *fakeFreezeSetEnvd) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func frozenSet(token string, mounts ...string) envd.FsFreezeSetResult {
	return envd.FsFreezeSetResult{OperationToken: token, State: envd.Frozen, Mountpoints: mounts, Frozen: mounts}
}

func TestGuestFreezeMountSetForPause(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		freeze   func(token string) (int, envd.FsFreezeSetResult)
		wantErr  error
		wantFail bool
	}{
		"whole set frozen": {
			freeze: func(tok string) (int, envd.FsFreezeSetResult) {
				return http.StatusOK, frozenSet(tok, "/workspace0", "/")
			},
		},
		"failed freeze was rolled back by envd": {
			freeze: func(tok string) (int, envd.FsFreezeSetResult) {
				return http.StatusInternalServerError, envd.FsFreezeSetResult{OperationToken: tok, State: envd.Failed, Mountpoints: []string{"/workspace0", "/"}, Frozen: []string{"/workspace0"}, FailedMountpoint: "/", Error: "EBUSY"}
			},
			wantFail: true,
		},
		"set missing a workspace mount": {
			freeze: func(tok string) (int, envd.FsFreezeSetResult) {
				return http.StatusOK, frozenSet(tok, "/")
			},
			wantFail: true,
		},
		"set reports a mountpoint unfrozen": {
			freeze: func(tok string) (int, envd.FsFreezeSetResult) {
				res := frozenSet(tok, "/workspace0", "/")
				res.Frozen = []string{"/workspace0"}

				return http.StatusOK, res
			},
			wantFail: true,
		},
		"envd without the mount-set freeze": {
			wantErr: ErrCapabilityUnsupported,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeFreezeSetEnvd{freeze: tc.freeze}
			srv := httptest.NewServer(fake.handler(t))
			t.Cleanup(srv.Close)

			fx := newWorkspaceFixture(t)
			s := newWorkspaceSandbox(t, fx, srv.URL)
			cleanup := NewCleanup()

			err := s.guestFreezeMountSetForPause(t.Context(), cleanup, "build-1")

			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			case tc.wantFail:
				require.Error(t, err)
			default:
				require.NoError(t, err)
			}

			assert.Equal(t, []string{"POST /fsfreeze-set"}, fake.seen(), "no thaw until the pause aborts")

			// Pause runs the cleanup chain on every failure; the thaw under the
			// same token was registered before the freeze was attempted.
			require.NoError(t, cleanup.Run(t.Context()))
			if tc.freeze != nil {
				assert.Equal(t, []string{"POST /fsfreeze-set", "POST /fsthaw-set"}, fake.seen())
			}
		})
	}
}

// TestGuestFreezeMountSetForPause_LostResponse: a freeze whose HTTP response
// was lost is resolved by asking envd what the token recorded.
func TestGuestFreezeMountSetForPause_LostResponse(t *testing.T) {
	t.Parallel()

	var once sync.Once
	fake := &fakeFreezeSetEnvd{freeze: func(tok string) (int, envd.FsFreezeSetResult) {
		return http.StatusOK, frozenSet(tok, "/workspace0", "/")
	}}
	inner := fake.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/fsfreeze-set" {
			dropped := false
			once.Do(func() {
				dropped = true
				// The freeze "happened" but the connection dies before a response.
				hj, ok := w.(http.Hijacker)
				require.True(t, ok)
				conn, _, err := hj.Hijack()
				require.NoError(t, err)
				_ = conn.Close()
			})
			if dropped {
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	fx := newWorkspaceFixture(t)
	s := newWorkspaceSandbox(t, fx, srv.URL)

	require.NoError(t, s.guestFreezeMountSetForPause(t.Context(), NewCleanup(), "build-1"))
	assert.Equal(t, []string{"GET /fsfreeze-set"}, fake.seen(), "the recorded outcome decides")
}

func TestResizeWorkspaceFilesystems(t *testing.T) {
	t.Parallel()

	fake := &fakeFreezeSetEnvd{}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	fx := newWorkspaceFixture(t)
	s := newFsFreezeSandbox(t, srv.URL)

	vol := fx.volumeFor(0, fx.inc)
	vol.ResizeToBytes = 2 * gib
	ws, err := fx.plan(t, blockTemplate(1, pairedRevision(0, fx.fsUUID)), []WorkspaceVolume{vol}, workspaceMemoryRestore)
	require.NoError(t, err)
	s.workspace = ws

	require.NoError(t, s.resizeWorkspaceFilesystems(t.Context()))
	assert.Equal(t, []string{"POST /volumes/resize"}, fake.seen())

	fake.setResize(http.StatusInternalServerError)
	require.Error(t, s.resizeWorkspaceFilesystems(t.Context()))

	fake.setResize(http.StatusNotFound)
	require.ErrorIs(t, s.resizeWorkspaceFilesystems(t.Context()), ErrCapabilityUnsupported)

	plain := newFsFreezeSandbox(t, srv.URL)
	require.NoError(t, plain.resizeWorkspaceFilesystems(t.Context()), "nothing to grow")
}
