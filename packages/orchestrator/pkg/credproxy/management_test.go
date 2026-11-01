package credproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateManagementAndRetiredImport(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("", "cred-test-") //nolint:usetesting // t.TempDir exceeds macOS's 104-byte Unix socket path limit.
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	p, err := New(root, "owner", "/unused.sock")
	if err != nil {
		t.Fatal(err)
	}
	p.Store.owner = &fakeOwner{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeManagement(ctx) }()
	socket := filepath.Join(root, "control.sock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode().Perm() == 0o600 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not start")
		}
		time.Sleep(time.Millisecond)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unix/guest/codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || strings.Contains(string(b), "synthetic-access") {
		t.Fatal("management lease not masked")
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, "http://unix/import", strings.NewReader(`{"refresh_token":"must-not-be-imported"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	res, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatal("legacy import accepted")
	}
	second, err := New(root, "owner", "/unused.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err = second.ServeManagement(t.Context()); err == nil {
		t.Fatal("duplicate management owner")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}
