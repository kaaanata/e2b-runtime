package credentialowner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// Browser-driven native sign-in. The owner still runs only the native CLI in
// the account's own home; it relays the CLI's authorization URL (and device
// code) to the administrator and, for Claude, types the pasted code into the
// CLI's terminal. Raw CLI output never leaves the owner.

type LoginState string

const (
	LoginStarting   LoginState = "starting"
	LoginAwaitCode  LoginState = "awaiting_code"   // Claude: open URL, paste the code back
	LoginAwaitAuth  LoginState = "awaiting_device" // Codex: open URL, enter the user code there
	LoginVerifying  LoginState = "verifying"
	LoginSucceeded  LoginState = "succeeded"
	LoginFailed     LoginState = "failed"
	LoginCanceled   LoginState = "canceled"
	loginTimeout               = 15 * time.Minute
	loginKeep                  = 10 * time.Minute
	maxActiveLogins            = 4
	maxLoginOutput             = 256 << 10
)

type LoginView struct {
	ID       string             `json:"id"`
	Account  string             `json:"account"`
	Provider credproxy.Provider `json:"provider"`
	State    LoginState         `json:"state"`
	URL      string             `json:"url,omitempty"`
	UserCode string             `json:"user_code,omitempty"`
	Message  string             `json:"message,omitempty"`
	Started  time.Time          `json:"started_at"`
}

type loginSession struct {
	mu       sync.Mutex
	view     LoginView
	terminal *os.File
	cancel   context.CancelFunc
	finished time.Time
}

type loginSessions struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func (s *loginSession) snapshot() LoginView {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.view
}

func (s *loginSession) set(f func(v *LoginView)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.view)
}

func (l *loginSessions) prune() {
	for id, s := range l.sessions {
		s.mu.Lock()
		done := !s.finished.IsZero() && time.Since(s.finished) > loginKeep
		s.mu.Unlock()
		if done {
			delete(l.sessions, id)
		}
	}
}

var (
	ansiPattern       = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	claudeURLPattern  = regexp.MustCompile(`https://[^\s"'<>]*oauth/authorize\?[^\s"'<>]+`)
	codexURLPattern   = regexp.MustCompile(`https://auth\.openai\.com/[^\s"'<>]+`)
	deviceCodePattern = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4,6}\b`)
)

// loginCommand is the native interactive sign-in for one account home.
func loginCommand(ctx context.Context, a Account, home string) *exec.Cmd {
	var args []string
	if a.Provider == credproxy.Claude {
		args = []string{"auth", "login"}
	} else {
		args = []string{"login", "--device-auth", "-c", `cli_auth_credentials_store="file"`}
	}
	cmd := exec.CommandContext(ctx, a.Binary, args...)
	cmd.Dir = home
	cmd.Env = append(nativeEnv(home), "BROWSER=/bin/false")
	// pty.Start makes the CLI a session leader, so its pid is the process
	// group to kill on cancel.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	return cmd
}

// StartLogin begins a browser-driven native sign-in for an account.
func (m *Manager) StartLogin(name string) (LoginView, error) {
	a, ok := m.account(name)
	if !ok {
		return LoginView{}, errors.New("unknown account")
	}
	m.logins.mu.Lock()
	defer m.logins.mu.Unlock()
	if m.logins.sessions == nil {
		m.logins.sessions = map[string]*loginSession{}
	}
	m.logins.prune()
	active := 0
	for _, s := range m.logins.sessions {
		s.mu.Lock()
		running := s.finished.IsZero()
		same := s.view.Account == name
		s.mu.Unlock()
		if running {
			active++
			if same {
				return LoginView{}, errors.New("a sign-in for this account is already in progress")
			}
		}
	}
	if active >= maxActiveLogins {
		return LoginView{}, errors.New("too many sign-ins in progress")
	}
	if !a.signingIn.CompareAndSwap(false, true) {
		return LoginView{}, errors.New("a sign-in for this account is already in progress")
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	s := &loginSession{cancel: cancel, view: LoginView{ID: hex.EncodeToString(id), Account: name, Provider: a.config.Provider, State: LoginStarting, Started: time.Now().UTC()}}
	m.logins.sessions[s.view.ID] = s
	go m.runLogin(ctx, a, s)

	return s.snapshot(), nil
}

func (m *Manager) runLogin(ctx context.Context, a *accountState, s *loginSession) {
	defer s.cancel()
	defer a.signingIn.Store(false)
	finish := func(state LoginState, message string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.view.State != LoginCanceled || state == LoginCanceled {
			s.view.State = state
			s.view.Message = message
		}
		s.view.URL, s.view.UserCode = "", ""
		s.finished = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := accountLock(a.home)
	if err != nil {
		finish(LoginFailed, "The account is busy with another owner operation.")

		return
	}
	defer unlock()
	cmd := loginCommand(ctx, a.config, a.home)
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 60, Cols: 4000})
	if err != nil {
		finish(LoginFailed, "The native CLI could not be started.")

		return
	}
	s.mu.Lock()
	s.terminal = terminal
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.watchLogin(a.config.Provider, terminal, s)
	}()
	waitErr := cmd.Wait()
	_ = terminal.Close()
	<-done
	if s.snapshot().State == LoginCanceled {
		finish(LoginCanceled, "Sign-in canceled.")

		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		finish(LoginFailed, "Sign-in timed out. Start it again.")

		return
	}
	if waitErr != nil {
		finish(LoginFailed, "The native CLI did not complete the sign-in.")

		return
	}
	state, err := readNative(a.config, a.home)
	if err != nil || !state.credential.ExpiresAt.After(time.Now()) {
		finish(LoginFailed, "The native CLI exited without a usable login.")

		return
	}
	if err = bindIdentity(a, state.credential, true); err != nil {
		finish(LoginFailed, "This login belongs to a different identity than the account was first signed in with. Add a new account instead.")

		return
	}
	a.lastError, a.rejectedGeneration, a.retryAfter = "", "", time.Time{}
	finish(LoginSucceeded, "Signed in.")
}

// watchLogin scans the CLI terminal for the authorization URL and code.
func (m *Manager) watchLogin(p credproxy.Provider, terminal io.Reader, s *loginSession) {
	buf := make([]byte, 4096)
	var seen strings.Builder
	for {
		n, err := terminal.Read(buf)
		if n > 0 && seen.Len() < maxLoginOutput {
			seen.Write(buf[:n])
			text := ansiPattern.ReplaceAllString(seen.String(), "")
			s.set(func(v *LoginView) {
				if v.State != LoginStarting {
					return
				}
				if p == credproxy.Claude {
					if u := claudeURLPattern.FindString(text); u != "" {
						v.URL, v.State = strings.TrimRight(u, ".,)"), LoginAwaitCode
					}

					return
				}
				u := codexURLPattern.FindString(text)
				code := deviceCodePattern.FindString(text)
				if u != "" && code != "" {
					v.URL, v.UserCode, v.State = strings.TrimRight(u, ".,)"), code, LoginAwaitAuth
				}
			})
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) loginSession(id string) (*loginSession, bool) {
	m.logins.mu.Lock()
	defer m.logins.mu.Unlock()
	s, ok := m.logins.sessions[id]

	return s, ok
}

func (m *Manager) LoginStatus(id string) (LoginView, bool) {
	s, ok := m.loginSession(id)
	if !ok {
		return LoginView{}, false
	}

	return s.snapshot(), true
}

// SubmitLoginCode types the authorization code into a waiting Claude sign-in.
func (m *Manager) SubmitLoginCode(id, code string) (LoginView, error) {
	s, ok := m.loginSession(id)
	if !ok {
		return LoginView{}, errors.New("unknown sign-in")
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 4096 || strings.ContainsFunc(code, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return LoginView{}, errors.New("invalid authorization code")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view.State != LoginAwaitCode || s.terminal == nil {
		return s.view, errors.New("this sign-in is not waiting for a code")
	}
	if _, err := io.WriteString(s.terminal, code+"\r"); err != nil {
		return s.view, errors.New("the native CLI is no longer accepting input")
	}
	s.view.State = LoginVerifying

	return s.view, nil
}

func (m *Manager) CancelLogin(id string) (LoginView, error) {
	s, ok := m.loginSession(id)
	if !ok {
		return LoginView{}, errors.New("unknown sign-in")
	}
	s.mu.Lock()
	running := s.finished.IsZero()
	if running {
		s.view.State = LoginCanceled
		s.view.URL, s.view.UserCode = "", ""
	}
	s.mu.Unlock()
	if running {
		s.cancel()
	}

	return s.snapshot(), nil
}

func (m *Manager) cancelLogins() {
	m.logins.mu.Lock()
	defer m.logins.mu.Unlock()
	for _, s := range m.logins.sessions {
		s.cancel()
	}
}
