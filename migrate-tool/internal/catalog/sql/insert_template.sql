-- 只插入目标缺失的 Template(envs 行);已存在的行在 dry-run 阶段已按 ID
-- 逐字段比对判定 identical/conflict,提交阶段绝不 UPDATE。
INSERT INTO public.envs (
    id, created_at, updated_at, public, build_count, spawn_count,
    last_spawned_at, team_id, created_by, cluster_id, source
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid, $9::uuid, $10::uuid, $11)
