package credentialowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// Quota is the usage a provider reports for a signed-in account through the
// read-only endpoints its native CLI and web app use (Claude Code /usage,
// Codex /status, ChatGPT reset credits). It is display only and never affects
// routing. Missing provider fields stay nil rather than becoming false or 0.
type Quota struct {
	Windows []QuotaWindow `json:"windows,omitempty"`
	// LimitReached is the provider's own verdict on the general limit.
	LimitReached *bool          `json:"limit_reached,omitempty"`
	Credits      *QuotaCredits  `json:"credits,omitempty"`
	ExtraUsage   *ExtraUsage    `json:"extra_usage,omitempty"`
	Resets       *ResetCredits  `json:"resets,omitempty"`
	FetchedAt    time.Time      `json:"fetched_at,omitzero"`
	// Error says why no report is shown. It never carries a response body.
	Error string `json:"error,omitempty"`
}

type QuotaWindow struct {
	// Bucket groups windows that share one limit: "" for the general limit,
	// otherwise a provider name such as "Code review" or "Opus".
	Bucket string `json:"bucket,omitempty"`
	// Name is the window length: "5h", "7d", or "<n>d"/"<n>h"/"<n>m".
	Name         string    `json:"name"`
	UsedPercent  float64   `json:"used_percent"`
	ResetsAt     time.Time `json:"resets_at,omitzero"`
	LimitReached bool      `json:"limit_reached,omitempty"`
}

// QuotaCredits is the ChatGPT credit balance Codex can draw on past its limits.
type QuotaCredits struct {
	HasCredits          bool   `json:"has_credits"`
	Unlimited           bool   `json:"unlimited"`
	Balance             string `json:"balance,omitempty"`
	ApproxLocalMessages []int  `json:"approx_local_messages,omitempty"`
	ApproxCloudMessages []int  `json:"approx_cloud_messages,omitempty"`
}

// ExtraUsage is Claude's pay-as-you-go usage past the plan limits.
type ExtraUsage struct {
	Enabled      bool     `json:"enabled"`
	MonthlyLimit *float64 `json:"monthly_limit,omitempty"`
	UsedCredits  *float64 `json:"used_credits,omitempty"`
	UsedPercent  *float64 `json:"used_percent,omitempty"`
}

// ResetCredits are ChatGPT's banked rate-limit resets for the account.
type ResetCredits struct {
	Eligible  *bool          `json:"purchase_eligible,omitempty"`
	Available *int           `json:"available,omitempty"`
	Statuses  map[string]int `json:"statuses,omitempty"`
	Error     string         `json:"error,omitempty"`
}

const (
	quotaTTL      = 5 * time.Minute
	quotaErrorTTL = time.Minute
	quotaTimeout  = 6 * time.Second
	quotaMaxBody  = 128 << 10
)

const (
	quotaUsage  = "usage"
	quotaResets = "resets"
)

var quotaURLs = map[credproxy.Provider]map[string]string{
	credproxy.Claude: {quotaUsage: "https://api.anthropic.com/api/oauth/usage"},
	credproxy.Codex: {
		quotaUsage:  "https://chatgpt.com/backend-api/wham/usage",
		quotaResets: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits",
	},
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
	urls    map[credproxy.Provider]map[string]string
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
		stale := entry.quota
		quota = Quota{Error: err.Error()}
		// A stale report is still more useful than none while the provider is
		// briefly unreachable.
		if cached && entry.generation == generation && len(stale.Windows) > 0 {
			stale.Error = quota.Error
			quota = stale
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

func fetchQuota(ctx context.Context, client *http.Client, urls map[string]string, c credproxy.Credential) (Quota, error) {
	if urls[quotaUsage] == "" {
		return Quota{}, errors.New("usage not supported for this provider")
	}
	body, err := quotaGet(ctx, client, urls[quotaUsage], c)
	if err != nil {
		return Quota{}, err
	}
	var quota Quota
	if c.Provider == credproxy.Claude {
		quota, err = claudeQuota(body)
	} else {
		quota, err = codexQuota(body)
	}
	if err != nil {
		return Quota{}, err
	}
	if url := urls[quotaResets]; url != "" {
		quota.Resets = fetchResets(ctx, client, url, c)
	}
	quota.FetchedAt = time.Now()

	return quota, nil
}

func quotaGet(ctx context.Context, client *http.Client, url string, c credproxy.Credential) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("usage request could not be built")
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
		return nil, errors.New("provider usage endpoint unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider usage endpoint returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, quotaMaxBody+1))
	if err != nil || len(body) > quotaMaxBody {
		return nil, errors.New("provider usage response unreadable")
	}

	return body, nil
}

func fetchResets(ctx context.Context, client *http.Client, url string, c credproxy.Credential) *ResetCredits {
	body, err := quotaGet(ctx, client, url, c)
	if err != nil {
		return &ResetCredits{Error: strings.Replace(err.Error(), "usage", "reset credits", 1)}
	}
	var doc struct {
		Eligible  *bool `json:"immediate_reset_purchase_eligible"`
		Available *int  `json:"available_count"`
		Credits   []struct {
			Status string `json:"status"`
		} `json:"credits"`
	}
	if json.Unmarshal(body, &doc) != nil || (doc.Available != nil && *doc.Available < 0) {
		return &ResetCredits{Error: "unexpected provider reset credits format"}
	}
	r := &ResetCredits{Eligible: doc.Eligible, Available: doc.Available}
	for _, credit := range doc.Credits {
		if r.Statuses == nil {
			r.Statuses = map[string]int{}
		}
		status := strings.ToLower(strings.TrimSpace(credit.Status))
		if status == "" {
			status = "unknown"
		}
		r.Statuses[status]++
	}

	return r
}

var claudeBuckets = map[string]string{
	"opus": "Opus", "sonnet": "Sonnet", "oauth_apps": "OAuth apps", "cowork": "Cowork", "omelette": "Omelette",
}

func claudeQuota(body []byte) (Quota, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return Quota{}, errors.New("unexpected provider usage format")
	}
	var quota Quota
	for key, raw := range doc {
		var name, bucket string
		switch {
		case key == "five_hour":
			name = "5h"
		case key == "seven_day":
			name = "7d"
		case strings.HasPrefix(key, "seven_day_"):
			name = "7d"
			suffix := strings.TrimPrefix(key, "seven_day_")
			bucket = claudeBuckets[suffix]
			if bucket == "" {
				bucket = suffix
			}
		default:
			continue
		}
		if string(raw) == "null" {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil || !finite(*w.Utilization) {
			continue
		}
		window := QuotaWindow{Bucket: bucket, Name: name, UsedPercent: *w.Utilization}
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			window.ResetsAt = t
		}
		quota.Windows = append(quota.Windows, window)
	}
	if raw, ok := doc["extra_usage"]; ok && string(raw) != "null" {
		var e struct {
			Enabled      bool     `json:"is_enabled"`
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  *float64 `json:"used_credits"`
			Utilization  *float64 `json:"utilization"`
		}
		if json.Unmarshal(raw, &e) == nil {
			quota.ExtraUsage = &ExtraUsage{Enabled: e.Enabled, MonthlyLimit: e.MonthlyLimit, UsedCredits: e.UsedCredits, UsedPercent: e.Utilization}
		}
	}
	if len(quota.Windows) == 0 {
		return Quota{}, errors.New("provider reported no usage windows")
	}
	sortWindows(quota.Windows)

	return quota, nil
}

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
}

// codexBlock is one Codex limit. Like the native CLI it accepts any
// "<name>_window" key and an optional "windows" object or list.
type codexBlock struct {
	windows      []codexWindow
	limitReached *bool
}

func (b *codexBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*b = codexBlock{}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := raw[key]
		switch {
		case key == "limit_reached":
			var reached bool
			if json.Unmarshal(value, &reached) == nil {
				b.limitReached = &reached
			}
		case key == "windows":
			var asObject map[string]*codexWindow
			if json.Unmarshal(value, &asObject) == nil {
				for _, w := range asObject {
					if w != nil {
						b.windows = append(b.windows, *w)
					}
				}

				continue
			}
			var asList []codexWindow
			if json.Unmarshal(value, &asList) == nil {
				b.windows = append(b.windows, asList...)
			}
		case strings.HasSuffix(key, "_window"):
			var w *codexWindow
			if json.Unmarshal(value, &w) == nil && w != nil {
				b.windows = append(b.windows, *w)
			}
		}
	}

	return nil
}

func codexQuota(body []byte) (Quota, error) {
	var doc struct {
		RateLimit  *codexBlock `json:"rate_limit"`
		CodeReview *codexBlock `json:"code_review_rate_limit"`
		Additional []struct {
			MeteredFeature string      `json:"metered_feature"`
			LimitName      string      `json:"limit_name"`
			RateLimit      *codexBlock `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		Credits *QuotaCredits `json:"credits"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return Quota{}, errors.New("unexpected provider usage format")
	}
	var quota Quota
	add := func(bucket string, block *codexBlock) {
		if block == nil {
			return
		}
		reached := block.limitReached != nil && *block.limitReached
		for _, w := range block.windows {
			if w.UsedPercent == nil || !finite(*w.UsedPercent) {
				continue
			}
			window := QuotaWindow{Bucket: bucket, Name: windowName(w.LimitWindowSeconds), UsedPercent: *w.UsedPercent, LimitReached: reached}
			switch {
			case w.ResetAt > 0:
				window.ResetsAt = time.Unix(w.ResetAt, 0)
			case w.ResetAfterSeconds > 0:
				window.ResetsAt = time.Now().Add(time.Duration(w.ResetAfterSeconds) * time.Second)
			}
			quota.Windows = append(quota.Windows, window)
		}
	}
	add("", doc.RateLimit)
	if doc.RateLimit != nil {
		quota.LimitReached = doc.RateLimit.limitReached
	}
	add("Code review", doc.CodeReview)
	for _, extra := range doc.Additional {
		add(codexBucketName(extra.MeteredFeature, extra.LimitName), extra.RateLimit)
	}
	quota.Credits = doc.Credits
	if len(quota.Windows) == 0 {
		return Quota{}, errors.New("provider reported no usage windows")
	}
	sortWindows(quota.Windows)

	return quota, nil
}

// codexBucketName follows the native CLI: codex_bengalfox is GPT-5.3-Codex-Spark.
func codexBucketName(feature, limit string) string {
	switch {
	case feature == "codex_bengalfox":
		return "GPT-5.3-Codex-Spark"
	case strings.TrimSpace(limit) != "":
		return strings.TrimSpace(limit)
	case strings.TrimSpace(feature) != "":
		return strings.TrimSpace(feature)
	default:
		return "Additional"
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

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

// sortWindows puts the general limit first and, within a bucket, the short
// window before the weekly one.
func sortWindows(windows []QuotaWindow) {
	order := map[string]int{"5h": 0, "7d": 1}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Bucket != windows[j].Bucket {
			if windows[i].Bucket == "" || windows[j].Bucket == "" {
				return windows[i].Bucket == ""
			}

			return windows[i].Bucket < windows[j].Bucket
		}
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
