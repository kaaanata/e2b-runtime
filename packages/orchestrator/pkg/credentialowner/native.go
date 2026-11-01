package credentialowner

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

type nativeState struct {
	credential credproxy.Credential
	refresh    string
}

type nativeRunner interface {
	Run(ctx context.Context, account Account, home string, state nativeState, login bool, in io.Reader, out io.Writer, stderr io.Writer) error
}
type commandRunner struct{}

func nativeCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	return cmd
}

// Each account has its own Linux-native files. Never inherit the operator's
// credential, provider, proxy, HOME or CODEX_HOME environment.
func nativeEnv(home string) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "USERPROFILE=" + home,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude"), "CODEX_HOME=" + filepath.Join(home, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"LANG=C.UTF-8", "TERM=xterm-256color", "DISABLE_AUTOUPDATER=1",
	}
}

func (commandRunner) Run(ctx context.Context, a Account, home string, state nativeState, login bool, in io.Reader, out, stderr io.Writer) error {
	var args []string
	env := nativeEnv(home)
	switch {
	case a.Provider == credproxy.Claude:
		args = []string{"auth", "login"}
		if !login {
			if state.refresh == "" || state.credential.Scope == "" {
				return errors.New("native Claude login required")
			}
			// This is Claude Code's documented native refresh-login flow. No
			// custom OAuth request or credential-file writer exists here.
			env = append(env, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN="+state.refresh, "CLAUDE_CODE_OAUTH_SCOPES="+state.credential.Scope)
		}
	case login:
		args = []string{"login"}
	default:
		return refreshCodex(ctx, a.Binary, home, env)
	}
	cmd := nativeCommand(ctx, a.Binary, args...)
	cmd.Dir = home
	cmd.Env = env
	if login {
		cmd.Stdin = in
		cmd.Stdout = out
		cmd.Stderr = stderr
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}
	if cmd.Run() != nil {
		return errors.New("native CLI authorization failed; run owner login interactively")
	}

	return nil
}

func refreshCodex(ctx context.Context, binary, home string, env []string) error {
	cmd := nativeCommand(ctx, binary, "app-server", "--stdio", "-c", `cli_auth_credentials_store="file"`)
	cmd.Dir = home
	cmd.Env = env
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return errors.New("cannot start native Codex credential manager")
	}
	defer func() {
		in.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()
	stop := context.AfterFunc(ctx, func() { _ = out.Close() })
	defer stop()
	enc := json.NewEncoder(in)
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 1<<20)
	rpc := func(id int, method string, params any) error {
		if err := enc.Encode(struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params any    `json:"params"`
		}{id, method, params}); err != nil {
			return errors.New("native Codex request failed")
		}
		for scan.Scan() {
			var reply struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(scan.Bytes(), &reply) != nil {
				return errors.New("invalid native Codex protocol response")
			}
			if reply.ID != id {
				continue
			}
			if len(reply.Error) != 0 {
				return errors.New("native Codex credential refresh failed")
			}
			if id == 2 {
				var status struct {
					AuthMethod string `json:"authMethod"`
				}
				if json.Unmarshal(reply.Result, &status) != nil || status.AuthMethod != "chatgpt" {
					return errors.New("native Codex ChatGPT login required")
				}
			}

			return nil
		}

		return errors.New("native Codex credential protocol ended")
	}
	if err = rpc(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "sbx_credential_owner", "version": "1"}}); err != nil {
		return err
	}
	if err := enc.Encode(map[string]string{"method": "initialized"}); err != nil {
		return errors.New("native Codex initialization failed")
	}

	return rpc(2, "getAuthStatus", map[string]bool{"includeToken": false, "refreshToken": true})
}

func accountLock(home string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(home, ".owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()

		return nil, errors.New("native account is in use by another owner operation")
	}

	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func readNative(a Account, home string) (nativeState, error) {
	var state nativeState
	c := &state.credential
	c.Provider = a.Provider
	if a.Provider == credproxy.Claude {
		b, err := privateRead(filepath.Join(home, "claude", ".credentials.json"))
		if err != nil {
			return state, errors.New("native Claude login unavailable")
		}
		var doc struct {
			OAuth struct {
				Access  string   `json:"accessToken"`
				Refresh string   `json:"refreshToken"`
				Expires int64    `json:"expiresAt"`
				Scopes  []string `json:"scopes"`
				Plan    string   `json:"subscriptionType"`
				Tier    string   `json:"rateLimitTier"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &doc) != nil {
			return state, errors.New("invalid native Claude credential format")
		}
		b, err = privateRead(filepath.Join(home, "claude", ".claude.json"))
		if err != nil {
			return state, errors.New("native Claude profile unavailable")
		}
		var profile struct {
			Onboarded bool `json:"hasCompletedOnboarding,omitzero"`
			Account   struct {
				UUID  string `json:"accountUuid"`
				Org   string `json:"organizationUuid"`
				Email string `json:"emailAddress"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &profile) != nil || profile.Account.UUID == "" || profile.Account.Org == "" {
			return state, errors.New("native Claude account identity unavailable")
		}
		c.AccountID = profile.Account.UUID + ":" + profile.Account.Org
		c.AccessToken = doc.OAuth.Access
		state.refresh = doc.OAuth.Refresh
		c.ExpiresAt = time.UnixMilli(doc.OAuth.Expires)
		c.Scope = strings.Join(doc.OAuth.Scopes, " ")
		c.SubscriptionType = doc.OAuth.Plan
		c.RateLimitTier = doc.OAuth.Tier
		// Only these public profile fields enter the VM, never CLI state or
		// cached feature flags from the native credential owner.
		c.State, err = json.Marshal(profile)
		if err != nil {
			return state, errors.New("cannot encode public native profile")
		}
	} else {
		b, err := privateRead(filepath.Join(home, "codex", "auth.json"))
		if err != nil {
			return state, errors.New("native Codex login unavailable")
		}
		var doc struct {
			Mode   string `json:"auth_mode"`
			Tokens struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
				ID      string `json:"id_token"`
				Account string `json:"account_id"`
			} `json:"tokens"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.Mode != "chatgpt" {
			return state, errors.New("native Codex ChatGPT credential format required")
		}
		c.AccessToken = doc.Tokens.Access
		c.IDToken = doc.Tokens.ID
		c.AccountID = doc.Tokens.Account
		state.refresh = doc.Tokens.Refresh
		parts := strings.Split(c.AccessToken, ".")
		if len(parts) != 3 {
			return state, errors.New("native Codex access expiry unavailable")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
			return state, errors.New("native Codex access expiry unavailable")
		}
		c.ExpiresAt = time.Unix(claims.Exp, 0)
	}
	if state.refresh == "" || c.Validate() != nil {
		return state, errors.New("native credentials incomplete")
	}

	return state, nil
}
