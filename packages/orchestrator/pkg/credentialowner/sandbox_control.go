package credentialowner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

var controlSandboxID = regexp.MustCompile("^[a-z0-9]{20}$")

func (m *Manager) validateSandboxControls() error {
	for key, name := range m.bindings.SandboxControls {
		provider, id, ok := strings.Cut(key, ":")
		p := credproxy.Provider(provider)
		account := m.accounts[name]
		if !ok || !controlSandboxID.MatchString(id) || !p.Valid() ||
			m.config.Routes[p].Control == "" || account == nil || account.config.Provider != p {
			return errors.New("invalid persisted sandbox control binding")
		}
	}
	return nil
}

func (m *Manager) controlAlias(p credproxy.Provider, sandboxID, fallback string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controlAliasLocked(p, sandboxID, fallback)
}

func (m *Manager) controlAliasLocked(p credproxy.Provider, sandboxID, fallback string) string {
	if account := m.bindings.SandboxControls[string(p)+":"+sandboxID]; account != "" {
		return account
	}
	return fallback
}

func (m *Manager) resolveControl(ctx context.Context, p credproxy.Provider, account string, force bool) (credproxy.Lease, error) {
	c, err := m.current(ctx, account, force)
	if err == nil && p == credproxy.Claude {
		for _, scope := range []string{"user:profile", "user:inference", "user:sessions:claude_code"} {
			if !slices.Contains(strings.Fields(c.Scope), scope) {
				return credproxy.Lease{}, errors.New("control account requires native full-scope Claude login")
			}
		}
	}
	return credproxy.Lease{Account: account, Credential: c}, err
}

// setSandboxControl is the owner-side compare-and-swap used by a controller
// after it has fenced the target Agent. Only the controller can call this
// operation, after proving the target sandbox and stopping its Agent.
func (m *Manager) setSandboxControl(ctx context.Context, p credproxy.Provider, sandboxID, expected, target string) error {
	if !p.Valid() || !controlSandboxID.MatchString(sandboxID) || expected == "" || !namePattern.MatchString(target) {
		return errors.New("invalid sandbox control binding request")
	}
	account, ok := m.account(target)
	if !ok || account.config.Provider != p {
		return errors.New("the account must belong to the selected provider")
	}
	if _, err := m.resolveControl(ctx, p, target, false); err != nil {
		return errors.New("the selected control account is unavailable")
	}
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	if m.accounts[target] != account || m.config.Routes[p].Control == "" {
		return errors.New("the account must belong to the selected provider")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	key := string(p) + ":" + sandboxID
	old := m.bindings.SandboxControls[key]
	current := m.controlAliasLocked(p, sandboxID, m.config.Routes[p].Control)
	if current == target {
		return nil
	}
	if current != expected {
		return errors.New("the sandbox control binding changed")
	}
	if old == "" && len(m.bindings.SandboxControls) >= m.config.MaxSessions {
		return errors.New("sandbox control binding capacity reached")
	}
	m.bindings.SandboxControls[key] = target
	raw, err := json.Marshal(m.bindings)
	if err == nil {
		err = savePrivate(filepath.Join(m.config.Root, "sessions.json"), raw)
	}
	if err != nil {
		if old == "" {
			delete(m.bindings.SandboxControls, key)
		} else {
			m.bindings.SandboxControls[key] = old
		}
		return errors.New("cannot persist sandbox control binding")
	}
	return nil
}
