package credproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type entry struct {
	mu          sync.Mutex
	credential  Credential
	retryAfter  time.Time
	pending     bool
	lastRefresh time.Time
}

// Store has one refresh lineage per provider for a single configured owner.
// Only the orchestrator writes it. CLI imports go through its private socket.
type Store struct {
	root    string
	entries map[Provider]*entry
	client  *http.Client
}

func OpenStore(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("credential root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("credential root must be a private 0700 directory")
	}
	s := &Store{root: root, entries: map[Provider]*entry{Claude: {}, Codex: {}}, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for p, e := range s.entries {
		path := s.path(p)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errors.New("credential files must be regular mode 0600")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &e.credential) != nil || e.credential.Validate() != nil || e.credential.Provider != p {
			return nil, errors.New("invalid stored credential")
		}
	}

	return s, nil
}
func (s *Store) path(p Provider) string { return filepath.Join(s.root, string(p)+".json") }
func (s *Store) Import(c Credential, replace bool) (GuestFiles, error) {
	if err := c.Validate(); err != nil {
		return GuestFiles{}, err
	}
	e := s.entries[c.Provider]
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.credential.AccountID != "" && e.credential.AccountID != c.AccountID && !replace {
		return GuestFiles{}, errors.New("account differs; explicit replacement required")
	}
	files, err := c.GuestFiles()
	if err != nil {
		return GuestFiles{}, err
	}
	if err = s.save(c); err != nil {
		return GuestFiles{}, err
	}
	e.credential = c
	e.retryAfter = time.Time{}
	e.pending = false

	return files, nil
}

func (s *Store) save(c Credential) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}

	return atomicPrivate(s.path(c.Provider), raw)
}

func atomicPrivate(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()

		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()

		return err
	}
	if err = f.Close(); err != nil {
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

func (s *Store) Current(ctx context.Context, p Provider) (Credential, error) {
	return s.current(ctx, p, false)
}

// Refresh coalesces guest-initiated refreshes, including reactive 401 recovery.
func (s *Store) Refresh(ctx context.Context, p Provider) (Credential, error) {
	return s.current(ctx, p, true)
}

func (s *Store) current(ctx context.Context, p Provider, force bool) (Credential, error) {
	e, ok := s.entries[p]
	if !ok {
		return Credential{}, errors.New("unknown provider")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.credential
	if e.pending {
		if err := s.save(c); err != nil {
			return Credential{}, errors.New("cannot persist refreshed credential")
		}
		e.pending = false
	}
	if c.AccessToken == "" {
		return Credential{}, errors.New("provider is not logged in")
	}
	if time.Until(c.ExpiresAt) > time.Minute && (!force || time.Since(e.lastRefresh) < 30*time.Second) {
		return c, nil
	}
	if time.Now().Before(e.retryAfter) {
		return Credential{}, errors.New("credential refresh temporarily unavailable")
	}
	next, err := s.refresh(ctx, c)
	if err != nil {
		e.retryAfter = time.Now().Add(15 * time.Second)
		// Even an unusable token response may already have rotated the lineage.
		if next.RefreshToken != "" && next.RefreshToken != c.RefreshToken {
			e.credential = next
			e.pending = s.save(next) != nil
		}
		return Credential{}, err
	}
	// Retain a rotated refresh token in memory even if disk persistence fails.
	// Fail closed until the new lineage has been durably persisted.
	e.credential = next
	e.lastRefresh = time.Now()
	if err = s.save(next); err != nil {
		e.retryAfter = time.Now().Add(15 * time.Second)
		e.pending = true

		return Credential{}, errors.New("cannot persist refreshed credential")
	}

	return next, nil
}

func (s *Store) Guest(ctx context.Context, p Provider) (GuestFiles, error) {
	c, err := s.Current(ctx, p)
	if err != nil {
		return GuestFiles{}, err
	}

	return c.GuestFiles()
}

func (s *Store) refresh(ctx context.Context, c Credential) (Credential, error) {
	body := map[string]string{"grant_type": "refresh_token", "refresh_token": c.RefreshToken, "client_id": "app_EMoamEEZ73f0CkXaXp7hrann"}
	endpoint := "https://auth.openai.com/oauth/token"

	if c.Provider == Claude {
		body["client_id"] = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
		endpoint = "https://platform.claude.com/v1/oauth/token"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Credential{}, errors.New("cannot encode refresh request")
	}
	contentType := "application/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.client.Do(req)
	if err != nil {
		return Credential{}, errors.New("credential refresh transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credential{}, fmt.Errorf("credential refresh HTTP %d", resp.StatusCode)
	}
	var out struct {
		Access  string          `json:"access_token"`
		Refresh string          `json:"refresh_token"`
		ID      string          `json:"id_token"`
		Expires int64           `json:"expires_in"`
		Scope   json.RawMessage `json:"scope"`
	}

	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil {
		return Credential{}, errors.New("invalid refresh response")
	}
	if out.Refresh != "" {
		c.RefreshToken = out.Refresh
	}
	if out.Access != "" {
		c.AccessToken = out.Access
	}
	if out.ID != "" {
		c.IDToken = out.ID
	}
	if out.Expires > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(out.Expires) * time.Second)
	} else {
		c.ExpiresAt = accessExpiry(c.AccessToken)
	}
	if out.Access == "" || !c.ExpiresAt.After(time.Now()) {
		c.ExpiresAt = time.Now()
		return c, errors.New("invalid refresh token expiry")
	}
	if len(out.Scope) > 0 {
		var scope string
		if json.Unmarshal(out.Scope, &scope) == nil {
			c.Scope = scope
		} else {
			var scopes []string
			if json.Unmarshal(out.Scope, &scopes) == nil {
				for i, v := range scopes {
					if i == 0 {
						c.Scope = v
					} else {
						c.Scope += " " + v
					}
				}
			}
		}
	}

	return c, nil
}
