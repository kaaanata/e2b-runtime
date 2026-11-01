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

type Route struct {
	Control   string   `json:"control"`
	Inference []string `json:"inference"`
}

type Config struct {
	Version     int                          `json:"version"`
	Root        string                       `json:"root"`
	Accounts    []Account                    `json:"accounts"`
	Routes      map[credproxy.Provider]Route `json:"routes"`
	MaxSessions int                          `json:"max_sessions"`
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
	for p, r := range c.Routes {
		if !p.Valid() || accounts[r.Control].Provider != p {
			return errors.New("route requires a matching control account")
		}
		seen := map[string]bool{}
		for _, name := range r.Inference {
			if accounts[name].Provider != p || seen[name] {
				return errors.New("invalid or duplicate inference account")
			}
			seen[name] = true
		}
	}
	if len(c.Routes) == 0 {
		return errors.New("at least one provider route is required")
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
