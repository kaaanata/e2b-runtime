package orchestrator

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
)

func TestGetMaxTTLNormal(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now, 2*time.Hour, 3*time.Hour)
	if ttl != 2*time.Hour {
		t.Fatalf("expected 2 hours, got %v", ttl)
	}
}

func TestGetMaxTTLMax(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now, 4*time.Hour, 3*time.Hour)
	if ttl != 3*time.Hour {
		t.Fatalf("expected 3 hours, got %v", ttl)
	}
}

func TestGetMaxTTLExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now.Add(-2*time.Hour), 4*time.Hour, time.Hour)
	if ttl != 0 {
		t.Fatalf("expected 0 hours, got %v", ttl)
	}
}

func TestSBXWithoutDeadlineIsOptInAndProjectScoped(t *testing.T) {
	td := ldtestdata.DataSource()
	client, err := featureflags.NewClientWithDatasource(td)
	require.NoError(t, err)
	o := &Orchestrator{featureFlagsClient: client}
	team := uuid.New()
	sbx := map[string]string{"managed-by": "sbx", "project": "workspace"}
	require.False(t, o.sbxWithoutDeadline(t.Context(), team, "test", sbx))

	td.Update(td.Flag(featureflags.SBXProjectsWithoutDeadline.Key()).VariationForAll(true))
	require.True(t, o.sbxWithoutDeadline(t.Context(), team, "test", sbx))
	require.False(t, o.sbxWithoutDeadline(t.Context(), team, "test", nil))
	require.False(t, o.sbxWithoutDeadline(t.Context(), team, "test", map[string]string{"managed-by": "sdk"}))
}
