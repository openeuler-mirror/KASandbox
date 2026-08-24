-- 只插入目标缺失的 Alias;(namespace, alias) 冲突由数据库唯一键兜底。
INSERT INTO public.env_aliases (id, alias, env_id, is_renamable, namespace)
VALUES ($1::uuid, $2, $3, $4, $5)
