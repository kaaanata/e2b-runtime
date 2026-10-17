package handlers

import (
	"fmt"
	"net/http"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/queries"
)

// templateLayoutBlockV1 is env_builds.layout of a build whose base snapshot
// reserves workspace block slots; "" is rootfs only and NULL not recorded.
const templateLayoutBlockV1 = "block-v1"

// templateBuildLayoutToAPI renders a build's recorded layout. Nothing is
// reported for a build that did not record one (or recorded a layout this API
// does not know).
func templateBuildLayoutToAPI(layout *string, slotCapacityBytes []int64) (*api.TemplateBuildLayout, *[]int64) {
	switch {
	case layout == nil:
		return nil, nil
	case *layout == "":
		return new(api.TemplateBuildLayoutRootfs), nil
	case *layout == templateLayoutBlockV1:
		gib := make([]int64, len(slotCapacityBytes))
		for i, b := range slotCapacityBytes {
			gib[i] = b >> 30
		}

		return new(api.TemplateBuildLayoutBlockV1), &gib
	default:
		return nil, nil
	}
}

// checkBlockMountsFitTemplate pre-checks a new sandbox's block mounts against
// the layout the template build recorded, with the rule the node enforces
// when it restores the template snapshot: mount i takes workspace slot i, and
// a volume needs a slot no larger than itself (the slot's placeholder is the
// device size the template snapshot's guest saw; the node grows the device to
// the volume). A build without a recorded layout is left to the node.
func checkBlockMountsFitTemplate(build *queries.EnvBuild, plans []blockMountPlan) *api.APIError {
	if len(plans) == 0 || build == nil || build.Layout == nil {
		return nil
	}

	refuse := func(msg string) *api.APIError {
		return &api.APIError{Code: http.StatusConflict, ErrorCode: errCodeTemplateLayoutMismatch, ClientMsg: msg}
	}

	if *build.Layout != templateLayoutBlockV1 {
		return refuse("The template has no workspace slots for block volumes; rebuild it with layout block-v1 to mount block volumes")
	}

	slots := build.WorkspaceSlotCapacityBytes
	if len(plans) > len(slots) {
		return refuse(fmt.Sprintf("The template reserves %d workspace slot(s); %d block volumes cannot be mounted", len(slots), len(plans)))
	}

	for _, p := range plans {
		if p.slot < 0 || p.slot >= len(slots) || p.volume.CapacityBytes == nil {
			continue
		}

		if *p.volume.CapacityBytes < slots[p.slot] {
			return refuse(fmt.Sprintf("Block volume '%s' (%d GiB) is smaller than the template's workspace slot %d (%d GiB); a slot takes volumes of at least its capacity",
				p.volume.Name, *p.volume.CapacityBytes>>30, p.slot, slots[p.slot]>>30))
		}
	}

	return nil
}
