package credentialowner

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// The administrative policy (accounts and routes) is persisted in the owner
// root. Once policy.json exists it replaces the accounts and routes of the
// static configuration, which then only seeds a fresh root and supplies the
// native binaries.
const policyFile = "policy.json"

type storedAccount struct {
	Name     string             `json:"name"`
	Provider credproxy.Provider `json:"provider"`
}

type storedPolicy struct {
	Version  int                          `json:"version"`
	Accounts []storedAccount              `json:"accounts"`
	Routes   map[credproxy.Provider]Route `json:"routes"`
}

// binaryFor resolves the native CLI of an account: the configured default for
// the provider, else the binary the static configuration gave this alias or
// any alias of the same provider.
func binaryFor(c Config, name string, p credproxy.Provider) (string, bool) {
	if b := c.Binaries[p]; b != "" {
		return b, true
	}
	fallback := ""
	for _, a := range c.Accounts {
		if a.Provider != p {
			continue
		}
		if a.Name == name {
			return a.Binary, true
		}
		if fallback == "" {
			fallback = a.Binary
		}
	}

	return fallback, fallback != ""
}

func loadPolicy(c Config) (Config, error) {
	path := filepath.Join(c.Root, policyFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return c, err
	}
	b, err := privateRead(path)
	if err != nil {
		return c, err
	}
	var p storedPolicy
	if json.Unmarshal(b, &p) != nil || p.Version != 1 {
		return c, errors.New("invalid persisted routing policy")
	}
	next := c
	next.Accounts = nil
	for _, a := range p.Accounts {
		binary, ok := binaryFor(c, a.Name, a.Provider)
		if !ok {
			return c, fmt.Errorf("no native binary configured for provider %s", a.Provider)
		}
		next.Accounts = append(next.Accounts, Account{Name: a.Name, Provider: a.Provider, Binary: binary})
	}
	next.Routes = p.Routes
	if err = next.validate(); err != nil {
		return c, err
	}

	return next, nil
}

func savePolicy(c Config) error {
	p := storedPolicy{Version: 1, Routes: c.Routes}
	for _, a := range c.Accounts {
		p.Accounts = append(p.Accounts, storedAccount{Name: a.Name, Provider: a.Provider})
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	return savePrivate(filepath.Join(c.Root, policyFile), b)
}

// PolicyView is the administrative projection. It never contains credentials.
type PolicyView struct {
	Accounts []storedAccount              `json:"accounts"`
	Routes   map[credproxy.Provider]Route `json:"routes"`
	// Providers lists providers that accounts can be added for.
	Providers []credproxy.Provider `json:"providers"`
}

func (m *Manager) Policy() PolicyView {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	v := PolicyView{Accounts: []storedAccount{}, Routes: map[credproxy.Provider]Route{}, Providers: []credproxy.Provider{}}
	for _, a := range m.config.Accounts {
		v.Accounts = append(v.Accounts, storedAccount{Name: a.Name, Provider: a.Provider})
	}
	for p, r := range m.config.Routes {
		r.Members = slices.Clone(r.Members)
		r.Inference = nil
		v.Routes[p] = r
	}
	for _, p := range []credproxy.Provider{credproxy.Claude, credproxy.Codex} {
		if _, ok := binaryFor(m.config, "", p); ok {
			v.Providers = append(v.Providers, p)
		}
	}

	return v
}

// commit validates and persists a candidate policy, then swaps it in. The
// caller holds pmu for writing.
func (m *Manager) commit(next Config) error {
	if err := next.validate(); err != nil {
		return err
	}
	if err := savePolicy(next); err != nil {
		return errors.New("cannot persist routing policy")
	}
	m.config = next

	return nil
}

func (m *Manager) candidate() Config {
	next := m.config
	next.Accounts = slices.Clone(m.config.Accounts)
	next.Routes = maps.Clone(m.config.Routes)

	return next
}

// SetRoute replaces the strategy and inference pool of a provider. The
// control account of an existing route is fixed for the life of the owner
// root; a new provider route binds its control account permanently.
func (m *Manager) SetRoute(p credproxy.Provider, r Route) error {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if !p.Valid() {
		return errors.New("unknown provider")
	}
	r.Inference = nil
	r = r.normalize()
	old, exists := m.config.Routes[p]
	if exists && r.Control != old.Control {
		return errors.New("the control account cannot change in an existing owner root; cold migration required")
	}
	next := m.candidate()
	next.Routes[p] = r
	if exists {
		return m.commit(next)
	}
	// A new route also binds its control account in sessions.json, the record
	// Open checks so that a control identity never changes silently.
	m.mu.Lock()
	defer m.mu.Unlock()
	controls := maps.Clone(m.bindings.Controls)
	m.bindings.Controls[p] = r.Control
	b, err := json.Marshal(m.bindings)
	if err == nil {
		err = m.commit(next)
	}
	if err == nil {
		if err = savePrivate(filepath.Join(m.config.Root, "sessions.json"), b); err != nil {
			err = errors.New("cannot persist control binding")
		}
	}
	if err != nil {
		m.bindings.Controls = controls
	}

	return err
}

// AddAccount registers a new native account alias with an empty login home.
func (m *Manager) AddAccount(name string, p credproxy.Provider) error {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if !namePattern.MatchString(name) || !p.Valid() {
		return errors.New("account names use letters, digits, '-' and '_' (max 64)")
	}
	if _, ok := m.accounts[name]; ok {
		return errors.New("an account with this name already exists")
	}
	if len(m.config.Accounts) >= 128 {
		return errors.New("account limit reached")
	}
	binary, ok := binaryFor(m.config, name, p)
	if !ok {
		return fmt.Errorf("no native binary configured for provider %s", p)
	}
	a := Account{Name: name, Provider: p, Binary: binary}
	state, err := newAccountState(m.config.Root, a)
	if err != nil {
		return err
	}
	next := m.candidate()
	next.Accounts = append(next.Accounts, a)
	if err = m.commit(next); err != nil {
		return err
	}
	m.accounts[name] = state

	return nil
}

// RemoveAccount retires an alias that is neither a control account nor in any
// pool. Its native home moves aside, so the alias can be reused by a new
// login without inheriting the old identity binding.
func (m *Manager) RemoveAccount(name string) error {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	a, ok := m.accounts[name]
	if !ok {
		return errors.New("unknown account")
	}
	for _, r := range m.config.Routes {
		if r.Control == name {
			return errors.New("a control account cannot be removed")
		}
		if slices.Contains(r.Inference, name) {
			return errors.New("remove the account from its routing pool first")
		}
	}
	if a.signingIn.Load() || !a.mu.TryLock() {
		return errors.New("the account is busy; try again")
	}
	defer a.mu.Unlock()
	retired := filepath.Join(m.config.Root, "native", ".retired")
	if err := privateDir(retired); err != nil {
		return err
	}
	moved := filepath.Join(retired, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	if err := os.Rename(a.home, moved); err != nil {
		return errors.New("cannot retire the account home")
	}
	next := m.candidate()
	next.Accounts = slices.DeleteFunc(next.Accounts, func(x Account) bool { return x.Name == name })
	if err := m.commit(next); err != nil {
		_ = os.Rename(moved, a.home)

		return err
	}
	delete(m.accounts, name)

	return nil
}
