-- 逐列核对工具依赖的表结构。$1 = 表名数组;期望的列名/类型/可空性清单
-- 是 postgres.go 里的 requiredPostgresColumns(唯一列规格表)。
SELECT table_name, column_name, udt_name, is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = ANY($1::text[])
