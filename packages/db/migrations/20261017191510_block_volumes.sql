-- +goose Up
-- Workspace block volumes: a volume with backend 'block' is an ext4 image in a
-- Btrfs subvolume on one node. Its physical lineage (incarnations = writable
-- images, revisions = sealed read-only snapshots), the sandboxes writing it
-- (attachments), the snapshots that need a revision (snapshot_volume_refs) and
-- every retryable change (volume_operations, the ledger River jobs work from)
-- live in the tables below. NFS volumes keep using only `volumes`.

-- +goose StatementBegin
ALTER TABLE volumes
    ADD COLUMN backend             VARCHAR(32)   NOT NULL DEFAULT 'nfs',
    ADD COLUMN owner_node_id       VARCHAR(250),
    ADD COLUMN capacity_bytes      BIGINT,
    ADD COLUMN lifecycle           VARCHAR(32)   NOT NULL DEFAULT 'ready',
    ADD COLUMN head_incarnation_id UUID,
    ADD COLUMN deleted_at          TIMESTAMP WITH TIME ZONE,
    ADD CONSTRAINT volumes_backend_check
        CHECK (backend IN ('nfs', 'block')),
    ADD CONSTRAINT volumes_lifecycle_check
        CHECK (lifecycle IN ('creating', 'ready', 'failed', 'deleting', 'deleted')),
    -- A block volume is pinned to the node holding its images and has a fixed
    -- capacity tier from the moment it is created.
    ADD CONSTRAINT volumes_block_placement_check
        CHECK (backend <> 'block' OR (owner_node_id IS NOT NULL AND capacity_bytes > 0)),
    ADD CONSTRAINT volumes_tombstone_check
        CHECK ((deleted_at IS NULL) = (lifecycle <> 'deleted'));
-- +goose StatementEnd

-- A deleted block volume stays as a tombstone (its revisions and operations
-- still reference it), so the name is unique only among live volumes.
-- +goose StatementBegin
ALTER TABLE volumes DROP CONSTRAINT volumes_teams_uq;
CREATE UNIQUE INDEX volumes_teams_live_name_uq ON volumes (team_id, name) WHERE deleted_at IS NULL;
-- +goose StatementEnd

-- volume_operations is the ledger of retryable volume changes. River only
-- executes; a job carries the operation id and the business state is read
-- from here (River prunes finished jobs). idempotency_key is scoped by the
-- caller (team id or volume id prefix), so UNIQUE (kind, idempotency_key)
-- turns a replayed request into the same operation.
-- +goose StatementBegin
CREATE TABLE volume_operations (
    id              UUID                      PRIMARY KEY,
    team_id         UUID                      NOT NULL REFERENCES teams (id),
    -- Deferred so a request inserts its operation (the idempotency check)
    -- before the volume row it creates, in one transaction.
    volume_id       UUID                      REFERENCES volumes (id) DEFERRABLE INITIALLY DEFERRED,
    kind            VARCHAR(32)               NOT NULL,
    idempotency_key TEXT                      NOT NULL,
    params_hash     TEXT                      NOT NULL,
    params          JSONB                     NOT NULL,
    state           VARCHAR(32)               NOT NULL DEFAULT 'pending',
    result          JSONB,
    error           TEXT,
    attempts        INTEGER                   NOT NULL DEFAULT 0,
    created_at      TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),
    finished_at     TIMESTAMP WITH TIME ZONE,

    CONSTRAINT volume_operations_kind_check
        CHECK (kind IN ('create', 'clone', 'seal', 'delete', 'reconcile')),
    -- unknown: the last RPC attempt ended without a definite answer; the next
    -- attempt reconciles before it retries.
    CONSTRAINT volume_operations_state_check
        CHECK (state IN ('pending', 'running', 'unknown', 'succeeded', 'failed')),
    CONSTRAINT volume_operations_kind_key_uq
        UNIQUE (kind, idempotency_key)
);
CREATE INDEX volume_operations_volume_idx ON volume_operations (volume_id, created_at DESC);
-- +goose StatementEnd

-- An incarnation is one writable image (Btrfs subvolume holding
-- workspace.ext4). Rows are written once the orchestrator has published it.
-- +goose StatementBegin
CREATE TABLE volume_incarnations (
    id                 UUID                      PRIMARY KEY,
    volume_id          UUID                      NOT NULL REFERENCES volumes (id),
    parent_revision_id UUID,
    node_id            VARCHAR(250)              NOT NULL,
    subvol_path        TEXT                      NOT NULL,
    subvol_uuid        UUID                      NOT NULL,
    fs_uuid            UUID                      NOT NULL,
    capacity_bytes     BIGINT                    NOT NULL,
    state              VARCHAR(32)               NOT NULL DEFAULT 'ready',
    created_at         TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),
    deleted_at         TIMESTAMP WITH TIME ZONE,
    operation_id       UUID                      NOT NULL REFERENCES volume_operations (id),

    CONSTRAINT volume_incarnations_state_check
        CHECK (state IN ('ready', 'blocked', 'deleting', 'deleted'))
);
CREATE INDEX volume_incarnations_volume_idx ON volume_incarnations (volume_id);
-- +goose StatementEnd

-- A revision is an immutable read-only snapshot of an incarnation.
-- consistency records what the image is consistent with: an fsfreeze
-- ('filesystem-quiesced', cold-bootable) or a memory snapshot taken at the
-- same point ('paired-memory', restorable only together with that memory).
-- +goose StatementBegin
CREATE TABLE volume_revisions (
    id             UUID                      PRIMARY KEY,
    volume_id      UUID                      NOT NULL REFERENCES volumes (id),
    incarnation_id UUID                      NOT NULL REFERENCES volume_incarnations (id),
    parent_id      UUID                      REFERENCES volume_revisions (id),
    subvol_uuid    UUID                      NOT NULL,
    fs_uuid        UUID                      NOT NULL,
    size_bytes     BIGINT                    NOT NULL,
    consistency    VARCHAR(32)               NOT NULL,
    state          VARCHAR(32)               NOT NULL DEFAULT 'ready',
    created_at     TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMP WITH TIME ZONE,
    operation_id   UUID                      NOT NULL REFERENCES volume_operations (id),

    CONSTRAINT volume_revisions_consistency_check
        CHECK (consistency IN ('filesystem-quiesced', 'paired-memory')),
    CONSTRAINT volume_revisions_state_check
        CHECK (state IN ('ready', 'deleting', 'deleted'))
);
CREATE INDEX volume_revisions_volume_idx ON volume_revisions (volume_id, created_at DESC);
CREATE INDEX volume_revisions_incarnation_idx ON volume_revisions (incarnation_id, created_at DESC);

ALTER TABLE volume_incarnations
    ADD CONSTRAINT volume_incarnations_parent_revision_fk
        FOREIGN KEY (parent_revision_id) REFERENCES volume_revisions (id);
ALTER TABLE volumes
    ADD CONSTRAINT volumes_head_incarnation_fk
        FOREIGN KEY (head_incarnation_id) REFERENCES volume_incarnations (id);
-- +goose StatementEnd

-- An attachment binds one sandbox execution to an incarnation. The partial
-- unique index is the database half of the single-writer rule; the node-local
-- half is the blockvol holder gate.
-- +goose StatementBegin
CREATE TABLE volume_attachments (
    id             UUID                      PRIMARY KEY,
    volume_id      UUID                      NOT NULL REFERENCES volumes (id),
    incarnation_id UUID                      NOT NULL REFERENCES volume_incarnations (id),
    sandbox_id     TEXT                      NOT NULL,
    execution_id   TEXT                      NOT NULL,
    node_id        VARCHAR(250)              NOT NULL,
    mode           VARCHAR(8)                NOT NULL,
    desired_state  VARCHAR(32)               NOT NULL,
    observed_state VARCHAR(32)               NOT NULL DEFAULT 'unknown',
    drive_id       VARCHAR(64)               NOT NULL DEFAULT '',
    guest_path     TEXT                      NOT NULL,
    created_at     TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),
    released_at    TIMESTAMP WITH TIME ZONE,

    CONSTRAINT volume_attachments_mode_check CHECK (mode IN ('rw', 'ro')),
    CONSTRAINT volume_attachments_desired_state_check
        CHECK (desired_state IN ('attaching', 'attached', 'releasing', 'released'))
);
CREATE UNIQUE INDEX volume_attachments_single_writer
    ON volume_attachments (incarnation_id)
    WHERE mode = 'rw' AND released_at IS NULL;
CREATE INDEX volume_attachments_active_volume_idx
    ON volume_attachments (volume_id)
    WHERE released_at IS NULL;
CREATE INDEX volume_attachments_sandbox_idx
    ON volume_attachments (sandbox_id, execution_id);
-- +goose StatementEnd

-- snapshot_volume_refs keeps a revision alive while a paused-sandbox snapshot
-- (an env_builds row) needs it. Deleting the build drops the reference.
-- +goose StatementBegin
CREATE TABLE snapshot_volume_refs (
    snapshot_build_id UUID                      NOT NULL REFERENCES env_builds (id) ON DELETE CASCADE,
    revision_id       UUID                      NOT NULL REFERENCES volume_revisions (id),
    volume_id         UUID                      NOT NULL REFERENCES volumes (id),
    mount_path        TEXT                      NOT NULL,
    -- slot is the template workspace slot the volume occupied when the
    -- revision was sealed; a memory restore must bind it to the same slot.
    slot              SMALLINT                  NOT NULL CHECK (slot >= 0),
    created_at        TIMESTAMP WITH TIME ZONE  NOT NULL DEFAULT now(),

    PRIMARY KEY (snapshot_build_id, revision_id),
    UNIQUE (snapshot_build_id, slot)
);
CREATE INDEX snapshot_volume_refs_revision_idx ON snapshot_volume_refs (revision_id);
CREATE INDEX snapshot_volume_refs_volume_idx ON snapshot_volume_refs (volume_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS snapshot_volume_refs;
DROP TABLE IF EXISTS volume_attachments;
ALTER TABLE volumes DROP CONSTRAINT IF EXISTS volumes_head_incarnation_fk;
ALTER TABLE volume_incarnations DROP CONSTRAINT IF EXISTS volume_incarnations_parent_revision_fk;
DROP TABLE IF EXISTS volume_revisions;
DROP TABLE IF EXISTS volume_incarnations;
DROP TABLE IF EXISTS volume_operations;
-- Tombstones cannot survive the restored (team_id, name) constraint, and block
-- volumes have no meaning without the tables above.
DELETE FROM volumes WHERE deleted_at IS NOT NULL OR backend = 'block';
DROP INDEX IF EXISTS volumes_teams_live_name_uq;
ALTER TABLE volumes ADD CONSTRAINT volumes_teams_uq UNIQUE (team_id, name);
ALTER TABLE volumes
    DROP CONSTRAINT IF EXISTS volumes_tombstone_check,
    DROP CONSTRAINT IF EXISTS volumes_block_placement_check,
    DROP CONSTRAINT IF EXISTS volumes_lifecycle_check,
    DROP CONSTRAINT IF EXISTS volumes_backend_check,
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS head_incarnation_id,
    DROP COLUMN IF EXISTS lifecycle,
    DROP COLUMN IF EXISTS capacity_bytes,
    DROP COLUMN IF EXISTS owner_node_id,
    DROP COLUMN IF EXISTS backend;
-- +goose StatementEnd
