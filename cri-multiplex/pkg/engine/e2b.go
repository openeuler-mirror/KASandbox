package engine

import (
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const annTemplateID = "e2b.dev/template-id"
const annExposePorts = "e2b.dev/expose-ports" // 新增

type E2BConfig struct {
	OrchestratorAddr      string
	OrchestratorProxyAddr string
	NodeIP                string
	CNI                   CNIConfig
	StateStore            StateStore
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
	return newGRPCE2BEngine(cfg.OrchestratorAddr, cfg.OrchestratorProxyAddr, cfg.NodeIP, cfg.CNI, cfg.StateStore)
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

func (p *podInfo) toPersistedState() E2BPodState {
	if p == nil {
		return E2BPodState{}
	}
	return E2BPodState{
		SandboxID:            p.sandboxID,
		E2BSandboxID:         p.e2bSandboxID,
		PodUID:               p.podUID,
		Name:                 p.name,
		Namespace:            p.namespace,
		Labels:               copyStringMap(p.labels),
		Annotations:          copyStringMap(p.annotations),
		CreatedAt:            p.createdAt,
		EndedAt:              p.endedAt,
		State:                p.state,
		TemplateID:           p.templateID,
		BuildID:              p.buildID,
		ImageRef:             p.imageRef,
		EnvdAccessToken:      p.envdAccessToken,
		ContainerLabels:      copyStringMap(p.containerLabels),
		ContainerAnnotations: copyStringMap(p.containerAnnotations),
		ContainerName:        p.containerName,
		ContainerCommand:     append([]string(nil), p.containerCommand...),
		ContainerArgs:        append([]string(nil), p.containerArgs...),
		ContainerStdin:       p.containerStdin,
		ContainerTTY:         p.containerTTY,
		ContainerState:       p.containerState,
		ContainerCreatedAt:   p.containerCreatedAt,
		ContainerStartedAt:   p.containerStartedAt,
		ContainerFinishedAt:  p.containerFinishedAt,
		ContainerExitCode:    p.containerExitCode,
		HostIP:               p.hostIP,
		HostPort:             p.hostPort,
		PodIP:                p.podIP,
		CNIEnabled:           p.cniEnabled,
		CNIRecord:            cloneCNIRecord(p.cniRecord),
		PortMappings:         append([]PortMapping(nil), p.portMappings...),
	}
}

func podInfoFromPersistedState(p E2BPodState) *podInfo {
	info := &podInfo{
		sandboxID:       p.SandboxID,
		e2bSandboxID:    p.E2BSandboxID,
		podUID:          p.PodUID,
		name:            p.Name,
		namespace:       p.Namespace,
		labels:          copyStringMap(p.Labels),
		annotations:     copyStringMap(p.Annotations),
		createdAt:       p.CreatedAt,
		endedAt:         p.EndedAt,
		state:           p.State,
		templateID:      p.TemplateID,
		buildID:         p.BuildID,
		imageRef:        p.ImageRef,
		envdAccessToken: p.EnvdAccessToken,

		containerLabels:      copyStringMap(p.ContainerLabels),
		containerAnnotations: copyStringMap(p.ContainerAnnotations),
		containerName:        p.ContainerName,
		containerCommand:     append([]string(nil), p.ContainerCommand...),
		containerArgs:        append([]string(nil), p.ContainerArgs...),
		containerStdin:       p.ContainerStdin,
		containerTTY:         p.ContainerTTY,
		containerState:       p.ContainerState,
		containerCreatedAt:   p.ContainerCreatedAt,
		containerStartedAt:   p.ContainerStartedAt,
		containerFinishedAt:  p.ContainerFinishedAt,
		containerExitCode:    p.ContainerExitCode,

		hostIP:       p.HostIP,
		hostPort:     p.HostPort,
		podIP:        p.PodIP,
		cniEnabled:   p.CNIEnabled,
		cniRecord:    cloneCNIRecord(p.CNIRecord),
		portMappings: append([]PortMapping(nil), p.PortMappings...),
	}
	return info
}

func cloneCNIRecord(rec *CNIRecord) *CNIRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	out.DNS = append([]string(nil), rec.DNS...)
	out.ResultJSON = append([]byte(nil), rec.ResultJSON...)
	return &out
}
