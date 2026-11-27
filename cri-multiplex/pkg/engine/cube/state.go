package cube

import "time"

const cubeStatePath = "cube.pods"

// CubePodState 是 cube 沙箱在本节点的持久化索引。
// 它只存在于后端自己的 KV 段，不进入共享的路由结构。
type CubePodState struct {
	CRISandboxID string `json:"cri_sandbox_id"`
	SandboxID    string `json:"sandbox_id"`
	SandboxUUID  string `json:"sandbox_uuid,omitempty"`
	SandboxIP    string `json:"sandbox_ip,omitempty"`
	OriginNode   string `json:"origin_node,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	VCPU         string `json:"vcpu,omitempty"`
	Memory       string `json:"memory,omitempty"`
	DiskLimit    string `json:"disk_limit,omitempty"`
	Backend      string `json:"backend,omitempty"`

	PauseSnapshotID      string `json:"pause_snapshot_id,omitempty"`
	CheckpointSnapshotID string `json:"checkpoint_snapshot_id,omitempty"`
	LastAdminOpID        string `json:"last_admin_op_id,omitempty"`
	LastAdminAction      string `json:"last_admin_action,omitempty"`

	State       string            `json:"state"`
	ParkedAt    time.Time         `json:"parked_at,omitempty"`
	StoppedAt   time.Time         `json:"stopped_at,omitempty"`
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	PodUID      string            `json:"pod_uid,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`

	ContainerID          string            `json:"container_id,omitempty"`
	ContainerName        string            `json:"container_name,omitempty"`
	ContainerImage       string            `json:"container_image,omitempty"`
	ContainerState       string            `json:"container_state,omitempty"`
	ContainerLabels      map[string]string `json:"container_labels,omitempty"`
	ContainerAnnotations map[string]string `json:"container_annotations,omitempty"`
	ContainerCreatedAt   time.Time         `json:"container_created_at,omitempty"`
	ContainerStartedAt   time.Time         `json:"container_started_at,omitempty"`
	ContainerFinishedAt  time.Time         `json:"container_finished_at,omitempty"`
	ContainerExitCode    int32             `json:"container_exit_code,omitempty"`
	ContainerAttempt     uint32            `json:"container_attempt,omitempty"`
	ContainerLogPath     string            `json:"container_log_path,omitempty"`
}
