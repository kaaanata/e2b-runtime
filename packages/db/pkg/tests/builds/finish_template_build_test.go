package builds

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
)

// seededKernel / seededFC match the defaults baked into testutils.CreateTestBuild.
const (
	seededKernel = "6.1.0"
	seededFC     = "1.4.0"
)

// getBuildVersions returns the kernel_version, firecracker_version and envd_version
// currently stored on the env_builds row. envd_version is nullable, so it is
// returned as *string.
func getBuildVersions(t *testing.T, ctx context.Context, db *testutils.Database, buildID uuid.UUID) (kernel, firecracker string, envd *string) {
	t.Helper()

	err := db.SqlcClient.TestsRawSQLQuery(ctx,
		"SELECT kernel_version, firecracker_version, envd_version FROM public.env_builds WHERE id = $1",
		func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}

			return rows.Scan(&kernel, &firecracker, &envd)
		},
		buildID,
	)
	require.NoError(t, err, "Failed to query build versions")

	return kernel, firecracker, envd
}

func TestFinishTemplateBuild_OverwritesVersionsWhenProvided(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "building")

	// Sanity check the seeded row matches the fixture constants.
	kernel, firecracker, _ := getBuildVersions(t, ctx, db, buildID)
	require.Equal(t, seededKernel, kernel)
	require.Equal(t, seededFC, firecracker)

	totalDisk := int64(4096)
	envdVersion := "v9.9.9"
	reportedKernel := "vmlinux-reported"
	reportedFC := "v9.9.9_reported"

	err := db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
		BuildID:            buildID,
		Status:             types.BuildStatusUploaded,
		TotalDiskSizeMb:    &totalDisk,
		EnvdVersion:        &envdVersion,
		KernelVersion:      reportedKernel,
		FirecrackerVersion: reportedFC,
	})
	require.NoError(t, err)

	kernel, firecracker, envd := getBuildVersions(t, ctx, db, buildID)
	assert.Equal(t, reportedKernel, kernel, "kernel_version should be overwritten with the template-manager reported value")
	assert.Equal(t, reportedFC, firecracker, "firecracker_version should be overwritten with the template-manager reported value")
	if assert.NotNil(t, envd) {
		assert.Equal(t, envdVersion, *envd)
	}
}

func TestFinishTemplateBuild_PreservesVersionsWhenEmpty(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "building")

	// Simulate an old template-manager that does not populate kernel / firecracker
	// versions on the response: both params empty, but the rest of the update
	// (status, envd version, disk size) still applies.
	totalDisk := int64(2048)
	envdVersion := "v1.2.3"

	err := db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
		BuildID:            buildID,
		Status:             types.BuildStatusUploaded,
		TotalDiskSizeMb:    &totalDisk,
		EnvdVersion:        &envdVersion,
		KernelVersion:      "",
		FirecrackerVersion: "",
	})
	require.NoError(t, err)

	kernel, firecracker, envd := getBuildVersions(t, ctx, db, buildID)
	assert.Equal(t, seededKernel, kernel, "kernel_version should fall back to the seeded value when the reported value is empty")
	assert.Equal(t, seededFC, firecracker, "firecracker_version should fall back to the seeded value when the reported value is empty")
	if assert.NotNil(t, envd) {
		assert.Equal(t, envdVersion, *envd)
	}
}

func TestFinishTemplateBuild_PreservesSingleVersionWhenOnlyOneEmpty(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	buildID := testutils.CreateTestBuild(t, ctx, db, templateID, "building")

	reportedKernel := "vmlinux-reported"

	err := db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
		BuildID:            buildID,
		Status:             types.BuildStatusUploaded,
		KernelVersion:      reportedKernel,
		FirecrackerVersion: "",
	})
	require.NoError(t, err)

	kernel, firecracker, _ := getBuildVersions(t, ctx, db, buildID)
	assert.Equal(t, reportedKernel, kernel, "kernel_version should be overwritten when a non-empty value is provided")
	assert.Equal(t, seededFC, firecracker, "firecracker_version should fall back to the seeded value when only kernel was reported")
}

// getBuildLayout returns the layout columns of an env_builds row.
func getBuildLayout(t *testing.T, ctx context.Context, db *testutils.Database, buildID uuid.UUID) (layout *string, slots []int64) {
	t.Helper()

	err := db.SqlcClient.TestsRawSQLQuery(ctx,
		"SELECT layout, workspace_slot_capacity_bytes FROM public.env_builds WHERE id = $1",
		func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}

			return rows.Scan(&layout, &slots)
		},
		buildID,
	)
	require.NoError(t, err, "Failed to query build layout")

	return layout, slots
}

func TestFinishTemplateBuild_RecordsReportedLayout(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	finish := func(buildID uuid.UUID, layout *string, slots []int64) {
		t.Helper()

		require.NoError(t, db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
			BuildID:                    buildID,
			Status:                     types.BuildStatusUploaded,
			Layout:                     layout,
			WorkspaceSlotCapacityBytes: slots,
		}))
	}

	block := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	finish(block, new("block-v1"), []int64{10 << 30, 20 << 30})
	layout, slots := getBuildLayout(t, ctx, db, block)
	require.NotNil(t, layout)
	assert.Equal(t, "block-v1", *layout)
	assert.Equal(t, []int64{10 << 30, 20 << 30}, slots)

	rootfs := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	finish(rootfs, new(""), nil)
	layout, slots = getBuildLayout(t, ctx, db, rootfs)
	require.NotNil(t, layout, "a reported rootfs-only layout is recorded")
	assert.Empty(t, *layout)
	assert.Empty(t, slots)

	unknown := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	finish(unknown, nil, []int64{1})
	layout, slots = getBuildLayout(t, ctx, db, unknown)
	assert.Nil(t, layout, "an unreported layout stays unknown")
	assert.Nil(t, slots, "slots are only recorded with a layout")
}

func getBuildMaxRamMB(t *testing.T, ctx context.Context, db *testutils.Database, buildID uuid.UUID) *int64 {
	t.Helper()

	var maxRamMB *int64
	err := db.SqlcClient.TestsRawSQLQuery(ctx,
		"SELECT max_ram_mb FROM public.env_builds WHERE id = $1",
		func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}

			return rows.Scan(&maxRamMB)
		},
		buildID,
	)
	require.NoError(t, err, "Failed to query build max_ram_mb")

	return maxRamMB
}

// max_ram_mb starts as the requested hotplug maximum and is replaced by what
// the template-manager reports, but only when it reports a layout (an older
// template-manager reports neither).
func TestFinishTemplateBuild_RecordsReportedMaxMemory(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	request := func(buildID uuid.UUID, maxRamMB int64) {
		t.Helper()

		require.NoError(t, db.SqlcClient.TestsRawSQL(ctx,
			"UPDATE public.env_builds SET max_ram_mb = $1 WHERE id = $2", maxRamMB, buildID))
	}
	finish := func(buildID uuid.UUID, layout *string, maxRamMB *int64) {
		t.Helper()

		require.NoError(t, db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
			BuildID:  buildID,
			Status:   types.BuildStatusUploaded,
			Layout:   layout,
			MaxRamMb: maxRamMB,
		}))
	}

	hotplug := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(hotplug, 65536)
	finish(hotplug, new(""), new(int64(65536)))
	assert.Equal(t, new(int64(65536)), getBuildMaxRamMB(t, ctx, db, hotplug))

	inheritedFixed := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(inheritedFixed, 65536)
	finish(inheritedFixed, new(""), nil)
	assert.Nil(t, getBuildMaxRamMB(t, ctx, db, inheritedFixed), "a reported fixed-size build replaces the request")

	unknown := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(unknown, 65536)
	finish(unknown, nil, nil)
	assert.Equal(t, new(int64(65536)), getBuildMaxRamMB(t, ctx, db, unknown), "an older template-manager leaves the request")
}

func getBuildMaxVCPU(t *testing.T, ctx context.Context, db *testutils.Database, buildID uuid.UUID) *int64 {
	t.Helper()

	var maxVCPU *int64
	err := db.SqlcClient.TestsRawSQLQuery(ctx,
		"SELECT max_vcpu FROM public.env_builds WHERE id = $1",
		func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}

			return rows.Scan(&maxVCPU)
		},
		buildID,
	)
	require.NoError(t, err, "Failed to query build max_vcpu")

	return maxVCPU
}

// max_vcpu follows max_ram_mb's rule: replaced by the reported value only when
// the template-manager reports a layout.
func TestFinishTemplateBuild_RecordsReportedMaxVCPU(t *testing.T) {
	t.Parallel()
	db := testutils.SetupDatabase(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	request := func(buildID uuid.UUID, maxVCPU int64) {
		t.Helper()

		require.NoError(t, db.SqlcClient.TestsRawSQL(ctx,
			"UPDATE public.env_builds SET max_vcpu = $1 WHERE id = $2", maxVCPU, buildID))
	}
	finish := func(buildID uuid.UUID, layout *string, maxVCPU *int64) {
		t.Helper()

		require.NoError(t, db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
			BuildID: buildID,
			Status:  types.BuildStatusUploaded,
			Layout:  layout,
			MaxVcpu: maxVCPU,
		}))
	}

	hot := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(hot, 32)
	finish(hot, new(""), new(int64(32)))
	assert.Equal(t, new(int64(32)), getBuildMaxVCPU(t, ctx, db, hot))

	fixed := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(fixed, 32)
	finish(fixed, new(""), nil)
	assert.Nil(t, getBuildMaxVCPU(t, ctx, db, fixed), "a reported fixed build replaces the request")

	unknown := testutils.CreateTestBuild(t, ctx, db, templateID, "building")
	request(unknown, 32)
	finish(unknown, nil, nil)
	assert.Equal(t, new(int64(32)), getBuildMaxVCPU(t, ctx, db, unknown), "an older template-manager leaves the request")
}
