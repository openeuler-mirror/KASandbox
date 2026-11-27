-- name: UpsertInternalSnapshot :one
-- UpsertSnapshot for the internal pause/checkpoint metadata endpoints, which
-- write snapshots for sandboxes the API does not run (CRI sandboxes). It differs
-- from UpsertSnapshot in two ways:
--   * a snapshot whose restore env was soft-deleted counts as missing: a fresh
--     env is created and the snapshots row adopts it, so a sandbox can be paused
--     again under the same ID after DELETE /internal/sandboxes/{id}/snapshot;
--   * the build's CPU info is the one reported by the caller (the node the
--     sandbox ran on), not copied from a source build.
WITH new_template AS (
    INSERT INTO "public"."envs" (id, public, created_by, team_id, updated_at, source, cluster_id)
    SELECT @template_id, FALSE, NULL, @team_id, now(), 'snapshot', @cluster_id
    WHERE NOT EXISTS (
        SELECT s.id
        FROM "public"."snapshots" s
        JOIN "public"."active_envs" e ON e.id = s.env_id
        WHERE s.sandbox_id = @sandbox_id
    ) RETURNING id
),

-- Create a new snapshot or update an existing one
snapshot as (
    INSERT INTO "public"."snapshots" (
       sandbox_id,
       base_env_id,
       team_id,
       env_id,
       metadata,
       sandbox_started_at,
       env_secure,
       allow_internet_access,
       origin_node_id,
       auto_pause,
       config,
       created_at
    )
    VALUES (
            @sandbox_id,
            @base_template_id,
            @team_id,
            -- If a live snapshot already exists, new_template id will be null, env_id can't be null, so use placeholder ''
            COALESCE((SELECT id FROM new_template), ''),
            @metadata,
            @started_at,
            @secure,
            @allow_internet_access,
            @origin_node_id,
            @auto_pause,
            @config,
            now()
   )
    ON CONFLICT (sandbox_id) DO UPDATE SET
        -- The row's env was soft-deleted: a fresh env was created above, adopt it.
        -- For a live row excluded.env_id is the '' placeholder, so keep the old env_id.
        env_id = COALESCE(NULLIF(excluded.env_id, ''), snapshots.env_id),
        metadata = excluded.metadata,
        sandbox_started_at = excluded.sandbox_started_at,
        allow_internet_access = COALESCE(excluded.allow_internet_access, snapshots.allow_internet_access),
        origin_node_id = excluded.origin_node_id,
        auto_pause = excluded.auto_pause,
        config = excluded.config
    RETURNING env_id as template_id
),

new_build as (
    INSERT INTO "public"."env_builds" (
        vcpu,
        ram_mb,
        free_disk_size_mb,
        kernel_version,
        firecracker_version,
        envd_version,
        status,
        cluster_node_id,
        total_disk_size_mb,
        updated_at,
        cpu_architecture,
        cpu_family,
        cpu_model,
        cpu_model_name,
        cpu_flags
    )
    VALUES (
        @vcpu,
        @ram_mb,
        @free_disk_size_mb,
        @kernel_version,
        @firecracker_version,
        @envd_version,
        @status,
        @origin_node_id,
        @total_disk_size_mb,
        now(),
        sqlc.narg(cpu_architecture),
        sqlc.narg(cpu_family),
        sqlc.narg(cpu_model),
        sqlc.narg(cpu_model_name),
        @cpu_flags
    )
    RETURNING id as build_id
),

-- Create the build assignment edge (explicit, not relying on trigger)
build_assignment as (
    INSERT INTO "public"."env_build_assignments" (env_id, build_id, tag)
    VALUES (
        (SELECT template_id FROM snapshot),
        (SELECT build_id FROM new_build),
        'default'
    )
    RETURNING build_id, env_id as template_id
)

SELECT build_id, template_id FROM build_assignment;
