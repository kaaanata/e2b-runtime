-- Workspace block volumes. See migrations/20261017191510_block_volumes.sql.

-- name: CreateBlockVolume :one
INSERT INTO volumes (id, team_id, volume_type, name, backend, owner_node_id, capacity_bytes, lifecycle)
VALUES (@id, @team_id, @volume_type, @name, 'block', @owner_node_id, @capacity_bytes, 'creating')
RETURNING *;

-- name: GetBlockVolumeForUpdate :one
SELECT * FROM volumes WHERE id = @volume_id AND backend = 'block' FOR UPDATE;

-- name: SetBlockVolumeReady :exec
UPDATE volumes
SET lifecycle = 'ready', head_incarnation_id = @head_incarnation_id
WHERE id = @volume_id AND backend = 'block' AND lifecycle = 'creating';

-- name: SetBlockVolumeFailed :exec
UPDATE volumes SET lifecycle = 'failed'
WHERE id = @volume_id AND backend = 'block' AND lifecycle = 'creating';

-- MarkBlockVolumeDeleting blocks new attachments and starts the tombstone.
-- It returns no row when the volume is not in a deletable state.
-- name: MarkBlockVolumeDeleting :one
UPDATE volumes SET lifecycle = 'deleting'
WHERE id = @volume_id AND team_id = @team_id AND backend = 'block'
  AND lifecycle IN ('ready', 'failed') AND deleted_at IS NULL
RETURNING *;

-- ListTeamBlockVolumesToDelete lists a team's block volumes that a teardown
-- still has to delete (not yet deleting or deleted).
-- name: ListTeamBlockVolumesToDelete :many
SELECT * FROM volumes
WHERE team_id = @team_id AND backend = 'block' AND deleted_at IS NULL
  AND lifecycle NOT IN ('deleting', 'deleted')
ORDER BY id;

-- TombstoneBlockVolume runs once every image of the volume is physically gone.
-- name: TombstoneBlockVolume :exec
UPDATE volumes SET lifecycle = 'deleted', deleted_at = now()
WHERE id = @volume_id AND backend = 'block' AND lifecycle = 'deleting';

-- name: CreateVolumeOperation :one
INSERT INTO volume_operations (id, team_id, volume_id, kind, idempotency_key, params_hash, params)
VALUES (@id, @team_id, @volume_id, @kind, @idempotency_key, @params_hash, @params)
ON CONFLICT (kind, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetVolumeOperationByKey :one
SELECT * FROM volume_operations WHERE kind = @kind AND idempotency_key = @idempotency_key;

-- name: GetVolumeOperation :one
SELECT * FROM volume_operations WHERE id = @id AND team_id = @team_id;

-- name: GetVolumeOperationForUpdate :one
SELECT * FROM volume_operations WHERE id = @id FOR UPDATE;

-- name: MarkVolumeOperationRunning :exec
UPDATE volume_operations
SET state = 'running', attempts = attempts + 1, updated_at = now()
WHERE id = @id AND state IN ('pending', 'running', 'unknown');

-- MarkVolumeOperationUnknown records that an RPC attempt ended without a
-- definite answer; the next attempt reconciles before retrying.
-- name: MarkVolumeOperationUnknown :exec
UPDATE volume_operations
SET state = 'unknown', error = @error, updated_at = now()
WHERE id = @id AND state IN ('pending', 'running', 'unknown');

-- name: CompleteVolumeOperation :exec
UPDATE volume_operations
SET state = 'succeeded', result = @result, error = NULL, updated_at = now(), finished_at = now()
WHERE id = @id AND state IN ('pending', 'running', 'unknown');

-- name: FailVolumeOperation :exec
UPDATE volume_operations
SET state = 'failed', error = @error, updated_at = now(), finished_at = now()
WHERE id = @id AND state IN ('pending', 'running', 'unknown');

-- name: InsertVolumeIncarnation :one
INSERT INTO volume_incarnations (id, volume_id, parent_revision_id, node_id, subvol_path, subvol_uuid, fs_uuid, capacity_bytes, operation_id)
VALUES (@id, @volume_id, @parent_revision_id, @node_id, @subvol_path, @subvol_uuid, @fs_uuid, @capacity_bytes, @operation_id)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetVolumeIncarnation :one
SELECT * FROM volume_incarnations WHERE id = @id AND volume_id = @volume_id;

-- name: ListLiveVolumeIncarnations :many
SELECT * FROM volume_incarnations WHERE volume_id = @volume_id AND state <> 'deleted' ORDER BY created_at;

-- name: MarkVolumeIncarnationDeleted :exec
UPDATE volume_incarnations SET state = 'deleted', deleted_at = now()
WHERE id = @id AND volume_id = @volume_id;

-- name: InsertVolumeRevision :one
INSERT INTO volume_revisions (id, volume_id, incarnation_id, parent_id, subvol_uuid, fs_uuid, size_bytes, consistency, operation_id)
VALUES (@id, @volume_id, @incarnation_id, @parent_id, @subvol_uuid, @fs_uuid, @size_bytes, @consistency, @operation_id)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetVolumeRevision :one
SELECT * FROM volume_revisions WHERE id = @id AND volume_id = @volume_id;

-- name: ListVolumeRevisions :many
SELECT * FROM volume_revisions
WHERE volume_id = @volume_id AND state = 'ready'
ORDER BY created_at DESC, id;

-- name: ListLiveVolumeRevisions :many
SELECT * FROM volume_revisions WHERE volume_id = @volume_id AND state <> 'deleted' ORDER BY created_at;

-- name: GetLatestIncarnationRevision :one
SELECT * FROM volume_revisions
WHERE incarnation_id = @incarnation_id AND state = 'ready'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: MarkVolumeRevisionDeleted :exec
UPDATE volume_revisions SET state = 'deleted', deleted_at = now()
WHERE id = @id AND volume_id = @volume_id;

-- CreateVolumeAttachment fails with a unique violation on
-- volume_attachments_single_writer when the incarnation already has an
-- active writer.
-- name: CreateVolumeAttachment :one
INSERT INTO volume_attachments (id, volume_id, incarnation_id, sandbox_id, execution_id, node_id, mode, desired_state, guest_path)
VALUES (@id, @volume_id, @incarnation_id, @sandbox_id, @execution_id, @node_id, @mode, 'attaching', @guest_path)
RETURNING *;

-- name: GetActiveIncarnationWriter :one
SELECT * FROM volume_attachments
WHERE incarnation_id = @incarnation_id AND mode = 'rw' AND released_at IS NULL;

-- name: ListActiveSandboxAttachments :many
SELECT * FROM volume_attachments
WHERE sandbox_id = @sandbox_id AND released_at IS NULL
ORDER BY created_at;

-- IncarnationAttachedSince reports whether any execution attached the
-- incarnation after the given time, i.e. whether a revision sealed at that
-- time is still the incarnation's content.
-- name: IncarnationAttachedSince :one
SELECT EXISTS (
    SELECT 1 FROM volume_attachments
    WHERE incarnation_id = @incarnation_id AND created_at > @since
)::boolean AS attached;

-- name: ReleaseSandboxAttachments :many
UPDATE volume_attachments
SET desired_state = 'released', released_at = now()
WHERE sandbox_id = @sandbox_id AND execution_id = @execution_id AND released_at IS NULL
RETURNING *;

-- name: ReleaseVolumeAttachment :exec
UPDATE volume_attachments
SET desired_state = 'released', released_at = now()
WHERE id = @id AND released_at IS NULL;

-- name: CountActiveVolumeAttachments :one
SELECT count(*) FROM volume_attachments WHERE volume_id = @volume_id AND released_at IS NULL;

-- name: InsertSnapshotVolumeRef :exec
INSERT INTO snapshot_volume_refs (snapshot_build_id, revision_id, volume_id, mount_path, slot)
VALUES (@snapshot_build_id, @revision_id, @volume_id, @mount_path, @slot)
ON CONFLICT (snapshot_build_id, revision_id) DO NOTHING;

-- name: CountVolumeSnapshotRefs :one
SELECT count(*) FROM snapshot_volume_refs WHERE volume_id = @volume_id;

-- name: ListSnapshotVolumeRefs :many
SELECT
    r.snapshot_build_id,
    r.mount_path,
    r.slot,
    sqlc.embed(rev),
    sqlc.embed(v)
FROM snapshot_volume_refs r
JOIN volume_revisions rev ON rev.id = r.revision_id
JOIN volumes v ON v.id = r.volume_id
WHERE r.snapshot_build_id = @snapshot_build_id
ORDER BY r.slot;

-- ReleaseTemplateSnapshotVolumeRefs drops the block volume references of
-- every build of a template that is being deleted (a killed paused sandbox's
-- snapshot, or a discarded checkpoint): nothing can resume those builds, so
-- their revisions must no longer keep the volumes from being deleted. The
-- revisions themselves stay until the volume is deleted.
-- name: ReleaseTemplateSnapshotVolumeRefs :execrows
DELETE FROM snapshot_volume_refs
WHERE snapshot_build_id IN (
    SELECT eba.build_id FROM env_build_assignments eba WHERE eba.env_id = @template_id
    UNION
    SELECT eb.id FROM env_builds eb WHERE eb.env_id = @template_id
);
