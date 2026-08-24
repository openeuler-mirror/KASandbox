-- 只插入目标缺失的 Assignment(Template 与 Build 的 Tag 绑定)。
INSERT INTO public.env_build_assignments (id, env_id, build_id, tag, source, created_at)
VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6)
