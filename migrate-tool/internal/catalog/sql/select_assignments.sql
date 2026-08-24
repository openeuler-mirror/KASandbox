-- Assignment 是 Template 与 Build 之间的 Tag 绑定;历史行 created_at 可为
-- NULL,统一折算为 epoch 以获得稳定排序。
SELECT
    a.id::text,
    a.env_id,
    a.build_id::text,
    a.tag,
    a.source,
    COALESCE(a.created_at, 'epoch'::timestamptz)
FROM public.env_build_assignments a
JOIN public.envs e ON e.id = a.env_id
WHERE e.source IN ('template', 'snapshot_template')
ORDER BY a.created_at, a.id
