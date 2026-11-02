// Package credentialowner runs outside sandboxes and the egress proxy. Native
// Claude Code and Codex exclusively own login and OAuth refresh persistence.
package credentialowner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

type Account struct {
	Name     string             `json:"name"`
	Provider credproxy.Provider `json:"provider"`
	Binary   string             `json:"binary"`
}

// Strategy orders the inference pool for NEW sessions only. Every strategy
// keeps the existing strict binding: a bound session never moves accounts.
type Strategy string

const (
	Rotate Strategy = "rotate" // each enabled member becomes first choice in turn
	Order  Strategy = "order"  // first eligible member in list order
	Weight Strategy = "weight" // smooth weighted round robin over enabled members
)

func (s Strategy) Valid() bool { return s == Rotate || s == Order || s == Weight }

// Member is one inference pool entry. A disabled member takes no new sessions
// but keeps serving the sessions already bound to it; removing it from the
// pool instead fails those sessions closed.
type Member struct {
	Account  string `json:"account"`
	Weight   int    `json:"weight,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

const maxWeight = 100

type Route struct {
	Control  string   `json:"control"`
	Strategy Strategy `json:"strategy,omitempty"`
	Members  []Member `json:"members,omitempty"`
	// Inference is the legacy member list; normalize derives it from Members.
	Inference []string `json:"inference,omitempty"`
}

type Config struct {
	Version     int                          `json:"version"`
	Root        string                       `json:"root"`
	Accounts    []Account                    `json:"accounts"`
	Routes      map[credproxy.Provider]Route `json:"routes"`
	MaxSessions int                          `json:"max_sessions"`
	// Binaries is the native CLI used for accounts added at runtime.
	Binaries map[credproxy.Provider]string `json:"binaries,omitempty"`
}

// normalize fills defaults and makes Members the source of truth.
func (r Route) normalize() Route {
	if r.Strategy == "" {
		r.Strategy = Rotate
	}
	if len(r.Members) == 0 {
		for _, name := range r.Inference {
			r.Members = append(r.Members, Member{Account: name})
		}
	}
	members := make([]Member, len(r.Members))
	r.Inference = make([]string, len(r.Members))
	for i, m := range r.Members {
		if m.Weight == 0 {
			m.Weight = 1
		}
		members[i] = m
		r.Inference[i] = m.Account
	}
	r.Members = members

	return r
}

// schedule is the new-session selection cycle over enabled members.
func (r Route) schedule() []string {
	var enabled []Member
	for _, m := range r.Members {
		if !m.Disabled {
			enabled = append(enabled, m)
		}
	}
	if r.Strategy != Weight {
		names := make([]string, len(enabled))
		for i, m := range enabled {
			names[i] = m.Account
		}

		return names
	}
	total := 0
	for _, m := range enabled {
		total += m.Weight
	}
	current := make([]int, len(enabled))
	cycle := make([]string, 0, total)
	for range total {
		best := 0
		for i, m := range enabled {
			current[i] += m.Weight
			if current[i] > current[best] {
				best = i
			}
		}
		current[best] -= total
		cycle = append(cycle, enabled[best].Account)
	}

	return cycle
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func LoadConfig(path string) (Config, error) {
	b, err := privateRead(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid credential owner configuration")
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}

	return c, nil
}

func (c *Config) validate() error {
	if c.MaxSessions == 0 {
		c.MaxSessions = 10000
	}
	if c.Version != 1 || !filepath.IsAbs(c.Root) || c.MaxSessions < 1 || c.MaxSessions > 50000 || len(c.Accounts) > 128 {
		return errors.New("invalid credential owner version, root, account count or session limit")
	}
	accounts := make(map[string]Account)
	for _, a := range c.Accounts {
		if !namePattern.MatchString(a.Name) || !a.Provider.Valid() || !filepath.IsAbs(a.Binary) {
			return errors.New("invalid native account configuration")
		}
		if _, ok := accounts[a.Name]; ok {
			return errors.New("duplicate account alias")
		}
		accounts[a.Name] = a
	}
	for p, b := range c.Binaries {
		if !p.Valid() || !filepath.IsAbs(b) {
			return errors.New("invalid native binary configuration")
		}
	}
	for p, r := range c.Routes {
		r = r.normalize()
		if err := r.validate(p, accounts); err != nil {
			return err
		}
		c.Routes[p] = r
	}
	if len(c.Routes) == 0 {
		return errors.New("at least one provider route is required")
	}

	return nil
}

func (r Route) validate(p credproxy.Provider, accounts map[string]Account) error {
	if !p.Valid() || accounts[r.Control].Provider != p {
		return errors.New("route requires a matching control account")
	}
	if !r.Strategy.Valid() {
		return errors.New("unknown routing strategy")
	}
	seen := map[string]bool{}
	for _, m := range r.Members {
		if accounts[m.Account].Provider != p || seen[m.Account] {
			return errors.New("invalid or duplicate inference account")
		}
		if m.Weight < 1 || m.Weight > maxWeight {
			return errors.New("inference weight must be between 1 and 100")
		}
		seen[m.Account] = true
	}

	return nil
}

func privateRead(path string) ([]byte, error) {
	return privateReadLimit(path, 1<<20)
}

func privateReadLimit(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open private file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, errors.New("private regular file required within size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("cannot read private file")
	}

	return b, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("native owner directory must be private and not a symlink")
	}

	return nil
}
