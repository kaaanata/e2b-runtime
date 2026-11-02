package credentialowner

import (
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

func statusOf(t *testing.T, m *Manager, name string) AccountStatus {
	t.Helper()
	for _, s := range m.Status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s", name)

	return AccountStatus{}
}

func TestStatusReportsPlanSessionsAndCooldown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider credproxy.Provider
		plan     string
	}{{credproxy.Claude, "max"}, {credproxy.Codex, "pro"}} {
		m := testManager(t, tc.provider)
		worker := pick(t, m, tc.provider, "first")
		pick(t, m, tc.provider, "first")
		s := statusOf(t, m, worker)
		if s.Plan != tc.plan || s.Sessions != 1 || !s.CoolingUntil.IsZero() || !s.LastLimitedAt.IsZero() {
			t.Fatalf("%s before limit: %+v", tc.provider, s)
		}
		before := time.Now()
		err := m.Observe(t.Context(), credproxy.Outcome{Provider: tc.provider, Purpose: credproxy.Inference, Session: session("first"), Account: worker, Status: 429, RetryAfter: 120})
		if err != nil {
			t.Fatal(err)
		}
		s = statusOf(t, m, worker)
		if s.CoolingUntil.Before(before.Add(119*time.Second)) || s.LastLimitedAt.Before(before) {
			t.Fatalf("%s cooldown not reported: %+v", tc.provider, s)
		}
		if c := statusOf(t, m, "control"); c.Sessions != 0 || !c.CoolingUntil.IsZero() {
			t.Fatalf("%s control picked up usage: %+v", tc.provider, c)
		}
	}
}
