package tests

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
)

const envBuildMaxVCPUVersion int64 = 20261019100000

// The CPU hot resize columns are forward-only additions to env_builds like the
// memory hotplug ones: existing rows read as fixed vCPU count, Down removes
// exactly the added columns.
func TestCPUHotplugMigrations(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	ctx := t.Context()
	sqlDB, err := sql.Open("pgx", db.ConnStr())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	store, err := database.NewStore(goose.DialectPostgres, testutils.TrackingTable)
	require.NoError(t, err)
	migrations, err := goose.NewProvider("", sqlDB, os.DirFS(filepath.Join("..", "..", "migrations")), goose.WithStore(store))
	require.NoError(t, err)

	column := func(name string) (exists bool, dataType, nullable string) {
		t.Helper()

		err := sqlDB.QueryRowContext(ctx, `
			SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'env_builds' AND column_name = $1
		`, name).Scan(&dataType, &nullable)
		if err == sql.ErrNoRows {
			return false, "", ""
		}
		require.NoError(t, err)

		return true, dataType, nullable
	}

	for _, name := range []string{"max_vcpu", "requested_vcpu"} {
		exists, dataType, nullable := column(name)
		require.True(t, exists, name)
		assert.Equal(t, "bigint", dataType, name)
		assert.Equal(t, "YES", nullable, name)
	}

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "uploaded")

	_, err = migrations.DownTo(ctx, envBuildMaxVCPUVersion-1)
	require.NoError(t, err)
	for _, name := range []string{"max_vcpu", "requested_vcpu"} {
		exists, _, _ := column(name)
		require.False(t, exists, "Down must remove %s", name)
	}

	_, err = migrations.Up(ctx)
	require.NoError(t, err)

	var maxVCPU, requestedVCPU *int64
	var vcpu int64
	require.NoError(t, sqlDB.QueryRowContext(ctx,
		`SELECT vcpu, max_vcpu, requested_vcpu FROM public.env_builds WHERE id = $1`, buildID).Scan(&vcpu, &maxVCPU, &requestedVCPU))
	assert.Nil(t, maxVCPU, "an existing build reads as a fixed vCPU count")
	assert.Nil(t, requestedVCPU)
	assert.Positive(t, vcpu, "existing data is untouched")
}
