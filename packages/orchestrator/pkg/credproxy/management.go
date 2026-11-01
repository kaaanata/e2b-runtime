//go:build unix

package credproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ServeManagement exposes only masked control-account documents. Native account
// login and renewal are owned by credowner, never by this management endpoint.
func (p *Proxy) ServeManagement(ctx context.Context) error {
	lock, err := os.OpenFile(filepath.Join(p.Store.root, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("credential proxy already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(p.Store.root, "control.sock")
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /import", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "credential import retired; use the external native credowner login", http.StatusGone)
	})
	mux.HandleFunc("GET /guest/{provider}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		files, err := p.Store.Guest(r.Context(), Provider(r.PathValue("provider")))
		if err != nil {
			http.Error(w, "host login required", http.StatusServiceUnavailable)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(files)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Second}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
