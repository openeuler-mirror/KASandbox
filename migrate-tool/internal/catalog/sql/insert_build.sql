-- 只插入目标缺失的 Build。cluster_node_id 固定写 NULL:源环境的节点标识
-- 在目标集群没有意义。status_group 由目标触发器按 status 重算,写入值须与
-- 领域映射一致(插入前由 validateBuildStatusGroup 核对)。
INSERT INTO public.env_builds (
    id, created_at, updated_at, finished_at, status, dockerfile, start_cmd,
    vcpu, ram_mb, free_disk_size_mb, total_disk_size_mb, kernel_version,
    firecracker_version, env_id, envd_version, ready_cmd, cluster_node_id,
    reason, version, cpu_architecture, cpu_family, cpu_model, cpu_model_name,
    cpu_flags, status_group, team_id
) VALUES (
    $1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, NULL, $17::jsonb, $18, $19, $20, $21, $22, $23, $24,
    $25::uuid
)
