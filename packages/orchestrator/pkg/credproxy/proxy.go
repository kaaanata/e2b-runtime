package credproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	Store     *Store
	authority *authority
	team      string
}

func New(root, team string) (*Proxy, error) {
	if team == "" {
		return nil, errors.New("CREDPROXY_TEAM_ID is required")
	}
	store, err := OpenStore(root)
	if err != nil {
		return nil, err
	}
	ca, err := loadAuthority(root)
	if err != nil {
		return nil, err
	}

	return &Proxy{Store: store, authority: ca, team: team}, nil
}
func (p *Proxy) CABundle() string            { return p.authority.bundle }
func (p *Proxy) EnabledFor(team string) bool { return team != "" && team == p.team }
func (p *Proxy) Intercepts(host string) bool {
	switch host {
	case "api.anthropic.com", "platform.claude.com", "chatgpt.com", "auth.openai.com":
		return true
	}

	return false
}

func provider(host string) Provider {
	if host == "api.anthropic.com" || host == "platform.claude.com" {
		return Claude
	}

	return Codex
}

// Handler binds authority to the verified TLS SNI. A guest Host header never
// controls where a credential is sent. ReverseProxy streams SSE and upgrades.
func (p *Proxy) Handler(host string, transport http.RoundTripper) http.Handler {
	reverse := &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0), ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "credential proxy upstream unavailable", http.StatusBadGateway)
	}, Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme = "https"
		r.Out.URL.Host = host
		r.Out.Host = host
		r.Out.Header.Del("Proxy-Authorization")
		// Never trust a guest-supplied forwarding chain at the provider boundary.
		r.Out.Header.Del("Forwarded")
		r.Out.Header.Del("X-Forwarded-For")
		r.Out.Header.Del("X-Forwarded-Host")
		r.Out.Header.Del("X-Forwarded-Proto")
	}}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.Intercepts(host) || (r.Host != host && r.Host != host+":443") || (r.URL.IsAbs() && r.URL.Host != host && r.URL.Host != host+":443") {
			http.Error(w, "authority mismatch", http.StatusBadRequest)

			return
		}
		endpoint := (host == "platform.claude.com" && r.URL.Path == "/v1/oauth/token") || (host == "auth.openai.com" && r.URL.Path == "/oauth/token")
		if endpoint {
			p.refreshResponse(w, r, provider(host))

			return
		}
		marker := r.Header.Get("Authorization") == "Bearer "+provider(host).AccessSentinel() || r.Header.Get("X-Api-Key") == provider(host).AccessSentinel()
		if marker {
			allowed := host == "api.anthropic.com" || (host == "chatgpt.com" && strings.HasPrefix(r.URL.Path, "/backend-api/"))
			if !allowed {
				http.Error(w, "credential destination refused", http.StatusForbidden)

				return
			}
			c, err := p.Store.Current(r.Context(), provider(host))
			if err != nil {
				http.Error(w, "login or refresh required on host", http.StatusServiceUnavailable)

				return
			}
			r = r.Clone(r.Context())
			if r.Header.Get("Authorization") == "Bearer "+c.Provider.AccessSentinel() {
				r.Header.Set("Authorization", "Bearer "+c.AccessToken)
			}
			if r.Header.Get("X-Api-Key") == c.Provider.AccessSentinel() {
				r.Header.Set("X-Api-Key", c.AccessToken)
			}
		} else if strings.Contains(r.Header.Get("Authorization"), "sbx-proxy-managed") || strings.Contains(r.Header.Get("X-Api-Key"), "sbx-proxy-managed") {
			http.Error(w, "wrong provider sentinel", http.StatusForbidden)

			return
		}
		// Non-sentinel session tokens, including both Claude RC path families and
		// Codex WebSocket tokens, are passed through without account-token rewrite.
		reverse.ServeHTTP(w, r)
	})
}

func (p *Proxy) refreshResponse(w http.ResponseWriter, r *http.Request, prov Provider) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var grant, token, client string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var v struct {
			Grant  string `json:"grant_type"`
			Token  string `json:"refresh_token"`
			Client string `json:"client_id"`
		}
		if json.NewDecoder(r.Body).Decode(&v) != nil {
			http.Error(w, "invalid refresh", http.StatusBadRequest)

			return
		}
		grant, token, client = v.Grant, v.Token, v.Client
	} else {
		if r.ParseForm() != nil {
			http.Error(w, "invalid refresh", http.StatusBadRequest)

			return
		}
		grant, token, client = r.PostForm.Get("grant_type"), r.PostForm.Get("refresh_token"), r.PostForm.Get("client_id")
	}
	expected := "app_EMoamEEZ73f0CkXaXp7hrann"
	if prov == Claude {
		expected = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	}
	if grant != "refresh_token" || token != prov.RefreshSentinel() || client != expected {
		http.Error(w, "use sbx host login", http.StatusForbidden)

		return
	}
	c, err := p.Store.Refresh(r.Context(), prov)
	if err != nil {
		http.Error(w, "host credential unavailable", http.StatusServiceUnavailable)

		return
	}
	out := map[string]any{"access_token": prov.AccessSentinel(), "refresh_token": prov.RefreshSentinel(), "token_type": "Bearer", "expires_in": max(int64(time.Until(c.ExpiresAt).Seconds()), 1), "scope": c.Scope}
	if prov == Codex {
		id, err := publicIDToken(c.IDToken)
		if err != nil {
			http.Error(w, "invalid host identity", http.StatusServiceUnavailable)

			return
		}
		out["id_token"] = id
	}
	raw, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "cannot encode masked refresh", http.StatusInternalServerError)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// ServeConn consumes an admitted transparent-proxy connection. ServeTLS uses
// net/http's HTTP/2 support (h2 ALPN) and keeps WebSocket/H1 upgrade support.
func (p *Proxy) ServeConn(ctx context.Context, conn net.Conn, host string, transport http.RoundTripper) {
	defer conn.Close()
	cert, err := p.authority.leaf(host)
	if err != nil {
		return
	}
	tracked := &closeConn{Conn: conn, done: make(chan struct{})}
	ln := &singleListener{conn: tracked}
	server := &http.Server{Handler: p.Handler(host, transport), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20, ErrorLog: log.New(io.Discard, "", 0), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	defer server.Close()
	_ = server.ServeTLS(ln, "", "")
}

type closeConn struct {
	net.Conn

	once sync.Once
	done chan struct{}
}

func (c *closeConn) Close() error {
	c.once.Do(func() { close(c.done) })

	return c.Conn.Close()
}

type singleListener struct {
	conn     *closeConn
	accepted bool
}

func (l *singleListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true

		return l.conn, nil
	}
	<-l.conn.done

	return nil, net.ErrClosed
}
func (l *singleListener) Close() error   { return l.conn.Close() }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }
