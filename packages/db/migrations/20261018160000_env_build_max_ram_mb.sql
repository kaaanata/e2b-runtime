-- +goose Up
-- +goose StatementBegin

-- The largest guest memory a sandbox of the build can be resized to while it
-- runs (virtio-mem hotplug). ram_mb stays the boot memory, the always-present
-- minimum. Set at registration from the build request, then overwritten with
-- the value the template-manager reports when the build finishes (after
-- inheritance from a base template). NULL means a fixed-size build (no
-- hotplug device) or not recorded; readers treat both as "not hot-resizable".
ALTER TABLE "public"."env_builds"
    ADD COLUMN max_ram_mb BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE "public"."env_builds"
    DROP COLUMN IF EXISTS max_ram_mb;

-- +goose StatementEnd
