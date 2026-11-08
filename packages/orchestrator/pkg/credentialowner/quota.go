package credentialowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// Quota is the usage a provider reports for a signed-in account through the
// same endpoint its native CLI uses (Claude Code /usage, Codex /status). It is
// display only and never affects routing.
type Quota struct {
	Windows   []QuotaWindow `json:"windows,omitempty"`
	FetchedAt time.Time     `json:"fetched_at,omitzero"`
	// Error says why no report is shown. It never carries a response body.
	Error string `json:"error,omitempty"`
}

type QuotaWindow struct {
	// Name is "5h", "7d", or "7d_<model>" for a model-specific weekly window.
	Name        string    `json:"name"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

const (
	quotaTTL      = 5 * time.Minute
	quotaErrorTTL = time.Minute
	quotaTimeout  = 6 * time.Second
	quotaMaxBody  = 64 << 10
)

var quotaURLs = map[credproxy.Provider]string{
	credproxy.Claude: "https://api.anthropic.com/api/oauth/usage",
	credproxy.Codex:  "https://chatgpt.com/backend-api/wham/usage",
}

type quotaEntry struct {
	quota      Quota
	generation string
	at         time.Time
}

type quotaCache struct {
	mu      sync.Mutex
	entries map[string]quotaEntry
	client  *http.Client
	urls    map[credproxy.Provider]string
}

// StatusWithQuota is Status plus each ready account's provider usage, cached
// for quotaTTL. Fetches run in parallel without holding any account lock.
func (m *Manager) StatusWithQuota(ctx context.Context) []AccountStatus {
	result := m.Status()
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := range result {
		if result[i].State != "ready" {
			continue
		}
		a, ok := m.account(result[i].Name)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(s *AccountStatus) {
			defer wg.Done()
			q := m.quotaFor(ctx, a)
			s.Quota = &q
		}(&result[i])
	}
	wg.Wait()

	return result
}

func (m *Manager) quotaFor(ctx context.Context, a *accountState) Quota {
	a.mu.Lock()
	state, err := readNative(a.config, a.home)
	a.mu.Unlock()
	if err != nil {
		return Quota{Error: "native sign-in unavailable"}
	}
	c := state.credential
	generation := c.Generation()
	q := &m.quotas
	q.mu.Lock()
	entry, cached := q.entries[a.config.Name]
	client, urls := q.client, q.urls
	q.mu.Unlock()
	if cached && entry.generation == generation {
		ttl := quotaTTL
		if entry.quota.Error != "" {
			ttl = quotaErrorTTL
		}
		if time.Since(entry.at) < ttl {
			return entry.quota
		}
	}
	if client == nil {
		client = &http.Client{Timeout: quotaTimeout}
	}
	if urls == nil {
		urls = quotaURLs
	}
	quota, err := fetchQuota(ctx, client, urls[c.Provider], c)
	if err != nil {
		quota = Quota{Error: err.Error()}
		// A stale report is still more useful than none while the provider is
		// briefly unreachable.
		if cached && entry.generation == generation && len(entry.quota.Windows) > 0 {
			quota.Windows, quota.FetchedAt = entry.quota.Windows, entry.quota.FetchedAt
		}
	}
	q.mu.Lock()
	if q.entries == nil {
		q.entries = map[string]quotaEntry{}
	}
	q.entries[a.config.Name] = quotaEntry{quota: quota, generation: generation, at: time.Now()}
	q.mu.Unlock()

	return quota
}

func fetchQuota(ctx context.Context, client *http.Client, url string, c credproxy.Credential) (Quota, error) {
	if url == "" {
		return Quota{}, errors.New("usage not supported for this provider")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Quota{}, errors.New("usage request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	if c.Provider == credproxy.Claude {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else {
		req.Header.Set("ChatGPT-Account-Id", c.AccountID)
		req.Header.Set("User-Agent", "codex_cli_rs")
	}
	res, err := client.Do(req)
	if err != nil {
		return Quota{}, errors.New("provider usage endpoint unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Quota{}, fmt.Errorf("provider usage endpoint returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, quotaMaxBody+1))
	if err != nil || len(body) > quotaMaxBody {
		return Quota{}, errors.New("provider usage response unreadable")
	}
	var windows []QuotaWindow
	if c.Provider == credproxy.Claude {
		windows, err = claudeWindows(body)
	} else {
		windows, err = codexWindows(body)
	}
	if err != nil {
		return Quota{}, err
	}

	return Quota{Windows: windows, FetchedAt: time.Now()}, nil
}

func claudeWindows(body []byte) ([]QuotaWindow, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, errors.New("unexpected provider usage format")
	}
	names := map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_opus": "7d_opus", "seven_day_sonnet": "7d_sonnet"}
	var windows []QuotaWindow
	for key, name := range names {
		raw, ok := doc[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		window := QuotaWindow{Name: name, UsedPercent: *w.Utilization}
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			window.ResetsAt = t
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, errors.New("provider reported no usage windows")
	}
	sortWindows(windows)

	return windows, nil
}

func codexWindows(body []byte) ([]QuotaWindow, error) {
	type window struct {
		UsedPercent        *float64 `json:"used_percent"`
		LimitWindowSeconds int64    `json:"limit_window_seconds"`
		ResetAt            int64    `json:"reset_at"`
		ResetAfterSeconds  int64    `json:"reset_after_seconds"`
	}
	var doc struct {
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil, errors.New("unexpected provider usage format")
	}
	if doc.RateLimit == nil {
		return nil, errors.New("provider reported no usage windows")
	}
	var windows []QuotaWindow
	for _, w := range []*window{doc.RateLimit.Primary, doc.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		q := QuotaWindow{Name: windowName(w.LimitWindowSeconds), UsedPercent: *w.UsedPercent}
		switch {
		case w.ResetAt > 0:
			q.ResetsAt = time.Unix(w.ResetAt, 0)
		case w.ResetAfterSeconds > 0:
			q.ResetsAt = time.Now().Add(time.Duration(w.ResetAfterSeconds) * time.Second)
		}
		windows = append(windows, q)
	}
	if len(windows) == 0 {
		return nil, errors.New("provider reported no usage windows")
	}
	sortWindows(windows)

	return windows, nil
}

func windowName(seconds int64) string {
	switch {
	case seconds <= 0:
		return "window"
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dm", seconds/60)
	}
}

// sortWindows puts the short window first, then weekly ones.
func sortWindows(windows []QuotaWindow) {
	order := map[string]int{"5h": 0, "7d": 1, "7d_opus": 2, "7d_sonnet": 3}
	sort.SliceStable(windows, func(i, j int) bool {
		oi, iok := order[windows[i].Name]
		oj, jok := order[windows[j].Name]
		if iok && jok {
			return oi < oj
		}
		if iok != jok {
			return iok
		}

		return windows[i].Name < windows[j].Name
	})
}
