-- name: GetEnvBuildStatusByID :one
-- Returns the current status of a build by ID, regardless of the owning env's
-- source (template, snapshot, snapshot_template). Used by the internal build
-- status write-back endpoint for idempotency/conflict checks.
SELECT eb.id, eb.status, eb.status_group
FROM "public"."env_builds" eb
WHERE eb.id = $1;
