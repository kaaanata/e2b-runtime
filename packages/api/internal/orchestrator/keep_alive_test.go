package orchestrator

import (
	"testing"

	"github.com/google/uuid"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
)

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
