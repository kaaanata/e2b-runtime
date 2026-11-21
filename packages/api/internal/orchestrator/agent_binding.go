package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
)

var (
	agentFingerprint        = regexp.MustCompile("^[0-9a-f]{64}$")
	errAgentBindingConflict = errors.New("sandbox agent binding conflict")
)

const agentBindingPrefix = "sbx-proxy-v1-"

// BindSandboxAgent writes an initially absent identity or compares and replaces
// it in an explicit control-account switch. The update uses
// the same sandbox-store lock as lifecycle transitions. Pausing a sandbox
// persists these metadata with the snapshot before the execution stops.
func (o *Orchestrator) BindSandboxAgent(ctx context.Context, teamID uuid.UUID, sandboxID string, req api.SandboxAgentBindingUpdate) (sandbox.Sandbox, *api.APIError) {
	provider := string(req.Provider)
	if (provider != "claude" && provider != "codex") || req.StartedAt.IsZero() || !agentFingerprint.MatchString(req.Fingerprint) ||
		req.PreviousFingerprint != nil && !agentFingerprint.MatchString(*req.PreviousFingerprint) {
		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusBadRequest, ClientMsg: "Invalid agent binding request"}
	}

	updated, err := o.sandboxStore.Update(ctx, teamID, sandboxID, func(current sandbox.Sandbox) (sandbox.Sandbox, error) {
		return withSandboxAgentBinding(current, req)
	})
	switch {
	case err == nil:
		return updated, nil
	case errors.Is(err, sandbox.ErrNotFound):
		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusNotFound, ClientMsg: "Sandbox not found", Err: err}
	case errors.Is(err, errAgentBindingConflict):
		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusConflict, ClientMsg: "The running project or its control-account bindings changed", Err: err}
	default:
		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Could not update the agent binding", Err: err}
	}
}

func withSandboxAgentBinding(current sandbox.Sandbox, req api.SandboxAgentBindingUpdate) (sandbox.Sandbox, error) {
	if current.State != sandbox.StateRunning || !current.StartTime.Equal(req.StartedAt) ||
		current.Metadata["managed-by"] != "sbx" || current.Metadata["project"] == "" {
		return current, fmt.Errorf("%w: project execution is not running", errAgentBindingConflict)
	}
	provider := string(req.Provider)
	key := agentBindingPrefix + provider
	old := current.Metadata[key]
	if req.PreviousFingerprint != nil && old == "" ||
		old != "" && old != req.Fingerprint && (req.PreviousFingerprint == nil || old != *req.PreviousFingerprint) {
		return current, fmt.Errorf("%w: provider already has a different identity", errAgentBindingConflict)
	}

	peer := "claude"
	if provider == peer {
		peer = "codex"
	}
	expected := req.ExistingBindings[peer]
	actual := current.Metadata[agentBindingPrefix+peer]
	if expected != actual || old == "" && !agentFingerprint.MatchString(actual) {
		return current, fmt.Errorf("%w: existing control identity is unverified", errAgentBindingConflict)
	}
	if old != req.Fingerprint {
		current.Metadata = maps.Clone(current.Metadata)
		current.Metadata[key] = req.Fingerprint
	}
	if req.PreviousFingerprint != nil && current.Metadata["sbx-control-v1-"+provider] != "scoped" {
		if old == req.Fingerprint {
			current.Metadata = maps.Clone(current.Metadata)
		}
		current.Metadata["sbx-control-v1-"+provider] = "scoped"
	}
	return current, nil
}
