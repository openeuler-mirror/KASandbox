-- name: SoftDeleteTemplate :many
-- Soft-deletes a template and deletes its aliases, returning the deleted alias
-- cache keys for cache invalidation.
-- Idempotent: calling it on an already soft-deleted template returns an empty list.
WITH soft_deleted AS (
  UPDATE "public"."envs" AS e
  SET deleted_at = now()
  WHERE e.id = @template_id
    AND e.deleted_at IS NULL
  RETURNING e.id
), deleted_aliases AS (
  DELETE FROM "public"."env_aliases" AS ea
  WHERE ea.env_id IN (SELECT id FROM soft_deleted)
  RETURNING CASE
    WHEN namespace IS NOT NULL THEN namespace || '/' || alias
    ELSE alias
  END::text AS alias_key
)
SELECT alias_key FROM deleted_aliases;
