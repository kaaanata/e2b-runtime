-- +goose Up
-- +goose StatementBegin

-- The device layout a template build was made with, as the template-manager
-- reports it when the build finishes (after inheritance from a base
-- template): '' for rootfs only, 'block-v1' with one workspace slot capacity
-- per slot. NULL means not reported (builds from before layouts were
-- recorded, or snapshot builds); readers treat that as unknown.
ALTER TABLE "public"."env_builds"
    ADD COLUMN layout                        TEXT,
    ADD COLUMN workspace_slot_capacity_bytes BIGINT[];

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE "public"."env_builds"
    DROP COLUMN IF EXISTS workspace_slot_capacity_bytes,
    DROP COLUMN IF EXISTS layout;

-- +goose StatementEnd
