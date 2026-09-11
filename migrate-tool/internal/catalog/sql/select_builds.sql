-- Source Snapshot passes an empty ID list and follows Assignments. Imports
-- additionally inspect the requested primary keys, including orphan rows.
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
LEFT JOIN public.env_build_assignments a ON a.build_id = b.id
LEFT JOIN public.envs e ON e.id = a.env_id
WHERE e.source IN ('template', 'snapshot_template') OR b.id = ANY($1::uuid[])
ORDER BY b.id::text
