package engine

import "time"

// 本文件是 admin v1 的兼容视图类型（E2B 专用），只为既有 v1 service 保留：
// 设计 §3.3.7.4 要求 v1 原样保留至少两个版本，而 v1 的 proto 暴露了 team_id /
// execution_id / CNI gateway+dns / 操作记录里的 cri+e2b ID 等 E2B 专有字段，
// 中性 DTO（admin.go）不承载这些字段。v2 只用 admin.go 的 DTO，新后端不要依赖本文件。
//
// Deprecated: 新代码请使用 admin.go 的中性 DTO 与 Admin* 能力接口。

// E2BOperation 是一条 E2B 管理操作的持久化记录（v1 视图）。
type E2BOperation struct {
	OperationID  string    `json:"operation_id"`
	Action       string    `json:"action"`
	CRISandboxID string    `json:"cri_sandbox_id,omitempty"`
	E2BSandboxID string    `json:"e2b_sandbox_id,omitempty"`
	TeamID       string    `json:"team_id,omitempty"`
	TemplateID   string    `json:"template_id,omitempty"`
	BuildID      string    `json:"build_id,omitempty"`
	State        string    `json:"state"`
	Error        string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
}

// E2B operation 状态取值，v1 与 e2b 共用。
const (
	OperationStateRunning   = "Running"
	OperationStateSucceeded = "Succeeded"
	OperationStateFailed    = "Failed"

	LegacyOperationStatePending = "Pending"
	OperationRestartError       = "process restarted during operation"
)

// RuntimeInfo 是 admin v1 GetSandboxRuntime 的视图（E2B 专有字段齐全）。
type RuntimeInfo struct {
	CRISandboxID string
	E2BSandboxID string
	TeamID       string
	PodName      string
	PodNamespace string
	PodUID       string
	NodeName     string
	TemplateID   string
	BuildID      string
	ExecutionID  string
	RuntimeState string
	CNIEnabled   bool
	NetNSPath    string
	IfName       string
	PodIP        string
	Gateway      string
	DNS          []string
	HostPorts    []HostPortMapping
}

// HostPortMapping 是 admin v1 视图里的 host→sandbox 端口映射。
type HostPortMapping struct {
	HostPort    int
	SandboxPort int
}
