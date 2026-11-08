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

func quotaServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("usage request without bearer token")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

func withQuotaServer(m *Manager, p credproxy.Provider, srv *httptest.Server) {
	m.quotas.client = srv.Client()
	m.quotas.urls = map[credproxy.Provider]string{p: srv.URL}
}

func TestStatusWithQuotaReportsProviderWindowsAndEmail(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		provider credproxy.Provider
		body     string
		want     []QuotaWindow
	}{
		{credproxy.Claude, `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-08T09:00:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-11-08T09:00:00Z"},"seven_day_opus":null,"extra_usage":{"is_enabled":false}}`,
			[]QuotaWindow{{Name: "5h", UsedPercent: 12.5, ResetsAt: reset}, {Name: "7d", UsedPercent: 40, ResetsAt: reset}}},
		{credproxy.Codex, `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1794128400},"secondary_window":{"used_percent":21,"limit_window_seconds":604800,"reset_at":1794128400}}}`,
			[]QuotaWindow{{Name: "5h", UsedPercent: 3, ResetsAt: reset}, {Name: "7d", UsedPercent: 21, ResetsAt: reset}}},
	} {
		m := testManager(t, tc.provider)
		srv, calls := quotaServer(t, 200, tc.body)
		withQuotaServer(m, tc.provider, srv)
		var control AccountStatus
		for _, s := range m.StatusWithQuota(t.Context()) {
			if s.Name == "control" {
				control = s
			}
		}
		if control.Email != "synthetic@example.invalid" || control.Quota == nil || control.Quota.Error != "" {
			t.Fatalf("%s status: %+v", tc.provider, control)
		}
		got := control.Quota.Windows
		if len(got) != len(tc.want) {
			t.Fatalf("%s windows: %+v", tc.provider, got)
		}
		for i := range got {
			if got[i].Name != tc.want[i].Name || got[i].UsedPercent != tc.want[i].UsedPercent || !got[i].ResetsAt.Equal(tc.want[i].ResetsAt) {
				t.Fatalf("%s window %d: %+v want %+v", tc.provider, i, got[i], tc.want[i])
			}
		}
		first := calls.Load()
		if first != 3 {
			t.Fatalf("%s expected one fetch per ready account, got %d", tc.provider, first)
		}
		m.StatusWithQuota(t.Context())
		if calls.Load() != first {
			t.Fatalf("%s quota not cached", tc.provider)
		}
		if s := statusOf(t, m, "control"); s.Quota != nil {
			t.Fatalf("%s lease status carries quota: %+v", tc.provider, s)
		}
	}
}

func TestQuotaErrorNeverCarriesResponseBody(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	srv, _ := quotaServer(t, 403, `{"error":"secret-detail-from-provider"}`)
	withQuotaServer(m, credproxy.Claude, srv)
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
	srv, calls := quotaServer(t, 200, `{}`)
	withQuotaServer(m, credproxy.Codex, srv)
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
