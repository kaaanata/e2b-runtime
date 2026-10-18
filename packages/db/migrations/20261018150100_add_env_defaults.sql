-- +goose Up
-- +goose StatementBegin

-- Upstream ships this as 20260915204740. This fork had already applied later
-- migrations (20261008100521 .. 20261018120000) when it arrived, and goose
-- refuses out-of-order versions, so it carries a later timestamp here; both
-- copies are CREATE TABLE IF NOT EXISTS and dedupe on a rebase.
--
CREATE TABLE IF NOT EXISTS public.env_defaults (
    env_id TEXT PRIMARY KEY REFERENCES public.envs(id),
    description TEXT
);

-- +goose StatementEnd

-- +goose Down
-- The table may hold data written under the separate dashboard migration
-- history. Retain it on rollback so applications using it continue to work.
