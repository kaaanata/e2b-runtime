package credentialowner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

type captureTransport func(*http.Request) (*http.Response, error)

func (f captureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPrivateOwnerSocketToProxyLease(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("", "owner-bridge-") //nolint:usetesting // t.TempDir exceeds macOS's 104-byte Unix socket path limit.
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	config := Config{Version: 1, Root: root, Accounts: []Account{{Name: "control", Provider: credproxy.Claude, Binary: "/usr/bin/claude"}, {Name: "worker", Provider: credproxy.Claude, Binary: "/usr/bin/claude"}}, Routes: map[credproxy.Provider]Route{credproxy.Claude: {Control: "control", Inference: []string{"worker"}}}}
	m, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range m.accounts {
		writeFixture(t, a, name, time.Now().Add(time.Hour))
		state, err := readNative(a.config, a.home)
		if err != nil {
			t.Fatal(err)
		}
		if err = bindIdentity(a, state.credential, true); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Serve(ctx) }()
	socket := filepath.Join(root, "owner.sock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode().Perm() == 0o600 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner socket not ready")
		}
		time.Sleep(time.Millisecond)
	}
	p, err := credproxy.New(filepath.Join(root, "proxy"), "team", socket)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/oauth/profile", "/v1/messages"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com"+path, strings.NewReader(`{"metadata":{"user_id":"{\"session_id\":\"thread\",\"account_uuid\":\"control\"}"}}`))
		r.Header.Set("Authorization", "Bearer "+credproxy.ClaudeAccess)
		want := "control"
		if path == "/v1/messages" {
			want = "worker"
		}
		state, err := readNative(m.accounts[want].config, m.accounts[want].home)
		if err != nil {
			t.Fatal(err)
		}
		transport := captureTransport(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "Bearer "+state.credential.AccessToken {
				t.Fatal("wrong owner lease injected")
			}

			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		w := httptest.NewRecorder()
		p.Handler("api.anthropic.com", transport, "trusted-sandbox").ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("private owner bridge HTTP %d", w.Code)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not stop")
	}
}
