package credproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
	p := t.TempDir()
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatal(err)
	}

	return p
}

func identity(account string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user","email":"synthetic@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"`+account+`"},"secret_claim":"not-public"}`)) + ".signed"
}

func credential(p Provider, account string) Credential {
	return Credential{Provider: p, AccountID: account, AccessToken: "synthetic-access-" + account, IDToken: identity(account), ExpiresAt: time.Now().Add(time.Hour), Scope: "user:profile user:inference user:sessions:claude_code"}
}

type fakeOwner struct {
	mu       sync.Mutex
	requests []ResolveRequest
	outcomes []Outcome
	fail     bool
}

func (o *fakeOwner) Resolve(_ context.Context, r ResolveRequest) (Lease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, r)
	if o.fail {
		return Lease{}, errors.New("owner unavailable")
	}
	name := "control"
	if r.Purpose == Inference {
		name = "inference"
	}
	account := name
	if r.Provider == Claude {
		account += ":org"
	}

	return Lease{Account: name, Credential: credential(r.Provider, account)}, nil
}

func (o *fakeOwner) Observe(_ context.Context, r Outcome) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, r)

	return nil
}

func testProxy(t *testing.T) (*Proxy, *fakeOwner) {
	t.Helper()
	p, err := New(privateDir(t), "owner", "/unused-owner.sock")
	if err != nil {
		t.Fatal(err)
	}
	o := &fakeOwner{}
	p.Store.owner = o

	return p, o
}

func TestGuestDocumentsHaveNoRealCredentials(t *testing.T) {
	t.Parallel()
	for _, p := range []Provider{Claude, Codex} {
		c := credential(p, "account")
		f, err := c.GuestFiles()
		if err != nil {
			t.Fatal(err)
		}
		b := string(f.Credentials)
		for _, secret := range []string{c.AccessToken, c.IDToken, "not-public", ".signed"} {
			if strings.Contains(b, secret) {
				t.Fatal("credential leaked into guest document")
			}
		}
		if !strings.Contains(b, p.AccessSentinel()) || !strings.Contains(b, p.RefreshSentinel()) {
			t.Fatal("missing sentinel")
		}
	}
}

func TestControlAndInferenceAreSeparate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		host, path, method, body string
		purpose                  Purpose
	}{
		{"api.anthropic.com", "/api/oauth/profile", "GET", "", Control},
		{"api.anthropic.com", "/v1/code/sessions/new", "POST", "{}", Control},
		{"api.anthropic.com", "/v1/messages", "POST", `{"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"control\",\"session_id\":\"thread\"}"},"messages":[]}`, Inference},
		{"chatgpt.com", "/backend-api/wham/remote/control/server/enroll", "POST", "{}", Control},
		{"chatgpt.com", "/backend-api/codex/models", "GET", "", Control},
		{"chatgpt.com", "/backend-api/codex/responses", "POST", "{}", Inference},
		{"chatgpt.com", "/backend-api/codex/responses/compact", "POST", "{}", Inference},
	} {
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			p, o := testProxy(t)
			req := httptest.NewRequestWithContext(t.Context(), test.method, "https://"+test.host+test.path, strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer "+provider(test.host).AccessSentinel())
			req.Header.Set("session_id", "thread")
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				name := "control"
				if test.purpose == Inference {
					name = "inference"
				}
				account := name
				if provider(test.host) == Claude {
					account += ":org"
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-access-"+account {
					t.Fatal("wrong account bearer")
				}
				if provider(test.host) == Codex && r.Header.Get("ChatGPT-Account-ID") != account {
					t.Fatal("wrong Codex account ID")
				}
				if test.host == "api.anthropic.com" && test.purpose == Inference {
					b, _ := io.ReadAll(r.Body)
					if strings.Contains(string(b), `\"account_uuid\":\"control\"`) || !strings.Contains(string(b), `\"account_uuid\":\"inference\"`) {
						t.Fatal("Claude identity not aligned")
					}
				}

				return response("ok"), nil
			})
			w := httptest.NewRecorder()
			p.Handler(test.host, rt, "sandbox").ServeHTTP(w, req)
			if w.Code != 200 || len(o.requests) != 1 || o.requests[0].Purpose != test.purpose {
				t.Fatalf("routing status=%d requests=%v", w.Code, o.requests)
			}
		})
	}
}

func TestScopedSessionTokensNeverReachOwner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ host, path string }{{"api.anthropic.com", "/v1/code/sessions/a/events"}, {"api.anthropic.com", "/v1/environments/a/work/poll"}, {"chatgpt.com", "/backend-api/wham/remote/control/server"}} {
		p, o := testProxy(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+test.host+test.path, nil)
		req.Header.Set("Authorization", "Bearer rc-session")
		w := httptest.NewRecorder()
		p.Handler(test.host, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer rc-session" {
				t.Fatal("RC identity rewritten")
			}

			return response("ok"), nil
		}), "sandbox").ServeHTTP(w, req)
		if w.Code != 200 || len(o.requests) != 0 {
			t.Fatal("RC session asked credential owner")
		}
	}
}

func TestAuthorityAndMixedIdentityRefused(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.Host = "evil.invalid" }, func(r *http.Request) { r.Header.Set("X-Api-Key", "another-account") }, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+CodexAccess) }} {
		p, _ := testProxy(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+ClaudeAccess)
		mutate(req)
		w := httptest.NewRecorder()
		p.Handler("api.anthropic.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsafe request reached upstream")

			return nil, nil
		}), "sandbox").ServeHTTP(w, req)
		if w.Code < 400 {
			t.Fatal("unsafe request accepted")
		}
	}
}

func TestInference401DoesNotRevokeControlOrReplay(t *testing.T) {
	t.Parallel()
	p, o := testProxy(t)
	calls := 0
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+CodexAccess)
	r.Header.Set("session_id", "thread")
	w := httptest.NewRecorder()
	p.Handler("chatgpt.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		res := response(`{"error":"token_revoked"}`)
		res.StatusCode = 401

		return res, nil
	}), "sandbox").ServeHTTP(w, r)
	if w.Code != 503 || calls != 1 || len(o.outcomes) != 1 || o.outcomes[0].Account != "inference" || strings.Contains(w.Body.String(), "token_revoked") {
		t.Fatal("pool failure leaked as control revocation or replayed")
	}
}

func TestMaskedRefreshDelegatesToNativeOwner(t *testing.T) {
	t.Parallel()
	p, o := testProxy(t)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"`+CodexRefresh+`","client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.Handler("auth.openai.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("proxy tried to renew OAuth itself")

		return nil, nil
	}), "sandbox").ServeHTTP(w, r)
	if w.Code != 200 || len(o.requests) != 1 || !o.requests[0].Force || o.requests[0].Purpose != Control || strings.Contains(w.Body.String(), "synthetic-access") {
		t.Fatal("masked native refresh contract failed")
	}
}

func TestGzipClaudeIdentityAndStableRouting(t *testing.T) {
	t.Parallel()
	p, o := testProxy(t)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(gz, `{"metadata":{"user_id":"user_device_account_control_session_thread"}}`)
	_ = gz.Close()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(compressed.Bytes()))
	r.Header.Set("Authorization", "Bearer "+ClaudeAccess)
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	p.Handler("api.anthropic.com", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		if req.Header.Get("Content-Encoding") != "" || !strings.Contains(string(b), "account_inference_session_thread") {
			t.Fatal("gzip identity rewrite failed")
		}

		return response("ok"), nil
	}), "sandbox").ServeHTTP(w, r)
	if w.Code != 200 || len(o.requests[0].Session) != 64 {
		t.Fatal("no stable session binding")
	}
}

func TestUnavailableOwnerFailsClosed(t *testing.T) {
	t.Parallel()
	p, o := testProxy(t)
	o.fail = true
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil)
	r.Header.Set("Authorization", "Bearer "+ClaudeAccess)
	w := httptest.NewRecorder()
	p.Handler("api.anthropic.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing owner forwarded sentinel")

		return nil, nil
	}), "sandbox").ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestNodeCAStableAcrossRestart(t *testing.T) {
	t.Parallel()
	root := privateDir(t)
	a, err := New(root, "owner", "/owner.sock")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(root, "owner", "/owner.sock")
	if err != nil || a.CABundle() != b.CABundle() {
		t.Fatal("node CA changed")
	}
}

func TestLeaseSerializationNeverIncludesRefresh(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Lease{Account: "a", Credential: credential(Codex, "account")})
	if err != nil || strings.Contains(string(b), "refresh_token") {
		t.Fatal("lease must have no refresh token field")
	}
}

func TestNullAndUnknownClaudeMetadataDoNotPanicOrForward(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`null`, `{"metadata":{"user_id":"null"}}`, `{"metadata":{"user_id":"unknown-identity"}}`} {
		p, _ := testProxy(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+ClaudeAccess)
		w := httptest.NewRecorder()
		p.Handler("api.anthropic.com", roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsupported identity forwarded")

			return nil, nil
		}), "sandbox").ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
