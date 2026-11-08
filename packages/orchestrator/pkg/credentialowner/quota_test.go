package credentialowner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

type quotaReply struct {
	status int
	body   string
}

func quotaServer(t *testing.T, replies map[string]quotaReply) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("usage request used %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("usage request without bearer token")
		}
		reply, ok := replies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)

			return
		}
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

func withQuotaServer(m *Manager, p credproxy.Provider, srv *httptest.Server, resets bool) {
	m.quotas.client = srv.Client()
	urls := map[string]string{quotaUsage: srv.URL + "/usage"}
	if resets {
		urls[quotaResets] = srv.URL + "/resets"
	}
	m.quotas.urls = map[credproxy.Provider]map[string]string{p: urls}
}

func controlQuota(t *testing.T, m *Manager) AccountStatus {
	t.Helper()
	for _, s := range m.StatusWithQuota(t.Context()) {
		if s.Name == "control" {
			return s
		}
	}
	t.Fatal("no control account")

	return AccountStatus{}
}

func sameWindows(t *testing.T, got, want []QuotaWindow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("windows: %+v want %+v", got, want)
	}
	for i := range got {
		if got[i].Bucket != want[i].Bucket || got[i].Name != want[i].Name || got[i].UsedPercent != want[i].UsedPercent ||
			!got[i].ResetsAt.Equal(want[i].ResetsAt) || got[i].LimitReached != want[i].LimitReached {
			t.Fatalf("window %d: %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestClaudeQuotaReportsWindowsExtraUsageAndEmail(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	m := testManager(t, credproxy.Claude)
	srv, calls := quotaServer(t, map[string]quotaReply{"/usage": {200, `{
		"five_hour":{"utilization":12.5,"resets_at":"2026-11-08T09:00:00Z"},
		"seven_day":{"utilization":40,"resets_at":"2026-11-08T09:00:00Z"},
		"seven_day_opus":null,
		"seven_day_sonnet":{"utilization":7,"resets_at":"2026-11-08T09:00:00Z"},
		"seven_day_cowork":{"utilization":null},
		"extra_usage":{"is_enabled":true,"monthly_limit":50,"used_credits":12.5,"utilization":25}}`}})
	withQuotaServer(m, credproxy.Claude, srv, false)
	s := controlQuota(t, m)
	if s.Email != "synthetic@example.invalid" || s.Quota == nil || s.Quota.Error != "" || s.Quota.Resets != nil {
		t.Fatalf("status: %+v quota %+v", s, s.Quota)
	}
	sameWindows(t, s.Quota.Windows, []QuotaWindow{
		{Name: "5h", UsedPercent: 12.5, ResetsAt: reset},
		{Name: "7d", UsedPercent: 40, ResetsAt: reset},
		{Bucket: "Sonnet", Name: "7d", UsedPercent: 7, ResetsAt: reset},
	})
	e := s.Quota.ExtraUsage
	if e == nil || !e.Enabled || *e.MonthlyLimit != 50 || *e.UsedCredits != 12.5 || *e.UsedPercent != 25 {
		t.Fatalf("extra usage: %+v", e)
	}
	first := calls.Load()
	if first != 3 {
		t.Fatalf("expected one fetch per ready account, got %d", first)
	}
	m.StatusWithQuota(t.Context())
	if calls.Load() != first {
		t.Fatal("quota not cached")
	}
	if statusOf(t, m, "control").Quota != nil {
		t.Fatal("lease status carries quota")
	}
}

func TestCodexQuotaReportsBucketsCreditsAndResets(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	m := testManager(t, credproxy.Codex)
	srv, _ := quotaServer(t, map[string]quotaReply{
		"/usage": {200, `{"plan_type":"pro",
			"rate_limit":{"allowed":true,"limit_reached":false,
				"primary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1794128400},
				"secondary_window":{"used_percent":21,"limit_window_seconds":604800,"reset_at":1794128400}},
			"code_review_rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1794128400},"secondary_window":null},
			"additional_rate_limits":[{"metered_feature":"codex_bengalfox","limit_name":"","rate_limit":{"limit_reached":false,"windows":[{"key":"weekly","used_percent":9,"limit_window_seconds":604800,"reset_at":1794128400}]}}],
			"credits":{"has_credits":true,"unlimited":false,"balance":"12.50","approx_local_messages":[10,40],"approx_cloud_messages":[5,20]}}`},
		"/resets": {200, `{"immediate_reset_purchase_eligible":true,"available_count":2,"credits":[{"id":"a","status":"available"},{"id":"b","status":"available"},{"id":"c","status":"used"}]}`},
	})
	withQuotaServer(m, credproxy.Codex, srv, true)
	q := controlQuota(t, m).Quota
	if q == nil || q.Error != "" || q.LimitReached == nil || *q.LimitReached {
		t.Fatalf("quota: %+v", q)
	}
	sameWindows(t, q.Windows, []QuotaWindow{
		{Name: "5h", UsedPercent: 3, ResetsAt: reset},
		{Name: "7d", UsedPercent: 21, ResetsAt: reset},
		{Bucket: "Code review", Name: "7d", UsedPercent: 100, ResetsAt: reset, LimitReached: true},
		{Bucket: "GPT-5.3-Codex-Spark", Name: "7d", UsedPercent: 9, ResetsAt: reset},
	})
	c := q.Credits
	if c == nil || !c.HasCredits || c.Unlimited || c.Balance != "12.50" || len(c.ApproxLocalMessages) != 2 || c.ApproxCloudMessages[1] != 20 {
		t.Fatalf("credits: %+v", c)
	}
	r := q.Resets
	if r == nil || r.Error != "" || !*r.Eligible || *r.Available != 2 || r.Statuses["available"] != 2 || r.Statuses["used"] != 1 {
		t.Fatalf("resets: %+v", r)
	}
}

func TestCodexResetFailureKeepsUsage(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	srv, _ := quotaServer(t, map[string]quotaReply{
		"/usage":  {200, `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":18000}}}`},
		"/resets": {404, `{"detail":"secret-detail"}`},
	})
	withQuotaServer(m, credproxy.Codex, srv, true)
	q := controlQuota(t, m).Quota
	if q.Error != "" || len(q.Windows) != 1 || q.Resets == nil || q.Resets.Error != "provider reset credits endpoint returned HTTP 404" {
		t.Fatalf("quota: %+v resets %+v", q, q.Resets)
	}
}

func TestQuotaErrorNeverCarriesResponseBody(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	srv, _ := quotaServer(t, map[string]quotaReply{"/usage": {403, `{"error":"secret-detail-from-provider"}`}})
	withQuotaServer(m, credproxy.Claude, srv, false)
	for _, s := range m.StatusWithQuota(t.Context()) {
		if s.Quota == nil || s.Quota.Error != "provider usage endpoint returned HTTP 403" || len(s.Quota.Windows) != 0 {
			t.Fatalf("unexpected quota: %+v", s.Quota)
		}
		if strings.Contains(s.Quota.Error, "secret") {
			t.Fatal("error leaked provider body")
		}
	}
}

func TestQuotaSkipsAccountsThatAreNotReady(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	srv, calls := quotaServer(t, map[string]quotaReply{"/usage": {200, `{}`}})
	withQuotaServer(m, credproxy.Codex, srv, false)
	a, _ := m.account("worker1")
	writeFixture(t, a, "worker1", time.Now().Add(-time.Hour))
	for _, s := range m.StatusWithQuota(t.Context()) {
		if s.Name == "worker1" && s.Quota != nil {
			t.Fatalf("expired account fetched quota: %+v", s)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 fetches, got %d", calls.Load())
	}
}

func TestWindowName(t *testing.T) {
	t.Parallel()
	for seconds, want := range map[int64]string{18000: "5h", 604800: "7d", 2592000: "30d", 5400: "90m", 0: "window"} {
		if got := windowName(seconds); got != want {
			t.Fatalf("windowName(%d)=%s want %s", seconds, got, want)
		}
	}
}
