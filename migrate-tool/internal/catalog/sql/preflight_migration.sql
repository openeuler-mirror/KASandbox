-- 已应用的最高迁移版本号,用于 MinimumPostgresSchema 基线校验。
SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0)
FROM public._migrations
