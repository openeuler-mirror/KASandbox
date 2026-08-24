-- Build 必须经 Assignment 连接到可迁移 Template。DISTINCT 用来消除同一
-- Build 被多个 Template/Tag 复用时产生的重复行。
SELECT DISTINCT
    b.id::text,
    b.created_at,
    b.updated_at,
    b.finished_at,
    b.status,
    b.status_group,
    b.dockerfile,
    b.start_cmd,
    b.ready_cmd,
    b.vcpu,
    b.ram_mb,
    b.free_disk_size_mb,
    b.total_disk_size_mb,
    b.kernel_version,
    b.firecracker_version,
    b.env_id,
    b.envd_version,
    b.cluster_node_id,
    b.reason::text,
    b.version,
    b.cpu_architecture,
    b.cpu_family,
    b.cpu_model,
    b.cpu_model_name,
    COALESCE(b.cpu_flags, '{}'::text[]),
    COALESCE(b.team_id::text, '')
FROM public.env_builds b
JOIN public.env_build_assignments a ON a.build_id = b.id
JOIN public.envs e ON e.id = a.env_id
WHERE e.source IN ('template', 'snapshot_template')
ORDER BY b.id::text
