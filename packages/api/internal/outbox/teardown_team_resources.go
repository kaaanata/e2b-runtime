package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	authdb "github.com/e2b-dev/infra/packages/db/pkg/auth"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/clusters"
	"github.com/e2b-dev/infra/packages/shared/pkg/sharedriver"
)

const teardownAttemptTimeout = 30 * time.Minute

var (
	errTeamNotBlocked     = errors.New("team is not blocked")
	errSandboxesLeft      = errors.New("team still has sandboxes in the store")
	errBlockVolumesQueued = errors.New("team still has block volumes being created")
)

type teamSandboxes interface {
	KillTeamSandboxes(ctx context.Context, teamID uuid.UUID, reason sandbox.KillReason) (killed, failed int, err error)
	GetSandboxes(ctx context.Context, teamID uuid.UUID, states []sandbox.State) ([]sandbox.Sandbox, error)
}

// teamResourcesTeardownWorker stops a deleted team's workloads by killing its
// sandboxes. A sandbox that is no longer running cannot be paused, so no
// snapshot of the team appears once this job has finished. Every attempt is
// safe to run again.
type teamResourcesTeardownWorker struct {
	river.WorkerDefaults[outbox.TeardownTeamResources]

	sandboxes teamSandboxes
	teams     *authdb.Client
	// volumes deletes the team's block volumes; nil when the API runs no
	// volume jobs (the step is then skipped).
	volumes *VolumeExecutor
	steps   sharedriver.JobSteps
}

func (w *teamResourcesTeardownWorker) Timeout(*river.Job[outbox.TeardownTeamResources]) time.Duration {
	return teardownAttemptTimeout
}

func (w *teamResourcesTeardownWorker) Work(ctx context.Context, job *river.Job[outbox.TeardownTeamResources]) error {
	teamID := job.Args.TeamID

	// The guard runs on every attempt: a team that is no longer blocked must
	// not lose its workloads on a later retry either.
	var clusterID *uuid.UUID
	if err := w.steps.Step(ctx, "check_team_blocked", func(ctx context.Context) error {
		var err error
		clusterID, err = w.checkTeamBlocked(ctx, teamID)

		return err
	}); err != nil {
		return err
	}

	if err := w.steps.Step(ctx, "kill_sandboxes", func(ctx context.Context) error {
		return w.killSandboxes(ctx, teamID)
	}); err != nil {
		return err
	}

	if w.volumes == nil {
		return nil
	}

	// After the kills: no execution of the team holds a volume any more, so
	// nothing can attach one between here and its delete.
	return w.steps.Step(ctx, "delete_block_volumes", func(ctx context.Context) error {
		return w.volumes.DeleteTeamBlockVolumes(ctx, teamID, clusters.WithClusterFallback(clusterID))
	})
}

// checkTeamBlocked refuses a team that exists and is not blocked; the job
// retries until it is blocked or gone. A missing team does not stop the
// teardown. It returns the team's cluster (nil when unknown or local).
func (w *teamResourcesTeardownWorker) checkTeamBlocked(ctx context.Context, teamID uuid.UUID) (*uuid.UUID, error) {
	team, err := w.teams.GetTeamWithTierByTeamID(ctx, teamID)
	if dberrors.IsNotFoundError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read team: %w", err)
	}
	if !team.Team.IsBlocked {
		return nil, errTeamNotBlocked
	}

	return team.Team.ClusterID, nil
}

// killSandboxes succeeds only once the store holds no sandbox of the team in
// any state: one still pausing or being killed fails the step, so it runs
// again.
func (w *teamResourcesTeardownWorker) killSandboxes(ctx context.Context, teamID uuid.UUID) error {
	killed, failed, err := w.sandboxes.KillTeamSandboxes(ctx, teamID, sandbox.KillReasonTeamDeleted)
	if err != nil {
		return err
	}

	remaining, err := w.sandboxes.GetSandboxes(ctx, teamID, nil)
	if err != nil {
		return fmt.Errorf("list team sandboxes: %w", err)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("%w: %d left (killed %d, failed %d)", errSandboxesLeft, len(remaining), killed, failed)
	}

	return nil
}
