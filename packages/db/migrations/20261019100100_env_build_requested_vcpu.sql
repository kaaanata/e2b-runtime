-- +goose Up
-- +goose StatementBegin

-- The vCPU count last granted to the sandbox a snapshot build row belongs to
-- (CPU hot resize). Written at pause next to max_vcpu (inherited from the
-- template build), read as the default count when the sandbox resumes. NULL
-- means the snapshot was taken before this column or by a sandbox without CPU
-- hot resize; readers then resume with vcpu.
ALTER TABLE "public"."env_builds"
    ADD COLUMN requested_vcpu BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE "public"."env_builds"
    DROP COLUMN IF EXISTS requested_vcpu;

-- +goose StatementEnd
