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

const envBuildMaxRamMBVersion int64 = 20261018160000

// The memory hotplug columns are forward-only additions to env_builds: a
// database from before them upgrades without touching existing rows (which
// read as "not hot-resizable"), and Down removes exactly the added columns.
func TestMemoryHotplugMigrations(t *testing.T) {
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

	// Fresh database: the column exists, a nullable bigint.
	exists, dataType, nullable := column("max_ram_mb")
	require.True(t, exists)
	assert.Equal(t, "bigint", dataType)
	assert.Equal(t, "YES", nullable)

	// A build from before the migration.
	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "uploaded")

	_, err = migrations.DownTo(ctx, envBuildMaxRamMBVersion-1)
	require.NoError(t, err)
	exists, _, _ = column("max_ram_mb")
	require.False(t, exists, "Down must remove the column")

	results, err := migrations.UpTo(ctx, envBuildMaxRamMBVersion)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, envBuildMaxRamMBVersion, results[0].Source.Version)

	var maxRamMB *int64
	var ramMB int64
	require.NoError(t, sqlDB.QueryRowContext(ctx,
		`SELECT ram_mb, max_ram_mb FROM public.env_builds WHERE id = $1`, buildID).Scan(&ramMB, &maxRamMB))
	assert.Nil(t, maxRamMB, "an existing build reads as fixed-size")
	assert.Positive(t, ramMB, "existing data is untouched")

	// Back to the newest version for the parallel suite's shared assumptions.
	_, err = migrations.Up(ctx)
	require.NoError(t, err)
}
