-- +goose Up
-- +goose StatementBegin

-- The guest memory last requested for the sandbox a snapshot build row
-- belongs to (virtio-mem hotplug): boot memory ram_mb plus the hotplugged
-- part. Written at pause next to max_ram_mb (inherited from the template
-- build), read as the default size when the sandbox resumes. NULL means the
-- snapshot was taken before this column or by a sandbox without hotplug;
-- readers then resume with ram_mb.
ALTER TABLE "public"."env_builds"
    ADD COLUMN requested_ram_mb BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE "public"."env_builds"
    DROP COLUMN IF EXISTS requested_ram_mb;

-- +goose StatementEnd
