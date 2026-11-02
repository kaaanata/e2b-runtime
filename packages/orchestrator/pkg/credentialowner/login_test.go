package credentialowner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// loginFixture plays the native interactive sign-ins in the test binary:
// Claude prints an authorization URL behind terminal escapes and reads the
// pasted code; Codex prints a device URL and user code, then completes.
func loginFixture() (int, bool) {
	switch {
	case os.Args[1] == "auth" && len(os.Args) == 3 && os.Args[2] == "login" && os.Getenv("CLAUDE_CODE_OAUTH_REFRESH_TOKEN") == "":
		fmt.Print("\x1b[?25l\x1b[2K\x1b[1GOpening browser to sign in…\r\n")
		fmt.Print("If the browser didn't open, visit: \x1b[4mhttps://claude.com/cai/oauth/authorize?code=true&client_id=fixture&state=s1\x1b[24m\r\n")
		fmt.Print("Paste code here if prompted > ")
		// The owner presses Enter (CR); a cooked terminal delivers it as NL.
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		code := strings.TrimSpace(line)
		if !strings.HasPrefix(code, "good-") {
			return 41, true
		}
		home := os.Getenv("CLAUDE_CONFIG_DIR")
		cred := fixtureJSON(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "access-login", "refreshToken": "refresh-login", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:profile", "user:inference", "user:sessions:claude_code"}}})
		profile := fixtureJSON(map[string]any{"oauthAccount": map[string]string{"accountUuid": strings.TrimPrefix(code, "good-"), "organizationUuid": "org"}})
		if os.WriteFile(filepath.Join(home, ".credentials.json"), cred, 0o600) != nil || os.WriteFile(filepath.Join(home, ".claude.json"), profile, 0o600) != nil {
			return 42, true
		}

		return 0, true
	case os.Args[1] == "login":
		if len(os.Args) != 5 || os.Args[2] != "--device-auth" || os.Args[4] != `cli_auth_credentials_store="file"` {
			return 43, true
		}
		fmt.Print("Follow these steps to sign in with ChatGPT:\r\n1. Open \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\r\n2. Enter this one-time code \x1b[94mABCD-EFG12\x1b[0m\r\n")
		time.Sleep(150 * time.Millisecond)
		access := fixtureJSON(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()})
		id := fixtureJSON(map[string]any{"sub": "device-subject"})
		auth := fixtureJSON(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token(string(access)), "refresh_token": "refresh-device", "id_token": token(string(id)), "account_id": "device-account"}})
		if os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), auth, 0o600) != nil {
			return 44, true
		}

		return 0, true
	}

	return 0, false
}

func loginManager(t *testing.T, p credproxy.Provider) *Manager {
	t.Helper()
	m := testManager(t, p)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AddAccount("fresh", p); err != nil {
		t.Fatal(err)
	}
	m.accounts["fresh"].config.Binary = binary

	return m
}

func waitLogin(t *testing.T, m *Manager, id string, want ...LoginState) LoginView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := m.LoginStatus(id)
		if !ok {
			t.Fatal("sign-in session vanished")
		}
		if slices.Contains(want, v.State) {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := m.LoginStatus(id)
	t.Fatalf("sign-in stayed %s (%s), want %v", v.State, v.Message, want)

	return v
}

func TestBrowserClaudeLoginRelaysURLAndCode(t *testing.T) {
	t.Parallel()
	m := loginManager(t, credproxy.Claude)
	v, err := m.StartLogin("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.StartLogin("fresh"); err == nil {
		t.Fatal("concurrent sign-in for one account admitted")
	}
	v = waitLogin(t, m, v.ID, LoginAwaitCode)
	if v.URL != "https://claude.com/cai/oauth/authorize?code=true&client_id=fixture&state=s1" {
		t.Fatalf("authorization URL not extracted cleanly: %q", v.URL)
	}
	if s := m.Status(); s[len(s)-1].State != "signing_in" {
		t.Fatal("status does not report the sign-in")
	}
	if _, err = m.SubmitLoginCode(v.ID, "bad\x1bcode"); err == nil {
		t.Fatal("control characters accepted in code")
	}
	if _, err = m.SubmitLoginCode(v.ID, "good-alice"); err != nil {
		t.Fatal(err)
	}
	v = waitLogin(t, m, v.ID, LoginSucceeded, LoginFailed)
	if v.State != LoginSucceeded || v.URL != "" {
		t.Fatalf("sign-in did not succeed cleanly: %+v", v)
	}
	for _, s := range m.Status() {
		if s.Name == "fresh" && s.State != "ready" {
			t.Fatalf("account not ready after sign-in: %s", s.State)
		}
	}
	// A second login with another identity must not replace the enrolled one.
	v, err = m.StartLogin("fresh")
	if err != nil {
		t.Fatal(err)
	}
	v = waitLogin(t, m, v.ID, LoginAwaitCode)
	if _, err = m.SubmitLoginCode(v.ID, "good-mallory"); err != nil {
		t.Fatal(err)
	}
	if v = waitLogin(t, m, v.ID, LoginSucceeded, LoginFailed); v.State != LoginFailed {
		t.Fatal("identity change admitted by browser sign-in")
	}
}

func TestBrowserClaudeLoginRejectedCodeFails(t *testing.T) {
	t.Parallel()
	m := loginManager(t, credproxy.Claude)
	v, _ := m.StartLogin("fresh")
	v = waitLogin(t, m, v.ID, LoginAwaitCode)
	if _, err := m.SubmitLoginCode(v.ID, "wrong"); err != nil {
		t.Fatal(err)
	}
	if v = waitLogin(t, m, v.ID, LoginSucceeded, LoginFailed); v.State != LoginFailed {
		t.Fatal("rejected code reported success")
	}
}

func TestBrowserCodexDeviceLogin(t *testing.T) {
	t.Parallel()
	m := loginManager(t, credproxy.Codex)
	v, err := m.StartLogin("fresh")
	if err != nil {
		t.Fatal(err)
	}
	v = waitLogin(t, m, v.ID, LoginAwaitAuth, LoginSucceeded)
	if v.State == LoginAwaitAuth && (v.URL != "https://auth.openai.com/codex/device" || v.UserCode != "ABCD-EFG12") {
		t.Fatalf("device URL or code not extracted: %+v", v)
	}
	if v = waitLogin(t, m, v.ID, LoginSucceeded, LoginFailed); v.State != LoginSucceeded {
		t.Fatalf("device sign-in failed: %s", v.Message)
	}
}

func TestBrowserLoginCancel(t *testing.T) {
	t.Parallel()
	m := loginManager(t, credproxy.Claude)
	v, _ := m.StartLogin("fresh")
	waitLogin(t, m, v.ID, LoginAwaitCode)
	if _, err := m.CancelLogin(v.ID); err != nil {
		t.Fatal(err)
	}
	if v = waitLogin(t, m, v.ID, LoginCanceled); v.URL != "" {
		t.Fatal("canceled sign-in still exposes URL")
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.accounts["fresh"].signingIn.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.StartLogin("fresh"); err != nil {
		t.Fatal("account stayed locked after cancel", err)
	}
}

func TestLoginViewCarriesNoOutput(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(LoginView{ID: "x", State: LoginFailed, Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "output") {
		t.Fatal("raw output field present")
	}
}
