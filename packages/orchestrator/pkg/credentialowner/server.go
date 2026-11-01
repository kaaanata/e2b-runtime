package credentialowner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/resolve", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var request credproxy.ResolveRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&request) != nil {
			http.Error(w, "invalid lease request", http.StatusBadRequest)

			return
		}
		lease, err := m.Resolve(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lease)
	})
	mux.HandleFunc("POST /v1/outcome", func(w http.ResponseWriter, r *http.Request) {
		var outcome credproxy.Outcome
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&outcome) != nil {
			http.Error(w, "invalid outcome", http.StatusBadRequest)

			return
		}
		if err := m.Observe(r.Context(), outcome); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)

			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(m.Status())
	})

	return mux
}

func (m *Manager) Serve(ctx context.Context) error {
	lock, err := os.OpenFile(filepath.Join(m.config.Root, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("native credential owner already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(m.config.Root, "owner.sock")
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
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	workers.Go(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			m.Sweep(child)
			select {
			case <-child.Done():
				return
			case <-ticker.C:
			}
		}
	})
	server := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return child }}
	stop := context.AfterFunc(child, func() { _ = server.Close() })
	defer stop()
	err = server.Serve(ln)
	cancel()
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
