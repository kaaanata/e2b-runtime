package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
)

func TestSandboxAgentBindingSurvivesSnapshot(t *testing.T) {
	t.Parallel()
	known, added := strings.Repeat("a", 64), strings.Repeat("b", 64)
	started := time.Now()
	box := sandbox.Sandbox{
		SandboxID: "box", State: sandbox.StateRunning, StartTime: started,
		Metadata: map[string]string{"managed-by": "sbx", "project": "citadel", "sbx-proxy-v1-codex": known},
	}
	request := api.SandboxAgentBindingUpdate{
		Provider: "claude", Fingerprint: added, StartedAt: started,
		ExistingBindings: map[string]string{"codex": known},
	}
	result, err := withSandboxAgentBinding(box, request)
	require.NoError(t, err)
	require.Equal(t, added, result.Metadata["sbx-proxy-v1-claude"])
	require.Equal(t, known, result.Metadata["sbx-proxy-v1-codex"])
	require.NotContains(t, box.Metadata, "sbx-proxy-v1-claude")
	require.Equal(t, box.State, result.State)
	require.Equal(t, added, buildUpsertSnapshotParams(result, &nodemanager.Node{ID: "test-node"}, false).Metadata["sbx-proxy-v1-claude"])
	retry, err := withSandboxAgentBinding(result, request)
	require.NoError(t, err)
	require.Equal(t, result.Metadata, retry.Metadata)

	for name, refused := range map[string]sandbox.Sandbox{
		"transitioning": {State: sandbox.StatePausing, StartTime: started, Metadata: box.Metadata},
		"new execution": {State: sandbox.StateRunning, StartTime: started.Add(time.Second), Metadata: box.Metadata},
		"unknown owner": {State: sandbox.StateRunning, StartTime: started, Metadata: map[string]string{"managed-by": "sbx", "project": "citadel"}},
		"other identity": {State: sandbox.StateRunning, StartTime: started, Metadata: map[string]string{
			"managed-by": "sbx", "project": "citadel", "sbx-proxy-v1-codex": known, "sbx-proxy-v1-claude": known,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := withSandboxAgentBinding(refused, request)
			require.ErrorIs(t, err, errAgentBindingConflict)
		})
	}
}
