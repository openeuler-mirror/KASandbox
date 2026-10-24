package engine

import (
	"context"
	"time"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type EngineType string

const (
	EngineTypeContainer EngineType = "container"
	EngineTypeE2B       EngineType = "e2b"
	EngineTypeAndroid   EngineType = "android"
)

type RuntimeEngine interface {
	Type() EngineType

	RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error)
	StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error)
	RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error)
	PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error)
	ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error)

	CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error)
	StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error)
	StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error)
	RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error)
	ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error)
	ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error)
	ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error)
	ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error)

	UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error)
	ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error)

	ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error)
	Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error)
	Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error)
	PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error)

	Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error)
	Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error)
	UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error)

	GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error)

	ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error)
	ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error)
	PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error)
	RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error)
	ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error)

	Close() error
}

// defaultString 返回 value，空值时返回 fallback。
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// E2BOperation 与 RuntimeInfo 暂时留在根包，供 admin v1 服务使用；
// Task 13 引入中性 Admin DTO 后删除。
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

// E2B operation 状态取值，暂时留在根包供 admin v1 与 e2b 共用。
const (
	OperationStateRunning   = "Running"
	OperationStateSucceeded = "Succeeded"
	OperationStateFailed    = "Failed"

	LegacyOperationStatePending = "Pending"
	OperationRestartError       = "process restarted during operation"
)

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
