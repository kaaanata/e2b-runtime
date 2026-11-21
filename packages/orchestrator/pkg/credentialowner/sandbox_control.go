package credentialowner

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

const (
	controlStaged  = "staged"
	controlRestart = "restart"
)

type sandboxControlLaunch struct {
	Directory string `json:"directory"`
	Name      string `json:"name,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

type sandboxControlSwitch struct {
	From                string               `json:"from"`
	To                  string               `json:"to"`
	PreviousFingerprint string               `json:"previousFingerprint"`
	Fingerprint         string               `json:"fingerprint"`
	Phase               string               `json:"phase"`
	Launch              sandboxControlLaunch `json:"launch"`
}

type sandboxControlView struct {
	Account string                `json:"account"`
	Pending *sandboxControlSwitch `json:"pending,omitempty"`
}

func controlKey(p credproxy.Provider, sandboxID string) string {
	return string(p) + ":" + sandboxID
}

var (
	controlSandboxID           = regexp.MustCompile("^[a-z0-9]{20}$")
	controlIdentityFingerprint = regexp.MustCompile("^[0-9a-f]{64}$")
)

func validControlLaunch(p credproxy.Provider, launch sandboxControlLaunch) bool {
	dir := launch.Directory
	if dir != "/workspace" && (path.Clean(dir) != dir || !strings.HasPrefix(dir, "/workspace/")) {
		return false
	}
	if p == credproxy.Codex {
		return true
	}
	return launch.Name != "" && len(launch.Name) <= 256 &&
		slices.Contains([]string{"default", "acceptEdits", "plan", "bypassPermissions", "dontAsk"}, launch.Mode)
}

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
	for key, pending := range m.bindings.ControlSwitches {
		provider, id, ok := strings.Cut(key, ":")
		p := credproxy.Provider(provider)
		from, to := m.accounts[pending.From], m.accounts[pending.To]
		route := m.config.Routes[p]
		if !ok || !controlSandboxID.MatchString(id) || !p.Valid() || route.Control == "" ||
			from == nil || to == nil || from.config.Provider != p || to.config.Provider != p ||
			!controlIdentityFingerprint.MatchString(pending.PreviousFingerprint) ||
			!controlIdentityFingerprint.MatchString(pending.Fingerprint) || !validControlLaunch(p, pending.Launch) {
			return errors.New("invalid persisted sandbox control switch")
		}
		current := m.controlAliasLocked(p, id, route.Control)
		if pending.Phase != controlStaged && pending.Phase != controlRestart ||
			pending.Phase == controlStaged && current != pending.From ||
			pending.Phase == controlRestart && current != pending.To {
			return errors.New("inconsistent persisted sandbox control switch")
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
	if account := m.bindings.SandboxControls[controlKey(p, sandboxID)]; account != "" {
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

func (m *Manager) sandboxControl(p credproxy.Provider, id string) (sandboxControlView, error) {
	route, ok := m.route(p)
	if !ok || !p.Valid() || !controlSandboxID.MatchString(id) {
		return sandboxControlView{}, errors.New("invalid sandbox control scope")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := sandboxControlView{Account: m.controlAliasLocked(p, id, route.Control)}
	if pending, ok := m.bindings.ControlSwitches[controlKey(p, id)]; ok {
		view.Pending = &pending
	}
	return view, nil
}

func (m *Manager) validateControlTarget(ctx context.Context, p credproxy.Provider, name string) (*accountState, credproxy.GuestFiles, error) {
	account, ok := m.account(name)
	if !ok || account.config.Provider != p || !namePattern.MatchString(name) {
		return nil, credproxy.GuestFiles{}, errors.New("the account must belong to the selected provider")
	}
	lease, err := m.resolveControl(ctx, p, name, false)
	if err != nil {
		return nil, credproxy.GuestFiles{}, errors.New("the selected control account is unavailable")
	}
	files, err := lease.Credential.GuestFiles()
	return account, files, err
}

// persistControl serializes with inference binding writes under m.mu.
func (m *Manager) persistControl(next bindings) error {
	raw, err := json.Marshal(next)
	if err == nil {
		err = savePrivate(filepath.Join(m.config.Root, "sessions.json"), raw)
	}
	if err != nil {
		return errors.New("cannot persist sandbox control binding")
	}
	m.bindings = next
	return nil
}

func (m *Manager) stageSandboxControl(ctx context.Context, p credproxy.Provider, id string, change sandboxControlSwitch) error {
	expected, target := change.From, change.To
	if !p.Valid() || !controlSandboxID.MatchString(id) || expected == "" || expected == target ||
		!controlIdentityFingerprint.MatchString(change.PreviousFingerprint) || !controlIdentityFingerprint.MatchString(change.Fingerprint) ||
		!validControlLaunch(p, change.Launch) {
		return errors.New("invalid sandbox control switch")
	}
	account, _, err := m.validateControlTarget(ctx, p, target)
	if err != nil {
		return err
	}
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	route, ok := m.config.Routes[p]
	if !ok || m.accounts[target] != account {
		return errors.New("the selected control account changed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := controlKey(p, id)
	if pending, exists := m.bindings.ControlSwitches[key]; exists {
		if pending.From == expected && pending.To == target &&
			pending.PreviousFingerprint == change.PreviousFingerprint && pending.Fingerprint == change.Fingerprint {
			return nil
		}
		return errors.New("resume the pending switch for this agent first")
	}
	if m.controlAliasLocked(p, id, route.Control) != expected {
		return errors.New("the sandbox control binding changed")
	}
	if len(m.bindings.ControlSwitches) >= m.config.MaxSessions ||
		m.bindings.SandboxControls[key] == "" && len(m.bindings.SandboxControls) >= m.config.MaxSessions {
		return errors.New("sandbox control binding capacity reached")
	}
	next := m.bindings
	next.ControlSwitches = maps.Clone(next.ControlSwitches)
	change.Phase = controlStaged
	next.ControlSwitches[key] = change
	return m.persistControl(next)
}

// setSandboxControl compares and replaces a scope. In production the caller
// requires a prepared switch and uses this after validating guest quiescence.
func (m *Manager) setSandboxControl(ctx context.Context, p credproxy.Provider, id, expected, target string) error {
	if !p.Valid() || !controlSandboxID.MatchString(id) || expected == "" {
		return errors.New("invalid sandbox control binding request")
	}
	account, _, err := m.validateControlTarget(ctx, p, target)
	if err != nil {
		return err
	}
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	route, ok := m.config.Routes[p]
	if !ok || m.accounts[target] != account {
		return errors.New("the selected control account changed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := controlKey(p, id)
	pending, switching := m.bindings.ControlSwitches[key]
	if !switching || pending.From != expected || pending.To != target {
		return errors.New("the staged control switch changed")
	}
	current := m.controlAliasLocked(p, id, route.Control)
	if current == target {
		return nil
	}
	if current != expected {
		return errors.New("the sandbox control binding changed")
	}
	if m.bindings.SandboxControls[key] == "" && len(m.bindings.SandboxControls) >= m.config.MaxSessions {
		return errors.New("sandbox control binding capacity reached")
	}
	next := m.bindings
	next.SandboxControls = maps.Clone(next.SandboxControls)
	next.SandboxControls[key] = target
	next.ControlSwitches = maps.Clone(next.ControlSwitches)
	pending.Phase = controlRestart
	next.ControlSwitches[key] = pending
	return m.persistControl(next)
}

func (m *Manager) finishSandboxControl(p credproxy.Provider, id, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := controlKey(p, id)
	pending, ok := m.bindings.ControlSwitches[key]
	if !ok && m.bindings.SandboxControls[key] == target {
		return nil
	}
	if pending.Phase != controlRestart || pending.To != target || m.bindings.SandboxControls[key] != target {
		return errors.New("the sandbox control switch is not ready to finish")
	}
	next := m.bindings
	next.ControlSwitches = maps.Clone(next.ControlSwitches)
	delete(next.ControlSwitches, key)
	return m.persistControl(next)
}
