-- name: GetSnapshotTemplateWithReadyBuild :one
-- Fetches a snapshot (checkpoint) template with its ready build by template ID and tag.
-- Checkpoint templates have no snapshots row and no recorded base env, so unlike
-- GetLastSnapshot no base-env aliases are returned.
SELECT st.sandbox_id, st.created_at, sqlc.embed(eb)
FROM "public"."snapshot_templates" st
JOIN "public"."envs" e ON e.id = st.env_id AND e.deleted_at IS NULL
JOIN "public"."env_build_assignments" eba ON eba.env_id = e.id AND eba.tag = @tag
JOIN "public"."env_builds" eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
WHERE st.env_id = @template_id
ORDER BY eba.created_at DESC
LIMIT 1;
