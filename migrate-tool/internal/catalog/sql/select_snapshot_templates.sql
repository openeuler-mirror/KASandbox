-- snapshot_templates 记录 Snapshot Template 的一对一来源信息。
SELECT s.env_id, s.sandbox_id, COALESCE(s.created_at, 'epoch'::timestamptz)
FROM public.snapshot_templates s
JOIN public.envs e ON e.id = s.env_id
WHERE e.source = 'snapshot_template'
ORDER BY s.env_id
