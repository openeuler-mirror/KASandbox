-- Alias 只跟随可迁移 Template 加载;(namespace, alias) 的唯一性由
-- catalog.Validate 按数据库唯一键同款语义校验。
SELECT a.id::text, a.env_id, a.namespace, a.alias, a.is_renamable
FROM public.env_aliases a
JOIN public.envs e ON e.id = a.env_id
WHERE e.source IN ('template', 'snapshot_template')
ORDER BY a.id
