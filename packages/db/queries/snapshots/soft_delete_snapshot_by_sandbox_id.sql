-- name: SoftDeleteSnapshotBySandboxID :one
-- Soft-deletes the snapshot belonging to a sandbox.
-- Returns no row when there is no live snapshot for the sandbox
-- (caller maps sqlc's no-rows error to 404).
UPDATE "public"."snapshots"
SET deleted_at = now()
WHERE sandbox_id = @sandbox_id
  AND deleted_at IS NULL
RETURNING env_id;
