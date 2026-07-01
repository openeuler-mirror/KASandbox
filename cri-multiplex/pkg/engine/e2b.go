package engine

import "time"

const annTemplateID = "e2b.dev/template-id"
const annExposePorts = "e2b.dev/expose-ports" // 新增

type E2BBackendType string

const (
	BackendGRPC E2BBackendType = "grpc"
	BackendREST E2BBackendType = "rest"
)

type E2BConfig struct {
	Backend               E2BBackendType
	OrchestratorAddr      string
	OrchestratorProxyAddr string
	APIBaseURL            string
	APIKey                string
	NodeIP                string
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

type e2bState int

const (
	stateRunning e2bState = iota
	statePaused
	stateRemoved
)

type podInfo struct {
	sandboxID       string
	podUID          string
	name            string
	namespace       string
	labels          map[string]string
	annotations     map[string]string
	createdAt       time.Time
	endedAt         *time.Time
	state           e2bState
	templateID      string
	buildID         string
	imageRef        string
	envdAccessToken string

	// 容器元数据（由 CreateContainer 记录，用于 ListContainers/ContainerStatus 返回
	// kubelet PLEG 识别容器所需的 label，如 io.kubernetes.container.name）
	containerLabels      map[string]string
	containerAnnotations map[string]string
	containerName        string

	// 网络信息
	hostIP   string
	hostPort int // 默认端口 49983 的映射

	// 新增：多端口映射
	portMappings []PortMapping // hostPort -> sandboxPort
}

