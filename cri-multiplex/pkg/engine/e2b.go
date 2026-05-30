package engine

import "time"

const annTemplateID = "e2b.dev/template-id"

type E2BBackendType string

const (
	BackendGRPC E2BBackendType = "grpc"
	BackendREST E2BBackendType = "rest"
)

type E2BConfig struct {
	Backend          E2BBackendType
	OrchestratorAddr string
	APIBaseURL       string
	APIKey           string
	NodeIP           string // 用于 PodSandboxStatus 网络状态报告
}

type E2BEngine interface {
	RuntimeEngine
}

func NewE2BEngine(cfg *E2BConfig) E2BEngine {
	switch cfg.Backend {
	case BackendREST:
		return newRestE2BEngine(cfg.APIBaseURL, cfg.APIKey)
	default:
		return newGRPCE2BEngine(cfg.OrchestratorAddr, cfg.NodeIP)
	}
}

// e2bState 定义在 e2b.go 以便被 pod_tracker.go 和 grpc_e2b.go 共用
type e2bState int

const (
	stateRunning e2bState = iota
	statePaused
	stateRemoved
)

type podInfo struct {
	sandboxID   string
	podUID      string
	name        string
	namespace   string
	labels      map[string]string
	annotations map[string]string
	createdAt   time.Time
	endedAt     *time.Time
	state       e2bState // 新增：本地状态机
	templateID  string   // 新增：用于 Pause 请求
	buildID     string   // 新增：用于 Pause 请求
}

