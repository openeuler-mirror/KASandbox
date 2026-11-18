package agentenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	annotationContainerHash         = "io.kubernetes.container.hash"
	annotationContainerRestartCount = "io.kubernetes.container.restartCount"
)

// CreateContainer 只创建 CRI 逻辑投影，不启动新的 microVM。
func (e *Engine) CreateContainer(_ context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	if req == nil || req.Config == nil || req.Config.Metadata == nil {
		return nil, status.Error(codes.InvalidArgument, "container config and metadata are required")
	}
	podID := req.GetPodSandboxId()
	name := req.Config.Metadata.GetName()
	imageName := req.Config.GetImage().GetImage()
	if podID == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(imageName) == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox id, container name and image are required")
	}
	imageReference, imageReferenceID, err := e.resolveVirtualImage(imageName)
	if err != nil {
		return nil, err
	}
	pod, ok := e.getPod(podID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", podID)
	}
	if pod.State != criStateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s is not ready", podID)
	}

	containerID := agentenvContainerID(podID, name, req.Config.Metadata.GetAttempt())
	if existing, found := e.getContainer(containerID); found && existing.State != containerStateRemoved {
		return &runtime.CreateContainerResponse{ContainerId: existing.ContainerID}, nil
	}
	labels := cloneStringMap(req.Config.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	for _, key := range []string{annotationContainerHash, annotationContainerRestartCount} {
		if value, ok := req.Config.Annotations[key]; ok {
			labels[key] = value
		}
	}
	logPath, err := resolveContainerLogPath(pod, req.Config.LogPath)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	record := containerRecord{
		ContainerID:  containerID,
		CRISandboxID: podID,
		Name:         name,
		Attempt:      req.Config.Metadata.GetAttempt(),
		ImageName:    imageReference,
		ImageRef:     imageReferenceID,
		State:        containerStateCreated,
		Command:      append([]string(nil), req.Config.Command...),
		Args:         append([]string(nil), req.Config.Args...),
		Env:          keyValueMap(req.Config.Envs),
		WorkingDir:   req.Config.WorkingDir,
		Tty:          req.Config.Tty,
		Stdin:        req.Config.Stdin,
		LogPath:      logPath,
		Labels:       labels,
		Annotations:  cloneStringMap(req.Config.Annotations),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := e.putContainer(record); err != nil {
		return nil, status.Errorf(codes.Internal, "create container projection: %v", err)
	}
	e.publishContainerEvent(containerID, runtime.ContainerEventType_CONTAINER_CREATED_EVENT)
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func resolveContainerLogPath(pod podRecord, logPath string) (string, error) {
	logPath = strings.TrimSpace(logPath)
	if logPath == "" {
		return "", nil
	}
	if filepath.IsAbs(logPath) {
		return filepath.Clean(logPath), nil
	}
	logDirectory := strings.TrimSpace(pod.LogDirectory)
	if logDirectory == "" || !filepath.IsAbs(logDirectory) {
		return "", status.Error(codes.InvalidArgument, "container log path is relative and pod log directory is unavailable")
	}
	return filepath.Join(logDirectory, logPath), nil
}

func keyValueMap(values []*runtime.KeyValue) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		if value == nil || value.GetKey() == "" {
			continue
		}
		out[value.GetKey()] = value.GetValue()
	}
	return out
}

func (e *Engine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	unlock := e.lockContainer(req.GetContainerId())
	defer unlock()
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	pod, ok := e.getPod(record.CRISandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", record.CRISandboxID)
	}
	if pod.State != criStateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s is not ready", record.CRISandboxID)
	}
	if record.State == containerStateRunning {
		if record.NativeProcessID == 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "container %s is running without a native process", record.ContainerID)
		}
		return &runtime.StartContainerResponse{}, nil
	}
	if record.State != containerStateCreated {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is in state %s", record.ContainerID, record.State)
	}

	pid, err := e.startContainerProcess(ctx, record)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_, _, err = e.mutateContainer(record.ContainerID, func(record *containerRecord) {
		record.State = containerStateRunning
		record.NativeProcessID = pid
		record.StartedAt = now
		record.FinishedAt = time.Time{}
		record.ExitCode = 0
		record.LastError = ""
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), containerProcessAPITimeout)
		_, _ = e.stopContainerProcess(cleanupCtx, containerRecord{
			ContainerID:     record.ContainerID,
			CRISandboxID:    record.CRISandboxID,
			NativeProcessID: pid,
		})
		cancel()
		return nil, status.Errorf(codes.Internal, "persist started container: %v", err)
	}
	e.publishContainerEvent(record.ContainerID, runtime.ContainerEventType_CONTAINER_STARTED_EVENT)
	go e.monitorContainerProcess(record.ContainerID)
	return &runtime.StartContainerResponse{}, nil
}

func (e *Engine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	unlock := e.lockContainer(req.GetContainerId())
	defer unlock()
	record, ok := e.getContainer(req.GetContainerId())
	if !ok {
		return &runtime.StopContainerResponse{}, nil
	}
	return e.stopContainerLocked(ctx, record)
}

func (e *Engine) stopContainerLocked(ctx context.Context, record containerRecord) (*runtime.StopContainerResponse, error) {
	if record.State == containerStateRemoved || record.State == containerStateExited {
		return &runtime.StopContainerResponse{}, nil
	}
	exitCode := int32(0)
	if record.State == containerStateRunning {
		var err error
		exitCode, err = e.stopContainerProcess(ctx, record)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now()
	transitioned := false
	_, _, err := e.mutateContainer(record.ContainerID, func(record *containerRecord) {
		if record.State == containerStateExited {
			return
		}
		transitioned = true
		record.State = containerStateExited
		record.ExitCode = exitCode
		record.FinishedAt = now
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stop container projection: %v", err)
	}
	if transitioned {
		e.publishContainerEvent(record.ContainerID, runtime.ContainerEventType_CONTAINER_STOPPED_EVENT)
	}
	return &runtime.StopContainerResponse{}, nil
}

func (e *Engine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	unlock := e.lockContainer(req.GetContainerId())
	defer unlock()
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return &runtime.RemoveContainerResponse{}, nil
	}
	if record.State == containerStateRunning {
		if _, err := e.stopContainerLocked(ctx, record); err != nil {
			return nil, err
		}
		record, _ = e.getContainer(record.ContainerID)
	}
	now := time.Now()
	_, _, err := e.mutateContainer(record.ContainerID, func(record *containerRecord) {
		if record.State != containerStateExited {
			record.FinishedAt = now
			record.ExitCode = 0
		}
		record.State = containerStateRemoved
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove container projection: %v", err)
	}
	e.publishContainerEvent(record.ContainerID, runtime.ContainerEventType_CONTAINER_DELETED_EVENT)
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *Engine) ListContainers(_ context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	var filter *runtime.ContainerFilter
	if req != nil {
		filter = req.Filter
	}
	items := make([]*runtime.Container, 0, len(e.listContainers()))
	for _, record := range e.listContainers() {
		if record.State == containerStateRemoved {
			continue
		}
		items = append(items, &runtime.Container{
			Id:           record.ContainerID,
			PodSandboxId: record.CRISandboxID,
			Metadata: &runtime.ContainerMetadata{
				Name:    record.Name,
				Attempt: record.Attempt,
			},
			Image:       &runtime.ImageSpec{Image: record.ImageName},
			ImageRef:    record.ImageRef,
			State:       containerCRIState(record.State),
			CreatedAt:   unixNano(record.CreatedAt),
			Labels:      cloneStringMap(record.Labels),
			Annotations: cloneStringMap(record.Annotations),
		})
	}
	return &runtime.ListContainersResponse{Containers: filterCRIContainers(items, filter)}, nil
}

func (e *Engine) ContainerStatus(_ context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	return &runtime.ContainerStatusResponse{Status: containerStatusFromRecord(record)}, nil
}

func containerStatusFromRecord(record containerRecord) *runtime.ContainerStatus {
	return &runtime.ContainerStatus{
		Id: record.ContainerID,
		Metadata: &runtime.ContainerMetadata{
			Name:    record.Name,
			Attempt: record.Attempt,
		},
		State:       containerCRIState(record.State),
		CreatedAt:   unixNano(record.CreatedAt),
		StartedAt:   unixNano(record.StartedAt),
		FinishedAt:  unixNano(record.FinishedAt),
		ExitCode:    record.ExitCode,
		Image:       &runtime.ImageSpec{Image: record.ImageName},
		ImageRef:    record.ImageRef,
		Labels:      cloneStringMap(record.Labels),
		Annotations: cloneStringMap(record.Annotations),
		LogPath:     record.LogPath,
	}
}

func (e *Engine) UpdateContainerResources(_ context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	if record, ok := e.getContainer(req.GetContainerId()); !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	return nil, status.Error(codes.Unimplemented, "AgentENV UpdateContainerResources is not implemented")
}

func (e *Engine) ReopenContainerLog(_ context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	unlock := e.lockContainer(req.GetContainerId())
	defer unlock()
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	if record.LogPath == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s has no log path", record.ContainerID)
	}
	_, _, err := e.mutateContainer(record.ContainerID, func(current *containerRecord) {
		current.LogGeneration++
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "request container log reopen: %v", err)
	}
	return &runtime.ReopenContainerLogResponse{}, nil
}

func agentenvContainerID(podSandboxID, name string, attempt uint32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", podSandboxID, name, attempt)))
	return "agentenv-c-" + hex.EncodeToString(sum[:])[:32]
}

// validateVirtualImage 强制 AgentENV 容器只使用虚拟镜像，避免把外部 OCI 镜像误当成可执行运行时。
func validateVirtualImage(reference string) error {
	if reference != strings.TrimSpace(reference) || !strings.HasPrefix(reference, VirtualImagePrefix) {
		return status.Errorf(codes.InvalidArgument,
			"image %q is not an AgentENV virtual image; expected %s<name>", reference, VirtualImagePrefix)
	}
	name := strings.TrimPrefix(reference, VirtualImagePrefix)
	if name == "" || strings.ContainsAny(name, " \t\r\n\\") {
		return status.Errorf(codes.InvalidArgument, "AgentENV virtual image %q has an invalid name", reference)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return status.Errorf(codes.InvalidArgument, "AgentENV virtual image %q has an invalid path", reference)
		}
	}
	return nil
}
func agentenvImageRef(reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return "agentenv-image:" + hex.EncodeToString(sum[:])[:32]
}

// resolveVirtualImage ?? kubelet ???????????? ImageStatus ????? ID?
func (e *Engine) resolveVirtualImage(reference string) (string, string, error) {
	if err := validateVirtualImage(reference); err == nil {
		return reference, agentenvImageRef(reference), nil
	} else if !isAgentENVVirtualImageID(reference) {
		return "", "", err
	}

	record, ok := e.findImage(reference)
	if !ok {
		return "", "", status.Errorf(codes.NotFound, "AgentENV virtual image %q is not registered", reference)
	}
	if err := validateVirtualImage(record.Reference); err != nil {
		return "", "", status.Errorf(codes.Internal, "persisted AgentENV virtual image %q is invalid: %v", record.Reference, err)
	}
	return record.Reference, record.ID, nil
}

// isAgentENVVirtualImageID ??? agentenvImageRef ??????? ID?
func isAgentENVVirtualImageID(reference string) bool {
	const prefix = "agentenv-image:"
	if !strings.HasPrefix(reference, prefix) {
		return false
	}
	encoded := strings.TrimPrefix(reference, prefix)
	if len(encoded) != 32 || encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

func containerCRIState(state string) runtime.ContainerState {
	switch state {
	case containerStateCreated:
		return runtime.ContainerState_CONTAINER_CREATED
	case containerStateRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	default:
		return runtime.ContainerState_CONTAINER_EXITED
	}
}

func unixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func filterCRIContainers(items []*runtime.Container, filter *runtime.ContainerFilter) []*runtime.Container {
	if filter == nil {
		return items
	}
	out := make([]*runtime.Container, 0, len(items))
	for _, item := range items {
		if filter.GetId() != "" && item.GetId() != filter.GetId() {
			continue
		}
		if filter.GetPodSandboxId() != "" && item.GetPodSandboxId() != filter.GetPodSandboxId() {
			continue
		}
		if filter.GetState() != nil && item.GetState() != filter.GetState().GetState() {
			continue
		}
		matched := true
		for key, value := range filter.GetLabelSelector() {
			if item.GetLabels()[key] != value {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, item)
		}
	}
	return out
}
