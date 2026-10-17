package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
)

func TestTemplateBuildLayoutToAPI(t *testing.T) {
	t.Parallel()

	layout, slots := templateBuildLayoutToAPI(nil, nil)
	assert.Nil(t, layout, "an unrecorded layout is not reported")
	assert.Nil(t, slots)

	layout, slots = templateBuildLayoutToAPI(new(""), nil)
	require.NotNil(t, layout)
	assert.Equal(t, api.TemplateBuildLayoutRootfs, *layout)
	assert.Nil(t, slots)

	layout, slots = templateBuildLayoutToAPI(new(templateLayoutBlockV1), []int64{10 << 30, 20 << 30})
	require.NotNil(t, layout)
	assert.Equal(t, api.TemplateBuildLayoutBlockV1, *layout)
	require.NotNil(t, slots)
	assert.Equal(t, []int64{10, 20}, *slots)
}

func TestCheckBlockMountsFitTemplate(t *testing.T) {
	t.Parallel()

	vol := func(name string, gib int64) queries.Volume {
		return queries.Volume{Name: name, CapacityBytes: new(gib << 30)}
	}
	plans := func(vols ...queries.Volume) []blockMountPlan {
		out := make([]blockMountPlan, 0, len(vols))
		for i, v := range vols {
			out = append(out, blockMountPlan{volume: v, binding: bindHead, slot: i})
		}

		return out
	}
	block := func(slotsGiB ...int64) *queries.EnvBuild {
		slots := make([]int64, 0, len(slotsGiB))
		for _, g := range slotsGiB {
			slots = append(slots, g<<30)
		}

		return &queries.EnvBuild{Layout: new(templateLayoutBlockV1), WorkspaceSlotCapacityBytes: slots}
	}
	refused := func(t *testing.T, apiErr *api.APIError, contains string) {
		t.Helper()

		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
		assert.Equal(t, errCodeTemplateLayoutMismatch, apiErr.ErrorCode)
		assert.Contains(t, apiErr.ClientMsg, contains)
	}

	assert.Nil(t, checkBlockMountsFitTemplate(&queries.EnvBuild{}, plans(vol("a", 10))), "an unrecorded layout is left to the node")
	assert.Nil(t, checkBlockMountsFitTemplate(&queries.EnvBuild{Layout: new("")}, nil), "no block mounts, nothing to check")

	refused(t, checkBlockMountsFitTemplate(&queries.EnvBuild{Layout: new("")}, plans(vol("a", 10))), "no workspace slots")
	refused(t, checkBlockMountsFitTemplate(block(10), plans(vol("a", 10), vol("b", 10))), "reserves 1 workspace slot")
	refused(t, checkBlockMountsFitTemplate(block(20), plans(vol("a", 10))), "smaller than the template's workspace slot 0")

	assert.Nil(t, checkBlockMountsFitTemplate(block(10, 10), plans(vol("a", 10))), "a volume of the slot's capacity fits")
	assert.Nil(t, checkBlockMountsFitTemplate(block(10, 10), plans(vol("a", 10), vol("b", 20))), "a larger volume fits; the node grows the device")
}

func TestGetTemplate_ReportsBuildLayout(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	f := newBlockAPIFixture(t, db)
	ctx := t.Context()

	templateID := testutils.CreateTestTemplate(t, f.db, f.team.ID)
	buildID := testutils.CreateTestBuild(t, ctx, f.db, templateID, "building")
	testutils.CreateTestBuildAssignment(t, ctx, f.db, templateID, buildID, "default")
	require.NoError(t, f.db.SqlcClient.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
		BuildID:                    buildID,
		Status:                     types.BuildStatusUploaded,
		Layout:                     new(templateLayoutBlockV1),
		WorkspaceSlotCapacityBytes: []int64{10 << 30, 10 << 30},
	}))

	c, rec := f.request(t, http.MethodGet, "/templates/"+templateID, nil)
	f.store.GetTemplatesTemplateID(c, templateID, api.GetTemplatesTemplateIDParams{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got api.TemplateWithBuilds
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Builds, 1)
	require.NotNil(t, got.Builds[0].Layout)
	assert.Equal(t, api.TemplateBuildLayoutBlockV1, *got.Builds[0].Layout)
	require.NotNil(t, got.Builds[0].WorkspaceSlotCapacityGiB)
	assert.Equal(t, []int64{10, 10}, *got.Builds[0].WorkspaceSlotCapacityGiB)
}
