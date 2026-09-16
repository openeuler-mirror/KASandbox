-- 只插入目标缺失的 Snapshot Template 来源行(与 envs 行一对一)。
INSERT INTO public.snapshot_templates (env_id, sandbox_id, created_at)
VALUES ($1, $2, $3)
