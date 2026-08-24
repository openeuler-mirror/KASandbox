-- 核对工具依赖的数据库能力:唯一索引、外键、status_group 触发器,以及
-- 对相关表的 RLS 已旁路。任一缺失都会在 preflight 阶段显式失败。
SELECT
    EXISTS (
        SELECT 1
        FROM pg_catalog.pg_class index_class
        JOIN pg_catalog.pg_index index_data ON index_data.indexrelid = index_class.oid
        WHERE index_class.oid = to_regclass('public.idx_env_aliases_alias_namespace_unique')
          AND index_data.indisunique
          AND index_data.indnullsnotdistinct
    ),
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.env_build_assignments'::regclass
          AND confrelid = 'public.env_builds'::regclass
          AND conname = 'fk_env_build_assignments_build'
          AND contype = 'f'
    ),
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.env_build_assignments'::regclass
          AND confrelid = 'public.envs'::regclass
          AND conname = 'fk_env_build_assignments_env'
          AND contype = 'f'
    ),
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.env_aliases'::regclass
          AND confrelid = 'public.envs'::regclass
          AND conname = 'env_aliases_envs_env_aliases'
          AND contype = 'f'
    ),
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.snapshot_templates'::regclass
          AND confrelid = 'public.envs'::regclass
          AND contype = 'f'
    ),
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_trigger
        WHERE tgrelid = 'public.env_builds'::regclass
          AND tgname = 'trg_compute_status_group'
          AND tgenabled <> 'D'
          AND NOT tgisinternal
    ),
    NOT (
        row_security_active('public.teams'::regclass)
        OR row_security_active('public.envs'::regclass)
        OR row_security_active('public.env_aliases'::regclass)
        OR row_security_active('public.env_builds'::regclass)
        OR row_security_active('public.env_build_assignments'::regclass)
        OR row_security_active('public.snapshot_templates'::regclass)
    )
