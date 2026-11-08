package credentialowner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

func token(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func marshalFixture(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func writeFixture(t *testing.T, a *accountState, account string, expiry time.Time) {
	t.Helper()
	if a.config.Provider == credproxy.Claude {
		b := marshalFixture(t, map[string]any{"claudeAiOauth": map[string]any{"accessToken": "access-" + expiry.String(), "refreshToken": "refresh-private", "expiresAt": expiry.UnixMilli(), "scopes": []string{"user:profile", "user:inference", "user:sessions:claude_code"}, "subscriptionType": "max"}})
		if err := os.WriteFile(filepath.Join(a.home, "claude", ".credentials.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		b = marshalFixture(t, map[string]any{"oauthAccount": map[string]string{"accountUuid": account, "organizationUuid": "org", "emailAddress": "synthetic@example.invalid"}, "cachedFeatureFlags": "never-copy"})
		if err := os.WriteFile(filepath.Join(a.home, "claude", ".claude.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		access := marshalFixture(t, map[string]int64{"exp": expiry.Unix()})
		id := marshalFixture(t, map[string]any{"sub": "subject-" + account, "email": "synthetic@example.invalid", "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account, "chatgpt_plan_type": "pro"}})
		b := marshalFixture(t, map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token(string(access)), "refresh_token": "refresh-private", "id_token": token(string(id)), "account_id": account}})
		if err := os.WriteFile(filepath.Join(a.home, "codex", "auth.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type fixtureRunner struct {
	run func(Account, string, nativeState, bool) error
}

func (f fixtureRunner) Run(_ context.Context, a Account, h string, s nativeState, login bool, _ io.Reader, _, _ io.Writer) error {
	return f.run(a, h, s, login)
}

func testManager(t *testing.T, p credproxy.Provider) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	c := Config{Version: 1, Root: root, MaxSessions: 10, Routes: map[credproxy.Provider]Route{p: {Control: "control", Inference: []string{"worker1", "worker2"}}}}
	for _, name := range []string{"control", "worker1", "worker2"} {
		c.Accounts = append(c.Accounts, Account{Name: name, Provider: p, Binary: "/usr/bin/native-cli"})
	}
	m, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range m.accounts {
		writeFixture(t, a, name, time.Now().Add(time.Hour))
		s, err := readNative(a.config, a.home)
		if err != nil {
			t.Fatal(err)
		}
		if err = bindIdentity(a, s.credential, true); err != nil {
			t.Fatal(err)
		}
	}
	m.runner = fixtureRunner{run: func(Account, string, nativeState, bool) error {
		t.Error("unexpected native refresh")

		return errors.New("unexpected")
	}}

	return m
}

func session(id string) string {
	s := sha256.Sum256([]byte(id))

	return hex.EncodeToString(s[:])
}

func TestControlIdentityAndPersistentInferenceBindings(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	a, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: credproxy.Codex, Purpose: credproxy.Control})
	if err != nil || a.Account != "control" {
		t.Fatal("control identity lost", err)
	}
	r := credproxy.ResolveRequest{Provider: credproxy.Codex, Purpose: credproxy.Inference, Session: session("one")}
	b, err := m.Resolve(t.Context(), r)
	if err != nil || b.Account != "worker1" {
		t.Fatal("first pool account", err)
	}
	c, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: credproxy.Codex, Purpose: credproxy.Inference, Session: session("two")})
	if err != nil || c.Account != "worker2" {
		t.Fatal("second pool account", err)
	}
	if err = m.Observe(t.Context(), credproxy.Outcome{Provider: credproxy.Codex, Purpose: credproxy.Inference, Session: r.Session, Account: b.Account, Status: 429, RetryAfter: 3600}); err != nil {
		t.Fatal(err)
	}
	still, err := m.Resolve(t.Context(), r)
	if err != nil || still.Account != b.Account {
		t.Fatal("quota result reassigned existing session")
	}
	reopened, err := Open(m.config)
	if err != nil {
		t.Fatal(err)
	}
	still, err = reopened.Resolve(t.Context(), r)
	if err != nil || still.Account != b.Account {
		t.Fatal("restart lost session binding")
	}
	changed := m.config
	changed.Routes = map[credproxy.Provider]Route{credproxy.Codex: {Control: "worker2", Inference: []string{"worker1"}}}
	if _, err = Open(changed); err == nil {
		t.Fatal("control account hot swap admitted")
	}
}

func TestNativeRefreshCoalescesAndRereadsFiles(t *testing.T) {
	t.Parallel()
	for _, p := range []credproxy.Provider{credproxy.Claude, credproxy.Codex} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			m := testManager(t, p)
			a := m.accounts["control"]
			writeFixture(t, a, "control", time.Now().Add(-time.Minute))
			var calls atomic.Int32
			m.runner = fixtureRunner{run: func(_ Account, _ string, s nativeState, login bool) error {
				if login || s.refresh != "refresh-private" {
					t.Error("wrong native operation")
				}
				calls.Add(1)
				writeFixture(t, a, "control", time.Now().Add(time.Hour))

				return nil
			}}
			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() {
					l, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: p, Purpose: credproxy.Control, Force: true})
					if err != nil || !l.Credential.ExpiresAt.After(time.Now()) {
						t.Error("renewed lease unavailable", err)
					}
				})
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("native renewal count %d", calls.Load())
			}
			status := m.Status()
			b, _ := json.Marshal(status)
			if strings.Contains(string(b), "refresh-private") || strings.Contains(string(b), "access-") {
				t.Fatal("status exposed credentials")
			}
		})
	}
}

func TestNativeIdentityChangeAndNoopRenewalFailClosed(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"identity-change", "no-write", "native-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			m := testManager(t, credproxy.Claude)
			a := m.accounts["control"]
			writeFixture(t, a, "control", time.Now().Add(2*time.Minute))
			m.runner = fixtureRunner{run: func(Account, string, nativeState, bool) error {
				if mode == "identity-change" {
					writeFixture(t, a, "other", time.Now().Add(time.Hour))
				}
				if mode == "native-error" {
					return errors.New("native login required")
				}

				return nil
			}}
			if _, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Control, Force: true}); err == nil {
				t.Fatal("invalid native renewal accepted")
			}
		})
	}
}

func TestNativeRotationPersistsEvenWhenProcessReportsFailure(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	a := m.accounts["control"]
	writeFixture(t, a, "control", time.Now().Add(-time.Minute))
	m.runner = fixtureRunner{run: func(Account, string, nativeState, bool) error {
		writeFixture(t, a, "control", time.Now().Add(time.Hour))

		return errors.New("native process reported failure after write")
	}}
	if _, err := m.current(t.Context(), "control", true); err == nil {
		t.Fatal("process failure ignored")
	}
	// Native files, not a second refresh-token copy, remain the durable truth.
	reopened, err := Open(m.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.current(t.Context(), "control", false); err != nil {
		t.Fatal("native persisted rotation lost", err)
	}
}

func TestPoolRemovalDoesNotReassignBoundConversation(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	r := credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Inference, Session: session("thread")}
	l, err := m.Resolve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	m.config.Routes[credproxy.Claude] = Route{Control: "control", Inference: []string{"worker2"}}
	if l.Account != "worker1" {
		t.Fatal("fixture selected wrong account")
	}
	if _, err = m.Resolve(t.Context(), r); err == nil {
		t.Fatal("removed bound account silently replaced")
	}
}

func TestUnknownSessionsAndOutcomesCannotSelectAccounts(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	for _, r := range []credproxy.ResolveRequest{{Provider: credproxy.Codex, Purpose: credproxy.Inference}, {Provider: credproxy.Codex, Purpose: "bad", Session: session("t")}, {Provider: "bad", Purpose: credproxy.Control}} {
		if _, err := m.Resolve(t.Context(), r); err == nil {
			t.Fatal("invalid routing accepted")
		}
	}
	if m.Observe(t.Context(), credproxy.Outcome{Provider: credproxy.Codex, Purpose: credproxy.Inference, Session: session("absent"), Account: "control", Status: 401}) == nil {
		t.Fatal("unbound outcome accepted")
	}
}

func TestOwnerHTTPExportsNoRefreshToken(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/resolve", strings.NewReader(`{"provider":"claude","purpose":"control"}`))
	m.Handler().ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "refresh-private") || strings.Contains(w.Body.String(), "refresh_token") || strings.Contains(w.Body.String(), "cachedFeatureFlags") {
		t.Fatal("private native state escaped owner lease")
	}
}

func TestNativeEnvironmentDoesNotInheritGatewayOrDesktop(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "desktop-secret")
	t.Setenv("HTTPS_PROXY", "http://wrong")
	env := strings.Join(nativeEnv("/private/account"), "\n")
	if strings.Contains(env, "desktop-secret") || strings.Contains(env, "HTTPS_PROXY=") || strings.Contains(env, "ANTHROPIC_BASE_URL=") {
		t.Fatal("native manager inherited sandbox routing or credentials")
	}
	if !strings.Contains(env, "CODEX_HOME=/private/account/codex") {
		t.Fatal("native home missing")
	}
}

func TestRejectedGenerationCannotBeReusedAfterFailedNativeRecovery(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	r := credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Inference, Session: session("thread")}
	lease, err := m.Resolve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	m.runner = fixtureRunner{run: func(Account, string, nativeState, bool) error { return errors.New("native login revoked") }}
	if err = m.Observe(t.Context(), credproxy.Outcome{Provider: r.Provider, Purpose: r.Purpose, Session: r.Session, Account: lease.Account, Status: 401, Generation: lease.Credential.Generation()}); err == nil {
		t.Fatal("native recovery failure ignored")
	}
	if _, err = m.Resolve(t.Context(), r); err == nil {
		t.Fatal("rejected but unexpired token reused")
	}
	if _, err = m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Control}); err != nil {
		t.Fatal("worker rejection changed control identity", err)
	}
}

func TestLate401CannotInvalidateNewNativeGeneration(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	r := credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Inference, Session: session("late")}
	lease, err := m.Resolve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, m.accounts[lease.Account], lease.Account, time.Now().Add(2*time.Hour))
	if err = m.Observe(t.Context(), credproxy.Outcome{Provider: r.Provider, Purpose: r.Purpose, Session: r.Session, Account: lease.Account, Status: 401, Generation: lease.Credential.Generation()}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Resolve(t.Context(), r); err != nil {
		t.Fatal("late failure invalidated fresh generation", err)
	}
}

func TestConcurrentNewSessionsUsePoolWithoutLosingBindings(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	m.config.MaxSessions = 24
	var wg sync.WaitGroup
	var one, two atomic.Int32
	for i := range 24 {
		wg.Go(func() {
			s := session(string(rune('a' + i)))
			lease, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: credproxy.Claude, Purpose: credproxy.Inference, Session: s})
			if err != nil {
				t.Error(err)

				return
			}
			switch lease.Account {
			case "worker1":
				one.Add(1)
			case "worker2":
				two.Add(1)
			default:
				t.Error("unknown pool account")
			}
		})
	}
	wg.Wait()
	if one.Load() != 12 || two.Load() != 12 || len(m.bindings.Sessions) != 24 {
		t.Fatalf("concurrent pool distribution %d/%d bindings=%d", one.Load(), two.Load(), len(m.bindings.Sessions))
	}
}
