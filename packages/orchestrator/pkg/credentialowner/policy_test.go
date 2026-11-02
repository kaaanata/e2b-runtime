package credentialowner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

func pick(t *testing.T, m *Manager, p credproxy.Provider, id string) string {
	t.Helper()
	l, err := m.Resolve(t.Context(), credproxy.ResolveRequest{Provider: p, Purpose: credproxy.Inference, Session: session(id)})
	if err != nil {
		t.Fatal(err)
	}

	return l.Account
}

func TestLegacyInferenceListNormalizesToRotation(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	r, _ := m.route(credproxy.Claude)
	if r.Strategy != Rotate || len(r.Members) != 2 || r.Members[0].Weight != 1 {
		t.Fatalf("legacy route not normalized: %+v", r)
	}
}

func TestOrderStrategyAlwaysPrefersFirstMember(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	if err := m.SetRoute(credproxy.Codex, Route{Control: "control", Strategy: Order, Members: []Member{{Account: "worker2"}, {Account: "worker1"}}}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if got := pick(t, m, credproxy.Codex, "s"+string(rune('a'+i))); got != "worker2" {
			t.Fatalf("order strategy chose %s", got)
		}
	}
}

func TestWeightStrategyDistributesNewSessions(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Codex)
	m.config.MaxSessions = 100
	if err := m.SetRoute(credproxy.Codex, Route{Control: "control", Strategy: Weight, Members: []Member{{Account: "worker1", Weight: 3}, {Account: "worker2", Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := range 8 {
		counts[pick(t, m, credproxy.Codex, "w"+string(rune('a'+i)))]++
	}
	if counts["worker1"] != 6 || counts["worker2"] != 2 {
		t.Fatalf("3:1 weights produced %v", counts)
	}
}

func TestDisabledMemberKeepsBoundSessionsButTakesNoNewOnes(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	bound := pick(t, m, credproxy.Claude, "kept")
	members := []Member{{Account: "worker1"}, {Account: "worker2"}}
	for i := range members {
		members[i].Disabled = members[i].Account == bound
	}
	if err := m.SetRoute(credproxy.Claude, Route{Control: "control", Members: members}); err != nil {
		t.Fatal(err)
	}
	if got := pick(t, m, credproxy.Claude, "kept"); got != bound {
		t.Fatal("disabled member dropped its bound session")
	}
	for i := range 3 {
		if got := pick(t, m, credproxy.Claude, "new"+string(rune('a'+i))); got == bound {
			t.Fatal("disabled member received a new session")
		}
	}
}

func TestRoutePolicyValidationAndPersistence(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	cases := []Route{
		{Control: "worker1", Members: []Member{{Account: "worker1"}}},
		{Control: "control", Strategy: "smart", Members: []Member{{Account: "worker1"}}},
		{Control: "control", Members: []Member{{Account: "worker1", Weight: 101}}},
		{Control: "control", Members: []Member{{Account: "worker1"}, {Account: "worker1"}}},
		{Control: "control", Members: []Member{{Account: "nobody"}}},
	}
	for _, c := range cases {
		if m.SetRoute(credproxy.Claude, c) == nil {
			t.Fatalf("invalid route accepted: %+v", c)
		}
	}
	if err := m.SetRoute(credproxy.Claude, Route{Control: "control", Strategy: Weight, Members: []Member{{Account: "worker2", Weight: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddAccount("worker3", credproxy.Claude); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{Version: 1, Root: m.config.Root, MaxSessions: 10, Accounts: m.config.Accounts[:3], Routes: map[credproxy.Provider]Route{credproxy.Claude: {Control: "control", Inference: []string{"worker1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := reopened.route(credproxy.Claude)
	if r.Strategy != Weight || len(r.Members) != 1 || r.Members[0].Weight != 5 {
		t.Fatalf("persisted policy did not override static routes: %+v", r)
	}
	if _, ok := reopened.account("worker3"); !ok {
		t.Fatal("runtime-added account lost on restart")
	}
}

func TestNewProviderRouteBindsControlAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	m.config.Binaries = map[credproxy.Provider]string{credproxy.Codex: "/usr/bin/codex"}
	if err := m.AddAccount("codex-a", credproxy.Codex); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRoute(credproxy.Codex, Route{Control: "codex-a", Members: []Member{{Account: "codex-a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRoute(credproxy.Codex, Route{Control: "codex-b"}); err == nil {
		t.Fatal("control change admitted")
	}
	if _, err := Open(m.config); err != nil {
		t.Fatal("new route not recorded consistently", err)
	}
}

func TestAccountRemovalRules(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	if m.RemoveAccount("control") == nil || m.RemoveAccount("worker1") == nil {
		t.Fatal("control or pool member removed")
	}
	if m.AddAccount("bad name", credproxy.Claude) == nil || m.AddAccount("worker1", credproxy.Claude) == nil {
		t.Fatal("invalid or duplicate alias accepted")
	}
	if err := m.AddAccount("spare", credproxy.Claude); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveAccount("spare"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddAccount("spare", credproxy.Claude); err != nil {
		t.Fatal("retired alias cannot be reused", err)
	}
}

func TestAdminSocketCannotIssueLeases(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	h := m.AdminHandler()
	for _, path := range []string{"/v1/resolve", "/v1/outcome"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"provider":"claude","purpose":"control"}`)))
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s answered %d on the admin socket", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/policy", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "refresh") || strings.Contains(w.Body.String(), "binary") {
		t.Fatalf("policy view leaked or failed: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/v1/routes/claude", strings.NewReader(`{"control":"control","strategy":"order","members":[{"account":"worker2"}]}`)))
	if w.Code != 200 {
		t.Fatalf("route update failed: %d %s", w.Code, w.Body.String())
	}
}
