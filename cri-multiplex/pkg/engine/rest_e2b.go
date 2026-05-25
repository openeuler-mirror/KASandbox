package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// createSandboxRequest matches the POST /sandboxes JSON body.
// Only templateID is required; commented fields document the full API contract.
type createSandboxRequest struct {
	TemplateID string `json:"templateID"`
	// Timeout             *int32                    `json:"timeout,omitempty"`
	// AutoPause           *bool                     `json:"autoPause,omitempty"`
	// AllowInternetAccess *bool                     `json:"allow_internet_access,omitempty"`
	// EnvVars             map[string]string         `json:"envVars,omitempty"`
	// Metadata            map[string]string         `json:"metadata,omitempty"`
}

// listedSandboxItem matches an entry in the GET /sandboxes response array.
type listedSandboxItem struct {
	SandboxID string `json:"sandboxID"`
	Alias     string `json:"alias,omitempty"`
	State     string `json:"state"`
	StartedAt string `json:"startedAt,omitempty"`
	// TemplateID  string  `json:"templateID"`
	// ClientID    string  `json:"clientID"`
	// CpuCount    int32   `json:"cpuCount"`
	// MemoryMB    int32   `json:"memoryMB"`
	// EndAt       string  `json:"endAt"`
}

// createSandboxResponse matches the 201 response from POST /sandboxes.
type createSandboxResponse struct {
	SandboxID string `json:"sandboxID"`
	// TemplateID         string  `json:"templateID"`
	// ClientID           string  `json:"clientID"`
	// EnvdVersion        string  `json:"envdVersion"`
	// EnvdAccessToken    *string `json:"envdAccessToken,omitempty"`
}

type restE2BEngine struct {
	apiBaseURL string
	apiKey     string
	tracker    *podTracker
}

func newRestE2BEngine(apiBaseURL, apiKey string) *restE2BEngine {
	log.Printf("[RestE2BEngine] API base URL: %s", apiBaseURL)
	return &restE2BEngine{
		apiBaseURL: apiBaseURL,
		apiKey:     apiKey,
		tracker:    newPodTracker(),
	}
}

func (e *restE2BEngine) Type() EngineType { return EngineTypeE2B }

func (e *restE2BEngine) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.apiBaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("X-API-Key", e.apiKey)
	}
	return http.DefaultClient.Do(req)
}

func (e *restE2BEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[RestE2BEngine] RunPodSandbox: name=%s", req.Config.Metadata.Name)

	templateID, ok := req.Config.Annotations[annTemplateID]
	if !ok || templateID == "" {
		return nil, fmt.Errorf("annotation %q is required for REST backend", annTemplateID)
	}

	payload, _ := json.Marshal(&createSandboxRequest{TemplateID: templateID})
	resp, err := e.doRequest(ctx, http.MethodPost, "/sandboxes", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("POST /sandboxes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST /sandboxes returned %d: %s", resp.StatusCode, string(body))
	}

	var sandboxResp createSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sandboxResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if sandboxResp.SandboxID == "" {
		return nil, fmt.Errorf("POST /sandboxes returned empty sandboxID")
	}

	e.tracker.Add(sandboxResp.SandboxID, &podInfo{
		sandboxID:   sandboxResp.SandboxID,
		podUID:      req.Config.Metadata.Uid,
		name:        req.Config.Metadata.Name,
		namespace:   req.Config.Metadata.Namespace,
		labels:      req.Config.Labels,
		annotations: req.Config.Annotations,
		createdAt:   time.Now(),
	})

	log.Printf("[RestE2BEngine] sandbox created: %s (template=%s)", sandboxResp.SandboxID, templateID)
	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxResp.SandboxID}, nil
}

func (e *restE2BEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	log.Printf("[RestE2BEngine] StopPodSandbox: id=%s", req.PodSandboxId)

	resp, err := e.doRequest(ctx, http.MethodPost, "/sandboxes/"+req.PodSandboxId+"/pause", nil)
	if err != nil {
		return nil, fmt.Errorf("POST /sandboxes/%s/pause: %w", req.PodSandboxId, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST /sandboxes/%s/pause returned %d: %s", req.PodSandboxId, resp.StatusCode, string(body))
	}

	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *restE2BEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	log.Printf("[RestE2BEngine] RemovePodSandbox: id=%s", req.PodSandboxId)

	resp, err := e.doRequest(ctx, http.MethodDelete, "/sandboxes/"+req.PodSandboxId, nil)
	if err != nil {
		return nil, fmt.Errorf("DELETE /sandboxes/%s: %w", req.PodSandboxId, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("DELETE /sandboxes/%s returned %d: %s", req.PodSandboxId, resp.StatusCode, string(body))
	}

	e.tracker.Delete(req.PodSandboxId)
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *restE2BEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	log.Printf("[RestE2BEngine] PodSandboxStatus: id=%s (stub)", req.PodSandboxId)
	return &runtime.PodSandboxStatusResponse{
		Status: &runtime.PodSandboxStatus{
			Id:    req.PodSandboxId,
			State: runtime.PodSandboxState_SANDBOX_READY,
		},
	}, nil
}

func (e *restE2BEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	log.Println("[RestE2BEngine] ListPodSandbox")

	resp, err := e.doRequest(ctx, http.MethodGet, "/sandboxes", nil)
	if err != nil {
		return nil, fmt.Errorf("GET /sandboxes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /sandboxes returned %d: %s", resp.StatusCode, string(body))
	}

	var list []listedSandboxItem
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}

	items := make([]*runtime.PodSandbox, 0, len(list))
	for _, item := range list {
		pb := &runtime.PodSandbox{Id: item.SandboxID}

		switch item.State {
		case "paused":
			pb.State = runtime.PodSandboxState_SANDBOX_NOTREADY
		default:
			pb.State = runtime.PodSandboxState_SANDBOX_READY
		}

		if item.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, item.StartedAt); err == nil {
				pb.CreatedAt = t.UnixNano()
			}
		}

		if p, ok := e.tracker.Get(item.SandboxID); ok {
			pb.Metadata = &runtime.PodSandboxMetadata{
				Name:      p.name,
				Uid:       p.podUID,
				Namespace: p.namespace,
			}
			pb.Labels = p.labels
			pb.Annotations = p.annotations
		} else {
			pb.Metadata = &runtime.PodSandboxMetadata{
				Name: item.Alias,
			}
		}

		items = append(items, pb)
	}

	return &runtime.ListPodSandboxResponse{Items: items}, nil
}

func (e *restE2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[RestE2BEngine] CreateContainer: pod=%s, name=%s (stub)", req.PodSandboxId, req.Config.Metadata.Name)
	containerID := fmt.Sprintf("e2b-rest-ctr-%s-%s", req.PodSandboxId, req.Config.Metadata.Name)
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *restE2BEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	log.Printf("[RestE2BEngine] StartContainer: id=%s (stub)", req.ContainerId)
	return &runtime.StartContainerResponse{}, nil
}

func (e *restE2BEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	log.Printf("[RestE2BEngine] StopContainer: id=%s (stub)", req.ContainerId)
	return &runtime.StopContainerResponse{}, nil
}

func (e *restE2BEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	log.Printf("[RestE2BEngine] RemoveContainer: id=%s (stub)", req.ContainerId)
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *restE2BEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	log.Println("[RestE2BEngine] ListContainers (stub)")
	return &runtime.ListContainersResponse{Containers: []*runtime.Container{}}, nil
}

func (e *restE2BEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	log.Printf("[RestE2BEngine] ContainerStatus: id=%s (stub)", req.ContainerId)
	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id:    req.ContainerId,
			State: runtime.ContainerState_CONTAINER_RUNNING,
		},
	}, nil
}

func (e *restE2BEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	log.Printf("[RestE2BEngine] ContainerStats: id=%s (stub)", req.ContainerId)
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{}}, nil
}

func (e *restE2BEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	log.Println("[RestE2BEngine] ListContainerStats (stub)")
	return &runtime.ListContainerStatsResponse{Stats: []*runtime.ContainerStats{}}, nil
}

func (e *restE2BEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	log.Printf("[RestE2BEngine] UpdateContainerResources: id=%s (stub)", req.ContainerId)
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (e *restE2BEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	log.Printf("[RestE2BEngine] ReopenContainerLog: id=%s (stub)", req.ContainerId)
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *restE2BEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	log.Printf("[RestE2BEngine] ExecSync: id=%s, cmd=%v (stub)", req.ContainerId, req.Cmd)
	return &runtime.ExecSyncResponse{Stdout: []byte(""), ExitCode: 0}, nil
}

func (e *restE2BEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	log.Printf("[RestE2BEngine] Exec: id=%s (stub)", req.ContainerId)
	return &runtime.ExecResponse{Url: ""}, nil
}

func (e *restE2BEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	log.Printf("[RestE2BEngine] Attach: id=%s (stub)", req.ContainerId)
	return &runtime.AttachResponse{Url: ""}, nil
}

func (e *restE2BEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	log.Printf("[RestE2BEngine] PortForward: id=%s, ports=%v (stub)", req.PodSandboxId, req.Port)
	return &runtime.PortForwardResponse{Url: ""}, nil
}

func (e *restE2BEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	log.Println("[RestE2BEngine] Status")
	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{{Status: true}},
		},
	}, nil
}

func (e *restE2BEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	log.Println("[RestE2BEngine] Version")
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "e2b",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *restE2BEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	log.Println("[RestE2BEngine] UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (e *restE2BEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	log.Println("[RestE2BEngine] GetContainerEvents (not implemented)")
	return nil, fmt.Errorf("GetContainerEvents not implemented")
}

func (e *restE2BEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	log.Println("[RestE2BEngine] ListImages")
	return &runtime.ListImagesResponse{Images: []*runtime.Image{}}, nil
}

func (e *restE2BEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	log.Println("[RestE2BEngine] ImageStatus")
	return &runtime.ImageStatusResponse{}, nil
}

func (e *restE2BEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	log.Println("[RestE2BEngine] PullImage")
	return &runtime.PullImageResponse{}, nil
}

func (e *restE2BEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	log.Println("[RestE2BEngine] RemoveImage")
	return &runtime.RemoveImageResponse{}, nil
}

func (e *restE2BEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	log.Println("[RestE2BEngine] ImageFsInfo")
	return &runtime.ImageFsInfoResponse{}, nil
}

func (e *restE2BEngine) Close() error {
	return nil
}
