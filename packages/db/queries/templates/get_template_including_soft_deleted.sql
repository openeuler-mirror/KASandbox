-- name: GetTemplateIncludingSoftDeleted :one
-- Returns minimal env info regardless of source or soft-delete state.
-- Used by internal delete endpoints to keep repeat deletes idempotent.
SELECT t.id, t.team_id, t.deleted_at
FROM "public"."envs" t
WHERE t.id = $1;
