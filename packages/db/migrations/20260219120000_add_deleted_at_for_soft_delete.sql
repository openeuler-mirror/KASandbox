-- +goose Up
-- +goose NO TRANSACTION

ALTER TABLE public.envs ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE public.snapshots ADD COLUMN IF NOT EXISTS deleted_at timestamptz;

-- +goose Down
-- +goose NO TRANSACTION

ALTER TABLE public.envs DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE public.snapshots DROP COLUMN IF EXISTS deleted_at;
