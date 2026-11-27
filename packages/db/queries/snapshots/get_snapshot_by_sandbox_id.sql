-- name: GetSnapshotBySandboxID :one
-- Returns the live snapshot row for a sandbox together with the build assigned
-- to its internal restore template under the 'default' tag. A snapshot is live
-- while its restore env is not soft-deleted (active_envs).
SELECT s.env_id, s.team_id, eba.build_id
FROM "public"."snapshots" s
JOIN "public"."active_envs" e ON e.id = s.env_id
LEFT JOIN "public"."env_build_assignments" eba ON eba.env_id = s.env_id AND eba.tag = 'default'
WHERE s.sandbox_id = $1
ORDER BY eba.created_at DESC
LIMIT 1;
