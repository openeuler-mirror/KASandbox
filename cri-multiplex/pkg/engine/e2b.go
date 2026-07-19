package engine

import (
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

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
	CNI                   CNIConfig
}

type CNIConfig struct {
	Enabled     bool
	ConfDir     string
	BinDir      string
	IfName      string
	NetNSDir    string
	NetNSPrefix string
}

type E2BEngine interface {
	RuntimeEngine
}

func NewE2BEngine(cfg *E2BConfig) E2BEngine {
	switch cfg.Backend {
	case BackendREST:
		return newRestE2BEngine(cfg.APIBaseURL, cfg.APIKey)
	default:
		return newGRPCE2BEngine(cfg.OrchestratorAddr, cfg.OrchestratorProxyAddr, cfg.NodeIP, cfg.CNI)
	}
}

type e2bState int

const (
	stateRunning e2bState = iota
	stateStopped
	statePaused
	stateRemoved
)

type e2bContainerState int

const (
	containerStateCreated e2bContainerState = iota
	containerStateRunning
	containerStateExited
	containerStateRemoved
)

type podInfo struct {
	sandboxID       string
	e2bSandboxID    string
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
	containerCommand     []string
	containerArgs        []string
	containerStdin       bool
	containerTTY         bool
	containerState       e2bContainerState
	containerCreatedAt   time.Time
	containerStartedAt   time.Time
	containerFinishedAt  time.Time
	containerExitCode    int32

	// 网络信息
	hostIP     string
	hostPort   int // 默认端口 49983 的映射
	podIP      string
	cniEnabled bool
	cniRecord  *CNIRecord

	// 新增：多端口映射
	portMappings []PortMapping // hostPort -> sandboxPort
}

func (p *podInfo) envdSandboxID() string {
	if p == nil {
		return ""
	}
	if p.e2bSandboxID != "" {
		return p.e2bSandboxID
	}
	return p.sandboxID
}

func (p *podInfo) toPodSandboxConfig() *runtime.PodSandboxConfig {
	if p == nil {
		return nil
	}
	return &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{
			Name:      p.name,
			Uid:       p.podUID,
			Namespace: p.namespace,
		},
		Labels:      p.labels,
		Annotations: p.annotations,
	}
}
