package credentialowner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

type accountState struct {
	mu                 sync.Mutex
	config             Account
	home               string
	lastRefresh        time.Time
	retryAfter         time.Time
	lastError          string
	rejectedGeneration string
}

type bindings struct {
	Sessions map[string]string             `json:"sessions"`
	Next     map[credproxy.Provider]uint64 `json:"next"`
	Controls map[credproxy.Provider]string `json:"controls"`
}

type Manager struct {
	config   Config
	accounts map[string]*accountState
	runner   nativeRunner
	mu       sync.Mutex
	bindings bindings
	cooldown map[string]time.Time
}

func Open(config Config) (*Manager, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if err := privateDir(config.Root); err != nil {
		return nil, err
	}
	unlock, err := accountLock(config.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := privateDir(filepath.Join(config.Root, "native")); err != nil {
		return nil, err
	}
	m := &Manager{
		config: config, accounts: map[string]*accountState{}, runner: commandRunner{},
		bindings: bindings{Sessions: map[string]string{}, Next: map[credproxy.Provider]uint64{}, Controls: map[credproxy.Provider]string{}}, cooldown: map[string]time.Time{},
	}
	if config.MaxSessions == 0 {
		m.config.MaxSessions = 10000
	}
	for _, a := range config.Accounts {
		home := filepath.Join(config.Root, "native", a.Name)
		if err := privateDir(home); err != nil {
			return nil, err
		}
		for _, dir := range []string{"claude", "codex", "config", "cache"} {
			if err := privateDir(filepath.Join(home, dir)); err != nil {
				return nil, err
			}
		}
		m.accounts[a.Name] = &accountState{config: a, home: home}
	}
	path := filepath.Join(config.Root, "sessions.json")
	if _, err := os.Lstat(path); err == nil {
		b, err := privateReadLimit(path, 16<<20)
		if err != nil || json.Unmarshal(b, &m.bindings) != nil || m.bindings.Sessions == nil || m.bindings.Next == nil || m.bindings.Controls == nil {
			return nil, errors.New("cannot load native session bindings")
		}
		for p, name := range m.bindings.Controls {
			if config.Routes[p].Control != name {
				return nil, errors.New("control account cannot change in an existing owner root; cold migration required")
			}
		}
		for p, r := range config.Routes {
			if m.bindings.Controls[p] != r.Control {
				return nil, errors.New("control route set changed; cold migration required")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		for p, r := range config.Routes {
			m.bindings.Controls[p] = r.Control
		}
		b, err := json.Marshal(m.bindings)
		if err != nil {
			return nil, err
		}
		if err = savePrivate(path, b); err != nil {
			return nil, err
		}
	}

	return m, nil
}

func identity(c credproxy.Credential) (string, error) {
	id := string(c.Provider) + "\x00" + c.AccountID
	if c.Provider == credproxy.Codex {
		parts := strings.Split(c.IDToken, ".")
		if len(parts) != 3 {
			return "", errors.New("native identity is malformed")
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Sub string `json:"sub"`
		}
		if err != nil || json.Unmarshal(b, &claims) != nil || claims.Sub == "" {
			return "", errors.New("native identity lacks subject")
		}
		id += "\x00" + claims.Sub
	}
	s := sha256.Sum256([]byte(id))

	return hex.EncodeToString(s[:]), nil
}

func bindIdentity(a *accountState, c credproxy.Credential, enroll bool) error {
	want, err := identity(c)
	if err != nil {
		return err
	}
	path := filepath.Join(a.home, "identity.json")
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) && enroll {
		return savePrivate(path, []byte(`{"binding":"`+want+`"}`))
	}
	b, err := privateRead(path)
	var old struct {
		Binding string `json:"binding"`
	}
	if err != nil || json.Unmarshal(b, &old) != nil || old.Binding != want {
		return errors.New("native account identity differs or is not enrolled; use owner login with a new account alias")
	}

	return nil
}

func savePrivate(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()

	return dir.Sync()
}

// Login runs only the selected native CLI in its dedicated account home. It
// does not copy desktop caches or issue an OAuth HTTP request itself.
func (m *Manager) Login(ctx context.Context, name string, in io.Reader, out, stderr io.Writer) error {
	a, ok := m.accounts[name]
	if !ok {
		return errors.New("unknown account alias")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := accountLock(a.home)
	if err != nil {
		return err
	}
	defer unlock()
	if err = m.runner.Run(ctx, a.config, a.home, nativeState{}, true, in, out, stderr); err != nil {
		return err
	}
	state, err := readNative(a.config, a.home)
	if err != nil {
		return err
	}
	if !state.credential.ExpiresAt.After(time.Now()) {
		return errors.New("native login did not persist a usable credential")
	}

	return bindIdentity(a, state.credential, true)
}

func (m *Manager) current(ctx context.Context, name string, force bool) (credproxy.Credential, error) {
	a, ok := m.accounts[name]
	if !ok {
		return credproxy.Credential{}, errors.New("account alias is no longer configured")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := accountLock(a.home)
	if err != nil {
		return credproxy.Credential{}, err
	}
	defer unlock()
	state, err := readNative(a.config, a.home)
	if err == nil {
		err = bindIdentity(a, state.credential, false)
	}
	if err != nil {
		a.lastError = "login_required"

		return credproxy.Credential{}, err
	}
	blocked := a.rejectedGeneration == state.credential.Generation()
	if time.Until(state.credential.ExpiresAt) > 5*time.Minute && !blocked && (!force || time.Since(a.lastRefresh) < 30*time.Second) {
		a.lastError = ""

		return state.credential, nil
	}
	if time.Now().Before(a.retryAfter) {
		return credproxy.Credential{}, errors.New("native credential renewal is cooling down")
	}
	if blocked && time.Since(a.lastRefresh) < 30*time.Second {
		return credproxy.Credential{}, errors.New("recently renewed credential was rejected; waiting before native recovery")
	}
	renew, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err = m.runner.Run(renew, a.config, a.home, state, false, nil, io.Discard, io.Discard); err != nil {
		a.rejectedGeneration = state.credential.Generation()
		a.lastError = "native_refresh_failed"
		a.retryAfter = time.Now().Add(30 * time.Second)

		return credproxy.Credential{}, err
	}
	// The native CLI is the durable writer. A process exit alone is not a
	// successful refresh: reread its file and verify identity and expiry.
	next, err := readNative(a.config, a.home)
	if err == nil {
		err = bindIdentity(a, next.credential, false)
	}
	if err != nil || !next.credential.ExpiresAt.After(time.Now().Add(time.Minute)) || (next.credential.AccessToken == state.credential.AccessToken && !next.credential.ExpiresAt.After(state.credential.ExpiresAt)) {
		a.rejectedGeneration = state.credential.Generation()
		a.lastError = "native_refresh_invalid"
		a.retryAfter = time.Now().Add(30 * time.Second)

		return credproxy.Credential{}, errors.New("native CLI did not persist a valid renewal")
	}
	a.lastRefresh = time.Now()
	a.lastError = ""
	a.rejectedGeneration = ""

	return next.credential, nil
}

func (m *Manager) Resolve(ctx context.Context, r credproxy.ResolveRequest) (credproxy.Lease, error) {
	route, ok := m.config.Routes[r.Provider]
	if !ok || !r.Provider.Valid() {
		return credproxy.Lease{}, errors.New("provider is not configured")
	}
	if r.Purpose == credproxy.Control {
		c, err := m.current(ctx, route.Control, r.Force)
		if err == nil && r.Provider == credproxy.Claude {
			for _, scope := range []string{"user:profile", "user:inference", "user:sessions:claude_code"} {
				if !slices.Contains(strings.Fields(c.Scope), scope) {
					return credproxy.Lease{}, errors.New("control account requires native full-scope Claude login")
				}
			}
		}

		return credproxy.Lease{Account: route.Control, Credential: c}, err
	}
	if r.Purpose != credproxy.Inference || len(r.Session) != 64 {
		return credproxy.Lease{}, errors.New("inference requires a bounded stable session")
	}
	if _, err := hex.DecodeString(r.Session); err != nil {
		return credproxy.Lease{}, errors.New("invalid session digest")
	}
	key := string(r.Provider) + ":" + r.Session
	for {
		if err := ctx.Err(); err != nil {
			return credproxy.Lease{}, err
		}
		retrySelection := false
		m.mu.Lock()
		chosen := m.bindings.Sessions[key]
		next := m.bindings.Next[r.Provider]
		m.mu.Unlock()
		if chosen != "" {
			if !slices.Contains(route.Inference, chosen) {
				return credproxy.Lease{}, errors.New("session account removed from pool; existing sessions are never reassigned")
			}
			c, err := m.current(ctx, chosen, r.Force)

			return credproxy.Lease{Account: chosen, Credential: c}, err
		}
		for offset := range len(route.Inference) {
			name := route.Inference[(int(next%uint64(len(route.Inference)))+offset)%len(route.Inference)]
			m.mu.Lock()
			cool := time.Now().Before(m.cooldown[name])
			m.mu.Unlock()
			if cool {
				continue
			}
			c, err := m.current(ctx, name, false)
			if err != nil {
				continue
			}
			m.mu.Lock()
			if old := m.bindings.Sessions[key]; old != "" {
				m.mu.Unlock()

				retrySelection = true

				break
			}
			if m.bindings.Next[r.Provider] != next {
				m.mu.Unlock()
				retrySelection = true

				break
			}
			if time.Now().Before(m.cooldown[name]) {
				m.mu.Unlock()

				continue
			}
			if len(m.bindings.Sessions) >= m.config.MaxSessions {
				m.mu.Unlock()

				return credproxy.Lease{}, errors.New("session binding capacity reached; no bindings are evicted automatically")
			}
			m.bindings.Sessions[key] = name
			m.bindings.Next[r.Provider] = next + uint64(offset) + 1
			b, err := json.Marshal(m.bindings)
			if err == nil {
				err = savePrivate(filepath.Join(m.config.Root, "sessions.json"), b)
			}
			if err != nil {
				delete(m.bindings.Sessions, key)
				m.bindings.Next[r.Provider] = next
				m.mu.Unlock()

				return credproxy.Lease{}, errors.New("cannot persist session account binding")
			}
			m.mu.Unlock()

			return credproxy.Lease{Account: name, Credential: c}, nil
		}

		if retrySelection {
			continue
		}

		return credproxy.Lease{}, errors.New("no eligible inference account; renew or authorize a native account")
	}
}

func (m *Manager) Observe(ctx context.Context, o credproxy.Outcome) error {
	route, ok := m.config.Routes[o.Provider]
	if !ok {
		return errors.New("unknown outcome provider")
	}
	switch o.Purpose {
	case credproxy.Control:
		if o.Account != route.Control {
			return errors.New("control outcome identity mismatch")
		}
	case credproxy.Inference:
		m.mu.Lock()
		name := m.bindings.Sessions[string(o.Provider)+":"+o.Session]
		m.mu.Unlock()
		if name == "" || name != o.Account {
			return errors.New("inference outcome binding mismatch")
		}
	default:
		return errors.New("unknown outcome purpose")
	}
	if o.Status == httpUnauthorized {
		if len(o.Generation) != 64 {
			return errors.New("credential rejection requires a lease generation")
		}
		a, exists := m.accounts[o.Account]
		if !exists {
			return errors.New("outcome account is no longer configured")
		}
		a.mu.Lock()
		state, err := readNative(a.config, a.home)
		if err != nil {
			a.mu.Unlock()

			return errors.New("rejected native account unavailable")
		}
		if state.credential.Generation() != o.Generation {
			a.mu.Unlock()

			return nil
		}
		a.rejectedGeneration = o.Generation
		a.lastError = "upstream_credential_rejected"
		a.mu.Unlock()
		_, err = m.current(ctx, o.Account, true)

		return err
	}
	if o.Status == 429 {
		m.mu.Lock()
		m.cooldown[o.Account] = time.Now().Add(time.Duration(min(max(o.RetryAfter, 30), 3600)) * time.Second)
		m.mu.Unlock()
	}

	return nil
}

const httpUnauthorized = 401

type AccountStatus struct {
	Name      string             `json:"name"`
	Provider  credproxy.Provider `json:"provider"`
	State     string             `json:"state"`
	ExpiresAt time.Time          `json:"expires_at,omitzero"`
}

func (m *Manager) Status() []AccountStatus {
	result := make([]AccountStatus, 0, len(m.config.Accounts))
	for _, spec := range m.config.Accounts {
		a := m.accounts[spec.Name]
		a.mu.Lock()
		s := AccountStatus{Name: spec.Name, Provider: spec.Provider, State: "login_required"}
		state, err := readNative(spec, a.home)
		if err == nil && bindIdentity(a, state.credential, false) == nil {
			s.ExpiresAt = state.credential.ExpiresAt
			s.State = "ready"
			if !s.ExpiresAt.After(time.Now()) {
				s.State = "expired"
			}
			if a.lastError != "" {
				s.State = a.lastError
			}
		}
		a.mu.Unlock()
		result = append(result, s)
	}

	return result
}

func (m *Manager) Sweep(ctx context.Context) {
	for _, a := range m.config.Accounts {
		if ctx.Err() != nil {
			return
		}
		_, _ = m.current(ctx, a.Name, false)
	}
}
