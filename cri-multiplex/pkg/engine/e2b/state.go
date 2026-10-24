package e2b

import (
	"sort"
	"time"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

// E2BPodState 是 e2b sandbox 的持久化状态（布局与旧版逐字一致）。
type E2BPodState struct {
	SandboxID       string            `json:"sandbox_id"`
	E2BSandboxID    string            `json:"e2b_sandbox_id"`
	PodUID          string            `json:"pod_uid"`
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	EndedAt         *time.Time        `json:"ended_at,omitempty"`
	State           e2bState          `json:"state"`
	TemplateID      string            `json:"template_id"`
	BuildID         string            `json:"build_id"`
	ExecutionID     string            `json:"execution_id,omitempty"`
	TeamID          string            `json:"team_id,omitempty"`
	NodeName        string            `json:"node_name,omitempty"`
	ImageRef        string            `json:"image_ref"`
	EnvdAccessToken string            `json:"envd_access_token,omitempty"`

	ContainerLabels      map[string]string `json:"container_labels,omitempty"`
	ContainerAnnotations map[string]string `json:"container_annotations,omitempty"`
	ContainerName        string            `json:"container_name,omitempty"`
	ContainerCommand     []string          `json:"container_command,omitempty"`
	ContainerArgs        []string          `json:"container_args,omitempty"`
	ContainerStdin       bool              `json:"container_stdin"`
	ContainerTTY         bool              `json:"container_tty"`
	ContainerState       e2bContainerState `json:"container_state"`
	ContainerCreatedAt   time.Time         `json:"container_created_at"`
	ContainerStartedAt   time.Time         `json:"container_started_at"`
	ContainerFinishedAt  time.Time         `json:"container_finished_at"`
	ContainerExitCode    int32             `json:"container_exit_code"`
	ContainerLogPath     string            `json:"container_log_path,omitempty"`
	FullLogPath          string            `json:"full_log_path,omitempty"`
	PodLogDirectory      string            `json:"pod_log_directory,omitempty"`

	HostIP       string             `json:"host_ip,omitempty"`
	HostPort     int                `json:"host_port,omitempty"`
	PodIP        string             `json:"pod_ip,omitempty"`
	CNIEnabled   bool               `json:"cni_enabled"`
	CNIRecord    *util.CNIRecord    `json:"cni_record,omitempty"`
	PortMappings []util.PortMapping `json:"port_mappings,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// E2BOperation 与状态常量暂时复用根包定义（admin v1 仍按根包类型接线）；
// Task 13 引入中性 Admin DTO 后改为 e2b 自己的类型。
type E2BOperation = engine.E2BOperation

// RuntimeInfo 暂时复用根包定义（admin v1 仍按根包类型接线）。
type RuntimeInfo = engine.RuntimeInfo

const (
	OperationStateRunning   = engine.OperationStateRunning
	OperationStateSucceeded = engine.OperationStateSucceeded
	OperationStateFailed    = engine.OperationStateFailed

	legacyOperationStatePending = engine.LegacyOperationStatePending
	operationRestartError       = engine.OperationRestartError
)

// 状态读写全部走通用 KV，序列化后的布局与旧的 typed 方法逐字一致。
func (e *Engine) loadPods() ([]E2BPodState, error) {
	if e == nil || e.stateStore == nil {
		return nil, nil
	}
	var pods []E2BPodState
	if _, err := e.stateStore.LoadBackendState("e2b.pods", &pods); err != nil {
		return nil, err
	}
	return pods, nil
}

func (e *Engine) savePod(p E2BPodState) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e == nil || e.stateStore == nil {
		return nil
	}
	pods, err := e.loadPods()
	if err != nil {
		return err
	}
	p.UpdatedAt = time.Now()
	return e.stateStore.SaveBackendState("e2b.pods", upsertE2BPodState(pods, p))
}

func (e *Engine) deletePod(sandboxID string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e == nil || e.stateStore == nil {
		return nil
	}
	pods, err := e.loadPods()
	if err != nil {
		return err
	}
	return e.stateStore.SaveBackendState("e2b.pods", deleteE2BPodState(pods, sandboxID))
}

func (e *Engine) loadImages() ([]string, error) {
	if e == nil || e.stateStore == nil {
		return nil, nil
	}
	var images []string
	if _, err := e.stateStore.LoadBackendState("e2b.images", &images); err != nil {
		return nil, err
	}
	return images, nil
}

func (e *Engine) saveImage(imageRef string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e == nil || e.stateStore == nil || imageRef == "" {
		return nil
	}
	images, err := e.loadImages()
	if err != nil {
		return err
	}
	return e.stateStore.SaveBackendState("e2b.images", upsertStrings(images, imageRef))
}

func (e *Engine) deleteImage(imageRef string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e == nil || e.stateStore == nil || imageRef == "" {
		return nil
	}
	images, err := e.loadImages()
	if err != nil {
		return err
	}
	return e.stateStore.SaveBackendState("e2b.images", deleteStrings(images, imageRef))
}

func (e *Engine) loadOperations() (map[string]E2BOperation, error) {
	operations := make(map[string]E2BOperation)
	if e == nil || e.stateStore == nil {
		return operations, nil
	}
	if _, err := e.stateStore.LoadBackendState("e2b_operations", &operations); err != nil {
		return nil, err
	}
	return operations, nil
}

func (e *Engine) persistOperation(op E2BOperation) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e == nil || e.stateStore == nil || op.OperationID == "" {
		return nil
	}
	operations, err := e.loadOperations()
	if err != nil {
		return err
	}
	operations[op.OperationID] = op
	return e.saveOperations(operations)
}

func (e *Engine) saveOperations(operations map[string]E2BOperation) error {

	if e == nil || e.stateStore == nil {
		return nil
	}
	if len(operations) == 0 {
		return e.stateStore.DeleteBackendState("e2b_operations")
	}
	return e.stateStore.SaveBackendState("e2b_operations", operations)
}

func upsertStrings(values []string, v string) []string {
	for _, item := range values {
		if item == v {
			return values
		}
	}
	values = append(values, v)
	sort.Strings(values)
	return values
}

func deleteStrings(values []string, v string) []string {
	out := values[:0]
	for _, item := range values {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

func upsertE2BPodState(records []E2BPodState, p E2BPodState) []E2BPodState {
	for i, record := range records {
		if record.SandboxID == p.SandboxID {
			records[i] = p
			return records
		}
	}
	return append(records, p)
}

func deleteE2BPodState(records []E2BPodState, sandboxID string) []E2BPodState {
	out := records[:0]
	for _, record := range records {
		if record.SandboxID != sandboxID {
			out = append(out, record)
		}
	}
	return out
}

func cloneE2BPodStates(values []E2BPodState) []E2BPodState {
	out := make([]E2BPodState, len(values))
	copy(out, values)
	return out
}

// failInFlightOperations 把重启前仍在运行的操作标记为失败（旧 JSONStateStore 的构造期语义）。
func (e *Engine) failInFlightOperations() error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	operations, err := e.loadOperations()
	if err != nil || len(operations) == 0 {
		return err
	}
	now := time.Now()
	changed := false
	for id, op := range operations {
		if op.State != OperationStateRunning && op.State != legacyOperationStatePending {
			continue
		}
		op.State = OperationStateFailed
		op.Error = operationRestartError
		if op.FinishedAt.IsZero() {
			op.FinishedAt = now
		}
		operations[id] = op
		changed = true
	}
	if !changed {
		return nil
	}
	return e.saveOperations(operations)
}
