//go:build unix

package credproxy

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestPrivateManagementSocket(t *testing.T) {
	t.Parallel()
	// Keep the Unix socket path below macOS's 104-byte sockaddr limit.
	root, err := os.MkdirTemp("", "cp-test-") //nolint:usetesting // t.TempDir exceeds macOS sockaddr_un path length for this test.
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	p, err := New(root, "owner")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeManagement(ctx) }()
	socket := filepath.Join(root, "control.sock")
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, e := os.Stat(socket)
		if e == nil && info.Mode().Perm() == 0o600 {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("management start: %v", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not start")
		}
		time.Sleep(time.Millisecond)
	}
	c := credential(Claude)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://unix/import", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal("import failed")
	}
	if strings.Contains(string(body), c.AccessToken) || strings.Contains(string(body), c.RefreshToken) {
		t.Fatal("management returned real token")
	}
	second, err := New(root, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = second.ServeManagement(t.Context()); err == nil {
		t.Fatal("two refresh owners admitted")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("management did not stop")
	}
}
