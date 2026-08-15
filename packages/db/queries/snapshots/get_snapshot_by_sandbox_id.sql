-- name: GetSnapshotBySandboxID :one
-- Returns the live (not soft-deleted) snapshot row for a sandbox together with
-- the build assigned to its internal restore template under the 'default' tag.
SELECT s.env_id, s.team_id, eba.build_id
FROM "public"."snapshots" s
LEFT JOIN "public"."env_build_assignments" eba ON eba.env_id = s.env_id AND eba.tag = 'default'
WHERE s.sandbox_id = $1
  AND s.deleted_at IS NULL
ORDER BY eba.created_at DESC
LIMIT 1;
