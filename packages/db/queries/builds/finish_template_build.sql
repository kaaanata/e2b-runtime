-- name: FinishTemplateBuild :exec
-- kernel_version and firecracker_version are overwritten with whatever the
-- template-manager reports back in TemplateBuildMetadata. Old template-managers
-- that do not populate those fields end up passing an empty string; the
-- NULLIF + COALESCE trick leaves the row's existing values untouched in that
-- case so we do not clobber the values the API seeded at build registration.
-- layout and workspace_slot_capacity_bytes are the device layout the build
-- was made with; an unset layout (an older template-manager) leaves both
-- unknown.
-- The build keeps the first terminal outcome recorded: a build failed for
-- running out of time has had its artifacts deleted, so a later success must
-- not publish it.
WITH finished AS (
    UPDATE "public"."env_builds"
    SET
        finished_at = NOW(),
        total_disk_size_mb = @total_disk_size_mb,
        status = @status,
        envd_version = @envd_version,
        kernel_version = COALESCE(NULLIF(@kernel_version::text, ''), kernel_version),
        firecracker_version = COALESCE(NULLIF(@firecracker_version::text, ''), firecracker_version),
        -- NULL when the template-manager does not report a layout.
        layout = COALESCE(sqlc.narg(layout)::text, layout),
        workspace_slot_capacity_bytes = CASE
            WHEN sqlc.narg(layout)::text IS NULL THEN workspace_slot_capacity_bytes
            ELSE @workspace_slot_capacity_bytes::bigint[]
        END
    WHERE
        id = @build_id
        AND status_group NOT IN ('ready', 'failed')
    RETURNING id
)
DELETE FROM public.active_template_builds
WHERE build_id IN (SELECT id FROM finished);
