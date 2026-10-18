-- +goose Up
-- +goose StatementBegin

-- The largest vCPU count a sandbox of the build can be resized to while it
-- runs (CPU hot resize: the VM has this many vCPUs, the guest boots vcpu of
-- them online). vcpu stays the template's count. Set at registration from the
-- build request, then overwritten with the value the template-manager
-- reports when the build finishes (after inheritance from a base template).
-- NULL means a fixed vCPU count or not recorded; readers treat both as "not
-- hot-resizable".
ALTER TABLE "public"."env_builds"
    ADD COLUMN max_vcpu BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE "public"."env_builds"
    DROP COLUMN IF EXISTS max_vcpu;

-- +goose StatementEnd
