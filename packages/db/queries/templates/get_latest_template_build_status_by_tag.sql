-- name: GetLatestTemplateBuildStatusByTag :one
-- Returns the status of the most recent build assigned to a template under the
-- given tag, without status_group filtering. Used to detect in-progress
-- snapshotting builds before template deletion.
SELECT eb.id, eb.status, eb.status_group
FROM "public"."env_build_assignments" eba
JOIN "public"."env_builds" eb ON eb.id = eba.build_id
WHERE eba.env_id = $1
  AND eba.tag = $2
ORDER BY eba.created_at DESC
LIMIT 1;
