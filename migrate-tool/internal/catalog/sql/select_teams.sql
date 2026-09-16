-- Team 不属于 Bundle,但导入必须用它解析目标 Team,并取得目标 Cluster。
SELECT id::text, slug, name, cluster_id::text
FROM public.teams
ORDER BY id
