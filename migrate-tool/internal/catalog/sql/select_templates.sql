-- Template 存放在 envs 表;只迁移 template / snapshot_template 两种来源,
-- 其它 source(如 runtime snapshot)不属于本工具范围。
SELECT
    id,
    created_at,
    updated_at,
    public,
    build_count,
    spawn_count,
    last_spawned_at,
    team_id::text,
    created_by::text,
    cluster_id::text,
    source
FROM public.envs
WHERE source IN ('template', 'snapshot_template')
ORDER BY id
