package credentialowner

import (
	"strings"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

func TestScopedControlKeepsOtherSandboxesAndInferencePinned(t *testing.T) {
	t.Parallel()
	for _, provider := range []credproxy.Provider{credproxy.Claude, credproxy.Codex} {
		t.Run(string(provider), func(t *testing.T) {
			m := testManager(t, provider)
			a, b := strings.Repeat("a", 20), strings.Repeat("b", 20)
			req := credproxy.ResolveRequest{Provider: provider, Purpose: credproxy.Control, SandboxID: a}
			inference := pick(t, m, provider, "existing-inference")
			if err := m.setSandboxControl(t.Context(), provider, a, "control", "worker2"); err != nil {
				t.Fatal(err)
			}
			if err := m.setSandboxControl(t.Context(), provider, a, "control", "worker1"); err == nil {
				t.Fatal("accepted an outdated control binding")
			}
			reopened, err := Open(m.config)
			if err != nil {
				t.Fatal(err)
			}
			for scope, want := range map[string]string{a: "worker2", b: "control", "": "control"} {
				req.SandboxID = scope
				lease, err := reopened.Resolve(t.Context(), req)
				if err != nil || lease.Account != want {
					t.Fatalf("scope %q resolved to %q, want %q: %v", scope, lease.Account, want, err)
				}
			}
			if got := pick(t, reopened, provider, "existing-inference"); got != inference {
				t.Fatalf("inference moved from %q to %q", inference, got)
			}
		})
	}
}
