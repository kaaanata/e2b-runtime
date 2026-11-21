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

func adminJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func adminError(w http.ResponseWriter, status int, err error) {
	adminJSON(w, status, map[string]string{"message": err.Error()})
}

func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		adminError(w, http.StatusBadRequest, errors.New("invalid request body"))

		return false
	}

	return true
}

// AdminHandler serves the administrative socket: account status, routing
// policy and browser sign-in. It has no lease endpoint, so a client of this
// socket can never obtain an access token.
func (m *Manager) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { adminJSON(w, 200, m.StatusWithQuota(r.Context())) })
	mux.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, _ *http.Request) { adminJSON(w, 200, m.Policy()) })
	mux.HandleFunc("PUT /v1/routes/{provider}", func(w http.ResponseWriter, r *http.Request) {
		var route Route
		if !decodeAdmin(w, r, &route) {
			return
		}
		if err := m.SetRoute(credproxy.Provider(r.PathValue("provider")), route); err != nil {
			adminError(w, http.StatusUnprocessableEntity, err)

			return
		}
		adminJSON(w, 200, m.Policy())
	})
	mux.HandleFunc("GET /v1/sandbox-controls/{id}/{provider}", func(w http.ResponseWriter, r *http.Request) {
		view, err := m.sandboxControl(credproxy.Provider(r.PathValue("provider")), r.PathValue("id"))
		if err != nil {
			adminError(w, http.StatusBadRequest, err)
			return
		}
		adminJSON(w, 200, view)
	})
	mux.HandleFunc("GET /v1/sandbox-controls/{id}/{provider}/guest/{account}", func(w http.ResponseWriter, r *http.Request) {
		p := credproxy.Provider(r.PathValue("provider"))
		if _, err := m.sandboxControl(p, r.PathValue("id")); err != nil {
			adminError(w, http.StatusBadRequest, err)
			return
		}
		_, files, err := m.validateControlTarget(r.Context(), p, r.PathValue("account"))
		if err != nil {
			adminError(w, http.StatusUnprocessableEntity, err)
			return
		}
		adminJSON(w, 200, files)
	})
	mux.HandleFunc("POST /v1/sandbox-controls/{id}/{provider}/{step}", m.switchSandboxControl)
	mux.HandleFunc("POST /v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		var body storedAccount
		if !decodeAdmin(w, r, &body) {
			return
		}
		if err := m.AddAccount(body.Name, body.Provider); err != nil {
			adminError(w, http.StatusUnprocessableEntity, err)

			return
		}
		adminJSON(w, 200, m.Policy())
	})
	mux.HandleFunc("DELETE /v1/accounts/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.RemoveAccount(r.PathValue("name")); err != nil {
			adminError(w, http.StatusUnprocessableEntity, err)

			return
		}
		adminJSON(w, 200, m.Policy())
	})
	mux.HandleFunc("POST /v1/accounts/{name}/login", func(w http.ResponseWriter, r *http.Request) {
		// The sign-in outlives this request on purpose; it has its own timeout
		// and is canceled with DELETE /v1/logins/{id} or on shutdown.
		v, err := m.StartLogin(r.PathValue("name")) //nolint:contextcheck // detached by design
		if err != nil {
			adminError(w, http.StatusConflict, err)

			return
		}
		adminJSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/logins/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := m.LoginStatus(r.PathValue("id"))
		if !ok {
			adminError(w, http.StatusNotFound, errors.New("unknown sign-in"))

			return
		}
		adminJSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/logins/{id}/code", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		if !decodeAdmin(w, r, &body) {
			return
		}
		v, err := m.SubmitLoginCode(r.PathValue("id"), body.Code)
		if err != nil {
			adminError(w, http.StatusConflict, err)

			return
		}
		adminJSON(w, 200, v)
	})
	mux.HandleFunc("DELETE /v1/logins/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.CancelLogin(r.PathValue("id"))
		if err != nil {
			adminError(w, http.StatusNotFound, err)

			return
		}
		adminJSON(w, 200, v)
	})

	return mux
}

func listenPrivate(ctx context.Context, path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		ln.Close()

		return nil, err
	}

	return ln, nil
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
	ln, err := listenPrivate(ctx, filepath.Join(m.config.Root, "owner.sock"))
	if err != nil {
		return err
	}
	defer ln.Close()
	adminLn, err := listenPrivate(ctx, filepath.Join(m.config.Root, "admin.sock"))
	if err != nil {
		return err
	}
	defer adminLn.Close()
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
	admin := &http.Server{Handler: m.AdminHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return child }}
	stop := context.AfterFunc(child, func() { _ = server.Close(); _ = admin.Close() })
	defer stop()
	workers.Go(func() {
		if err := admin.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
	})
	err = server.Serve(ln)
	cancel()
	m.cancelLogins()
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

func (m *Manager) switchSandboxControl(w http.ResponseWriter, r *http.Request) {
	p, id := credproxy.Provider(r.PathValue("provider")), r.PathValue("id")
	view, err := m.sandboxControl(p, id)
	if err != nil {
		adminError(w, http.StatusBadRequest, err)
		return
	}
	var body sandboxControlSwitch
	if !decodeAdmin(w, r, &body) {
		return
	}
	switch r.PathValue("step") {
	case "stage":
		err = m.stageSandboxControl(r.Context(), p, id, body)
	case "commit":
		if view.Pending == nil || view.Pending.From != body.From || view.Pending.To != body.To ||
			view.Pending.PreviousFingerprint != body.PreviousFingerprint || view.Pending.Fingerprint != body.Fingerprint {
			err = errors.New("the prepared switch does not match")
		} else {
			err = m.setSandboxControl(r.Context(), p, id, body.From, body.To)
		}
	case "finish":
		err = m.finishSandboxControl(p, id, body.To)
	default:
		adminError(w, http.StatusNotFound, errors.New("unknown sandbox control switch step"))
		return
	}
	if err != nil {
		adminError(w, http.StatusConflict, err)
		return
	}
	view, err = m.sandboxControl(p, id)
	if err != nil {
		adminError(w, http.StatusConflict, err)
		return
	}
	adminJSON(w, 200, view)
}
