package credproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func privateDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}

	return root
}

func testProxy(t *testing.T) *Proxy {
	t.Helper()
	p, err := New(privateDir(t), "owner")
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func identity() string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_plan_type":"pro"},"secret_claim":"must-not-leak"}`)) + ".real-signature"
}

func credential(p Provider) Credential {
	return Credential{Provider: p, AccountID: "account", AccessToken: "real-access-secret", RefreshToken: "real-refresh-secret", IDToken: identity(), ExpiresAt: time.Now().Add(time.Hour), Scope: "user:profile user:inference user:sessions:claude_code"}
}

func importCredential(t *testing.T, p *Proxy, c Credential) {
	t.Helper()
	if _, err := p.Store.Import(c, false); err != nil {
		t.Fatal(err)
	}
}

func TestGuestDocumentsNeverContainRealTokens(t *testing.T) {
	t.Parallel()
	for _, prov := range []Provider{Claude, Codex} {
		t.Run(string(prov), func(t *testing.T) {
			t.Parallel()
			c := credential(prov)
			files, err := c.GuestFiles()
			if err != nil {
				t.Fatal(err)
			}
			raw := string(files.Credentials)
			for _, secret := range []string{c.AccessToken, c.RefreshToken, c.IDToken, "must-not-leak", "real-signature"} {
				if strings.Contains(raw, secret) {
					t.Fatal("secret escaped into guest document")
				}
			}
			if !strings.Contains(raw, prov.AccessSentinel()) || !strings.Contains(raw, prov.RefreshSentinel()) {
				t.Fatal("missing sentinel")
			}
			if prov == Codex {
				var guest struct {
					Mode   string `json:"auth_mode"`
					Tokens struct {
						ID string `json:"id_token"`
					} `json:"tokens"`
				}
				if json.Unmarshal(files.Credentials, &guest) != nil || guest.Mode != "chatgpt" {
					t.Fatal("not ChatGPT auth")
				}
				parts := strings.Split(guest.Tokens.ID, ".")
				if len(parts) != 3 || parts[2] != "c2J4LXByb3h5LW1hbmFnZWQ" {
					t.Fatal("Codex requires a non-empty metadata placeholder segment")
				}
				payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(guest.Tokens.ID, ".")[1])
				if strings.Contains(string(payload), "secret_claim") {
					t.Fatal("private claim copied")
				}
			}
		})
	}
}

func TestOnlyExactProviderSentinelsAreRewritten(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	importCredential(t, p, credential(Claude))
	importCredential(t, p, credential(Codex))
	for _, test := range []struct {
		host, path, auth, want string
		status                 int
	}{
		{"api.anthropic.com", "/v1/messages", "Bearer " + ClaudeAccess, "Bearer real-access-secret", 200},
		{"api.anthropic.com", "/v1/code/sessions/a/events", "Bearer rc-session-secret", "Bearer rc-session-secret", 200},
		{"api.anthropic.com", "/v1/environments/a/work/poll", "Bearer rc-server-secret", "Bearer rc-server-secret", 200},
		{"chatgpt.com", "/backend-api/wham/remote/control/server", "Bearer codex-rc-session", "Bearer codex-rc-session", 200},
		{"chatgpt.com", "/backend-api/codex/responses", "Bearer " + CodexAccess, "Bearer real-access-secret", 200},
		{"chatgpt.com", "/elsewhere", "Bearer " + CodexAccess, "", 403},
		{"api.anthropic.com", "/v1/messages", "Bearer " + CodexAccess, "", 403},
	} {
		t.Run(test.path+test.auth, func(t *testing.T) {
			t.Parallel()
			called := false
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Header.Get("Authorization") != test.want {
					t.Error("wrong auth rewrite")
				}
				if r.URL.Host != test.host || r.URL.Scheme != "https" {
					t.Error("upstream authority not pinned")
				}

				return response("ok"), nil
			})
			r := httptest.NewRequestWithContext(t.Context(), "POST", "https://"+test.host+test.path, nil)
			r.Header.Set("Authorization", test.auth)
			w := httptest.NewRecorder()
			p.Handler(test.host, rt).ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("HTTP %d", w.Code)
			}
			if called != (test.status == 200) {
				t.Fatal("unexpected upstream call")
			}
		})
	}
}

func TestAuthorityMismatchCannotExfiltrateCredential(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	r := httptest.NewRequestWithContext(t.Context(), "GET", "https://evil.example/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+ClaudeAccess)
	w := httptest.NewRecorder()
	p.Handler("api.anthropic.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("upstream reached")

		return nil, nil
	})).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	if p.EnabledFor("other") || p.EnabledFor("") || !p.EnabledFor("owner") {
		t.Fatal("team admission broken")
	}
	for _, h := range []string{"api.anthropic.com.evil.example", "bridge.claudeusercontent.com", "API.ANTHROPIC.COM", "1.1.1.1"} {
		if p.Intercepts(h) {
			t.Fatal("interception too broad")
		}
	}
}

func TestConcurrentRefreshRotatesOnceAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	c := credential(Claude)
	c.ExpiresAt = time.Now().Add(-time.Hour)
	importCredential(t, p, c)
	var calls atomic.Int32
	p.Store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), c.RefreshToken) {
			t.Error("missing actual refresh token")
		}

		return response(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			next, err := p.Store.Current(t.Context(), Claude)
			if err != nil || next.AccessToken != "new-access" {
				t.Error("refresh failed")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls %d", calls.Load())
	}
	store, err := OpenStore(p.Store.root)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Current(t.Context(), Claude)
	if err != nil || next.RefreshToken != "new-refresh" {
		t.Fatal("rotation not durable")
	}
	if _, err = store.Import(Credential{Provider: Claude, AccountID: "other", AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, false); err == nil {
		t.Fatal("account silently replaced")
	}
}

func TestRefreshFailureIsRedactedAndCoalesced(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	c := credential(Codex)
	c.ExpiresAt = time.Now()
	importCredential(t, p, c)
	var calls atomic.Int32
	p.Store.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		r := response("private-refresh-error-token")
		r.StatusCode = 400

		return r, nil
	})
	for range 5 {
		_, err := p.Store.Current(t.Context(), Codex)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("refresh error not redacted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("refresh failure storm")
	}
}

func TestRefreshEndpointMasksResponsesAndRejectsNativeExchange(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	importCredential(t, p, credential(Codex))
	p.Store.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})
	handler := p.Handler("auth.openai.com", nil)
	for _, test := range []struct {
		body   string
		status int
	}{
		{"grant_type=refresh_token&client_id=app_EMoamEEZ73f0CkXaXp7hrann&refresh_token=" + CodexRefresh, 200},
		{"grant_type=authorization_code&code=secret", 403},
		{"grant_type=refresh_token&client_id=wrong&refresh_token=" + CodexRefresh, 403},
		{"grant_type=refresh_token&refresh_token=real-token", 403},
	} {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "https://auth.openai.com/oauth/token", strings.NewReader(test.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatal(w.Code)
		}
		if strings.Contains(w.Body.String(), "real-access-secret") || strings.Contains(w.Body.String(), "real-signature") {
			t.Fatal("secret refresh response")
		}
	}
}

func TestCAStableAcrossRestart(t *testing.T) {
	t.Parallel()
	root := privateDir(t)
	p, err := New(root, "owner")
	if err != nil {
		t.Fatal(err)
	}
	again, err := New(root, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if p.CABundle() != again.CABundle() {
		t.Fatal("resume CA changed")
	}
	if err = os.Chmod(filepath.Join(root, "ca.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = New(root, "owner"); err == nil {
		t.Fatal("public private key accepted")
	}
}

func TestTLSHTTP2AndStreaming(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	importCredential(t, p, credential(Claude))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer real-access-secret" {
			t.Error("TLS request not injected")
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = fmt.Fprint(writer, "data: first\n\n")
			close(sent)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}()
		out := response("")
		out.Body = reader
		out.Header.Set("Content-Type", "text/event-stream")

		return out, nil
	})
	go func() {
		conn, e := ln.Accept()
		if e == nil {
			p.ServeConn(ctx, conn, "api.anthropic.com", rt)
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(p.CABundle()))
	tr := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+ClaudeAccess)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("ALPN negotiated %s", resp.Proto)
	}
	body := make([]byte, len("data: first\n\n"))
	if _, err = io.ReadFull(resp.Body, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "data: first\n\n" {
		t.Fatal("stream corrupted")
	}
	<-sent
}

func TestPersistenceFailureRetainsRotatedLineage(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	c := credential(Claude)
	c.ExpiresAt = time.Now()
	importCredential(t, p, c)
	var calls atomic.Int32
	p.Store.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)

		return response(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})
	root := p.Store.root
	p.Store.root = filepath.Join(root, "missing", "directory")
	if _, err := p.Store.Current(t.Context(), Claude); err == nil {
		t.Fatal("write failure ignored")
	}
	p.Store.root = root
	next, err := p.Store.Current(t.Context(), Claude)
	if err != nil || next.RefreshToken != "new-refresh" {
		t.Fatal("rotated lineage lost")
	}
	if calls.Load() != 1 {
		t.Fatal("rotated refresh token reused")
	}
	again, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	next, err = again.Current(t.Context(), Claude)
	if err != nil || next.RefreshToken != "new-refresh" {
		t.Fatal("pending rotation not persisted")
	}
}

func TestReactiveRefreshCoalescesWhileAccessIsUnexpired(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	importCredential(t, p, credential(Codex))
	var calls atomic.Int32
	p.Store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("Codex refresh must be JSON")
		}

		return response(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`), nil
	})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := p.Store.Refresh(t.Context(), Codex); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("reactive refresh did not coalesce")
	}
}

func TestWebSocketUpgradePreservesRCSessionToken(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rc-session" {
			t.Error("session bearer overwritten")
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)

			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buf.Flush()
		payload := make([]byte, 4)
		if _, err = io.ReadFull(buf, payload); err == nil {
			_, _ = buf.Write(payload)
			_ = buf.Flush()
		}
	}))
	defer upstream.Close()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		u, err := url.Parse(upstream.URL)
		if err != nil {
			return nil, err
		}
		r.URL.Scheme = u.Scheme
		r.URL.Host = u.Host

		return http.DefaultTransport.RoundTrip(r)
	})
	gateway := httptest.NewServer(p.Handler("chatgpt.com", rt))
	defer gateway.Close()
	u, err := url.Parse(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://chatgpt.com/backend-api/wham/remote/control/server", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer rc-session")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if err = req.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatal(resp.StatusCode)
	}
	if _, err = io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 4)
	if _, err = io.ReadFull(reader, raw); err != nil || string(raw) != "ping" {
		t.Fatal("upgrade stream did not pass through")
	}
}

func TestRefreshRetainsRotatedTokenOnInvalidExpiry(t *testing.T) {
	t.Parallel()
	p := testProxy(t)
	c := credential(Codex)
	c.ExpiresAt = time.Now()
	importCredential(t, p, c)
	p.Store.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"access_token":"opaque","refresh_token":"rotated-without-expiry"}`), nil
	})
	if _, err := p.Store.Current(t.Context(), Codex); err == nil {
		t.Fatal("invalid expiry accepted")
	}
	again, err := OpenStore(p.Store.root)
	if err != nil {
		t.Fatal(err)
	}
	if again.entries[Codex].credential.RefreshToken != "rotated-without-expiry" {
		t.Fatal("rotation lost on protocol failure")
	}
}
