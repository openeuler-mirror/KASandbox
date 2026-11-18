package agentenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const podStatePath = "agentenv.pods"

const (
	criStateCreating       = "creating"
	criStateCleanupPending = "cleanup-pending"
	criStateReady          = "ready"
	criStateNotReady       = "notready"
)

// podRecord 是 agentenv-engine 对 CRI PodSandbox 的本地投影。
// native UUID 仍由 AgentENV Runtime 权威管理；这里只保存路由和 CRI 展示所需事实。
type runtimeNetworkRecord struct {
	Mode      string `json:"mode"`
	PodIP     string `json:"pod_ip,omitempty"`
	NetNSPath string `json:"netns_path,omitempty"`
}

type podRecord struct {
	// RuntimeNetwork 仅记录 Runtime 所有的网络，绝不能传给 CNI DEL。
	RuntimeNetwork     *runtimeNetworkRecord `json:"runtime_network,omitempty"`
	CRISandboxID       string                `json:"cri_sandbox_id"`
	ProductSandboxID   string                `json:"product_sandbox_id"`
	NativeSandboxID    string                `json:"native_sandbox_id,omitempty"`
	RuntimeDeleted     bool                  `json:"runtime_deleted,omitempty"`
	PodUID             string                `json:"pod_uid"`
	Name               string                `json:"name"`
	Namespace          string                `json:"namespace"`
	LogDirectory       string                `json:"log_directory,omitempty"`
	TemplateID         string                `json:"template_id"`
	BuildID            string                `json:"build_id"`
	Labels             map[string]string     `json:"labels,omitempty"`
	Annotations        map[string]string     `json:"annotations,omitempty"`
	CNIRecord          json.RawMessage       `json:"cni_record,omitempty"`
	HostPorts          json.RawMessage       `json:"host_ports,omitempty"`
	CreateOperationID  string                `json:"create_operation_id,omitempty"`
	RequestFingerprint string                `json:"request_fingerprint,omitempty"`
	CreateStartedAt    time.Time             `json:"create_started_at,omitempty"`
	LastError          string                `json:"last_error,omitempty"`
	State              string                `json:"state"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at,omitempty"`
}

const (
	containerStateCreated = "created"
	containerStateRunning = "running"
	containerStateExited  = "exited"
	containerStateRemoved = "removed"
)

// containerRecord 是 CRI 容器在 AgentENV microVM 内的逻辑投影。
// 容器不会创建新的 VM；命令、镜像和状态只用于满足 kubelet/CRI 语义。
type containerRecord struct {
	ContainerID  string            `json:"container_id"`
	CRISandboxID string            `json:"cri_sandbox_id"`
	Name         string            `json:"name"`
	Attempt      uint32            `json:"attempt"`
	ImageName    string            `json:"image_name"`
	ImageRef     string            `json:"image_ref"`
	State        string            `json:"state"`
	Command      []string          `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	Tty          bool              `json:"tty,omitempty"`
	Stdin        bool              `json:"stdin,omitempty"`
	LogPath      string            `json:"log_path,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	// NativeProcessID 是 guest envd 返回的 PID；0 表示尚未启动真实进程。
	NativeProcessID uint32 `json:"native_process_id,omitempty"`
	// ProcessLogSequence 是已经写入 CRI 日志并确认的最新输出序号。
	ProcessLogSequence uint64 `json:"process_log_sequence,omitempty"`
	// LogGeneration 每次 kubelet 请求日志轮转时递增，监控循环据此重新打开日志。
	LogGeneration uint64 `json:"log_generation,omitempty"`
	// LastError 保存进程启动、监控或停止失败的脱敏摘要。
	LastError  string    `json:"last_error,omitempty"`
	ExitCode   int32     `json:"exit_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// imageRecord 表示已经由 CRI 声明可用的虚拟镜像引用。
// AgentENV 模板决定实际 rootfs，镜像内容不会在节点侧拉取或执行。
type imageRecord struct {
	Reference string `json:"reference"`
	ID        string `json:"id"`
}

type persistedAgentENVState struct {
	Pods       []podRecord       `json:"pods"`
	Containers []containerRecord `json:"containers,omitempty"`
}

func (e *Engine) restorePods() error {
	if e == nil || e.store == nil {
		return nil
	}
	var state persistedAgentENVState
	found, err := e.store.LoadBackendState(podStatePath, &state)
	if err != nil {
		return fmt.Errorf("agentenv pods: load state: %w", err)
	}
	if !found {
		return nil
	}
	pods := make(map[string]*podRecord, len(state.Pods))
	for i := range state.Pods {
		record := state.Pods[i]
		if record.CRISandboxID == "" {
			return fmt.Errorf("agentenv pods: persisted CRI sandbox id is empty")
		}
		if record.NativeSandboxID == "" && record.State != criStateCreating && record.State != criStateCleanupPending {
			return fmt.Errorf("agentenv pods: persisted native sandbox id is empty for %s in state %s",
				record.CRISandboxID, record.State)
		}
		if _, duplicate := pods[record.CRISandboxID]; duplicate {
			return fmt.Errorf("agentenv pods: duplicate CRI sandbox id %s", record.CRISandboxID)
		}
		record.Labels = cloneStringMap(record.Labels)
		record.Annotations = cloneStringMap(record.Annotations)
		record.HostPorts = append(json.RawMessage(nil), record.HostPorts...)
		if record.CreatedAt.IsZero() {
			record.CreatedAt = time.Now()
		}
		if record.UpdatedAt.IsZero() {
			record.UpdatedAt = record.CreatedAt
		}
		if record.CreateStartedAt.IsZero() && record.State == criStateCreating {
			record.CreateStartedAt = record.CreatedAt
		}
		if legacyNetworkState(record) {
			return fmt.Errorf("agentenv pods: legacy CNI/HostPort state for %s; drain with the previous version before upgrading", record.CRISandboxID)
		}
		if record.RuntimeNetwork != nil {
			if record.RuntimeNetwork.Mode != runtimeManagedNetworkMode {
				return fmt.Errorf("agentenv pods: conflicting network ownership for %s", record.CRISandboxID)
			}
			network := *record.RuntimeNetwork
			record.RuntimeNetwork = &network
		}
		pods[record.CRISandboxID] = &record
	}
	containers := make(map[string]*containerRecord, len(state.Containers))
	for i := range state.Containers {
		record := cloneContainerRecord(&state.Containers[i])
		if record.ContainerID == "" || record.CRISandboxID == "" {
			return fmt.Errorf("agentenv containers: persisted container or sandbox id is empty")
		}
		if _, ok := pods[record.CRISandboxID]; !ok {
			return fmt.Errorf("agentenv containers: container %s references missing sandbox %s",
				record.ContainerID, record.CRISandboxID)
		}
		if !validContainerState(record.State) {
			return fmt.Errorf("agentenv containers: invalid state %q for %s", record.State, record.ContainerID)
		}
		if _, duplicate := containers[record.ContainerID]; duplicate {
			return fmt.Errorf("agentenv containers: duplicate container id %s", record.ContainerID)
		}
		normalizeContainerRecord(record)
		containers[record.ContainerID] = record
	}
	e.podMu.Lock()
	e.pods = pods
	e.containers = containers
	e.podMu.Unlock()
	return nil
}

func (e *Engine) getPod(criSandboxID string) (podRecord, bool) {
	if e == nil || criSandboxID == "" {
		return podRecord{}, false
	}
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	record, ok := e.pods[criSandboxID]
	if !ok {
		return podRecord{}, false
	}
	return *clonePodRecord(record), true
}

func (e *Engine) listPods() []podRecord {
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	out := make([]podRecord, 0, len(e.pods))
	for _, record := range e.pods {
		out = append(out, *clonePodRecord(record))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CRISandboxID < out[j].CRISandboxID })
	return out
}

func validatePodRecord(record podRecord) error {
	if record.CRISandboxID == "" {
		return fmt.Errorf("agentenv pods: CRI sandbox id is required")
	}
	if record.NativeSandboxID == "" && record.State != criStateCreating && record.State != criStateCleanupPending {
		return fmt.Errorf("agentenv pods: native sandbox id is required for state %s", record.State)
	}
	return nil
}

func preparePodRecord(record *podRecord, now time.Time) {
	record.Labels = cloneStringMap(record.Labels)
	record.Annotations = cloneStringMap(record.Annotations)
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
}

func (e *Engine) putPod(record podRecord) error {
	if err := validatePodRecord(record); err != nil {
		return err
	}
	e.podMu.Lock()
	now := time.Now()
	preparePodRecord(&record, now)
	previous, existed := e.pods[record.CRISandboxID]
	e.pods[record.CRISandboxID] = &record
	version := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	return e.persistPodStateChange(version, func() {
		if existed {
			e.pods[record.CRISandboxID] = previous
		} else {
			delete(e.pods, record.CRISandboxID)
		}
	})
}

// 状态和 AgentENV 私有 ID 对照分别保存，不扩展公共 StateStore。
func (e *Engine) putPodWithRoute(record podRecord, route podRoute) error {
	if err := e.putPod(record); err != nil {
		return err
	}
	return e.routes.PutPodRoute(route)
}

func (e *Engine) setPodState(criSandboxID, state string) error {
	return e.setPodStateWithRuntimeDeleted(criSandboxID, state, nil)
}

// setPodRuntimeDeleted 标记 Runtime 已删除，供 RemovePodSandbox 重试 Delete 时复用。
func (e *Engine) setPodRuntimeDeleted(criSandboxID string) error {
	deleted := true
	return e.setPodStateWithRuntimeDeleted(criSandboxID, criStateNotReady, &deleted)
}

func (e *Engine) setPodStateWithRuntimeDeleted(criSandboxID, state string, runtimeDeleted *bool) error {
	e.podMu.Lock()
	record, ok := e.pods[criSandboxID]
	if !ok {
		e.podMu.Unlock()
		return nil
	}
	previous := clonePodRecord(record)
	next := clonePodRecord(record)
	now := time.Now()
	next.State = state
	next.UpdatedAt = now
	if runtimeDeleted != nil {
		next.RuntimeDeleted = *runtimeDeleted
	} else if state != criStateNotReady {
		next.RuntimeDeleted = false
	}
	previousContainers := make(map[string]*containerRecord)
	if state == criStateNotReady {
		for id, container := range e.containers {
			if container.CRISandboxID != criSandboxID ||
				(container.State != containerStateCreated && container.State != containerStateRunning) {
				continue
			}
			previousContainers[id] = cloneContainerRecord(container)
			updated := cloneContainerRecord(container)
			updated.State = containerStateExited
			updated.ExitCode = 0
			updated.FinishedAt = now
			updated.UpdatedAt = now
			e.containers[id] = updated
		}
	}
	e.pods[criSandboxID] = next
	version := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	return e.persistPodStateChange(version, func() {
		e.pods[criSandboxID] = previous
		for id, container := range previousContainers {
			e.containers[id] = container
		}
	})
}

func (e *Engine) deletePod(criSandboxID string) error {
	e.podMu.Lock()
	previousPod, existed := e.pods[criSandboxID]
	if !existed {
		e.podMu.Unlock()
		return nil
	}
	previousContainers := make(map[string]*containerRecord)
	for id, container := range e.containers {
		if container.CRISandboxID == criSandboxID {
			previousContainers[id] = cloneContainerRecord(container)
			delete(e.containers, id)
		}
	}
	delete(e.pods, criSandboxID)
	version := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	return e.persistPodStateChange(version, func() {
		e.pods[criSandboxID] = previousPod
		for id, container := range previousContainers {
			e.containers[id] = container
		}
	})
}

func (e *Engine) markPodStateDirtyLocked() uint64 {
	e.podStateVersion++
	return e.podStateVersion
}

// persistPodStateChange 等待包含本次 mutation 的状态快照落盘。
// 失败时回滚内存变更并再提交一次修复快照，避免失败写入污染后续快照。
func (e *Engine) persistPodStateChange(version uint64, rollback func()) error {
	err := e.flushPodState(version)
	if err == nil {
		return nil
	}
	return e.repairPodStateAfterFailure(err, rollback)
}

func (e *Engine) repairPodStateAfterFailure(cause error, rollback func()) error {
	e.podMu.Lock()
	if rollback != nil {
		rollback()
	}
	repairVersion := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	if repairErr := e.flushPodState(repairVersion); repairErr != nil {
		return errors.Join(cause, repairErr)
	}
	return cause
}

// flushPodState 只串行化快照写入，不在持 podMu 时执行文件 fsync。
// 多个并发 mutation 可以让后一个快照覆盖前一个快照，从而自然合并写入。
func (e *Engine) flushPodState(minVersion uint64) error {
	if e == nil || e.store == nil {
		return nil
	}
	e.podPersistMu.Lock()
	defer e.podPersistMu.Unlock()
	if minVersion <= e.persistedPodStateVersion {
		return nil
	}
	e.podMu.RLock()
	snapshotVersion := e.podStateVersion
	state := e.snapshotPodStateLocked()
	e.podMu.RUnlock()
	if err := e.store.SaveBackendState(podStatePath, state); err != nil {
		return fmt.Errorf("agentenv pods: save state: %w", err)
	}
	e.persistedPodStateVersion = snapshotVersion
	return nil
}

func (e *Engine) snapshotPodStateLocked() persistedAgentENVState {
	state := persistedAgentENVState{
		Pods:       make([]podRecord, 0, len(e.pods)),
		Containers: make([]containerRecord, 0, len(e.containers)),
	}
	for _, record := range e.pods {
		state.Pods = append(state.Pods, *clonePodRecord(record))
	}
	for _, record := range e.containers {
		state.Containers = append(state.Containers, *cloneContainerRecord(record))
	}
	sort.Slice(state.Pods, func(i, j int) bool { return state.Pods[i].CRISandboxID < state.Pods[j].CRISandboxID })
	sort.Slice(state.Containers, func(i, j int) bool { return state.Containers[i].ContainerID < state.Containers[j].ContainerID })
	return state
}
func clonePodRecord(record *podRecord) *podRecord {
	if record == nil {
		return nil
	}
	out := *record
	out.Labels = cloneStringMap(record.Labels)
	out.Annotations = cloneStringMap(record.Annotations)
	out.HostPorts = append(json.RawMessage(nil), record.HostPorts...)
	if record.RuntimeNetwork != nil {
		network := *record.RuntimeNetwork
		out.RuntimeNetwork = &network
	}
	out.CNIRecord = append(json.RawMessage(nil), record.CNIRecord...)
	return &out
}

// legacyNetworkState 仅识别旧字段，拒绝带资源的旧状态；不会迁移或清理宿主网络。
func legacyNetworkState(record podRecord) bool {
	cni := string(record.CNIRecord)
	ports := string(record.HostPorts)
	return cni != "" && cni != "null" || ports != "" && ports != "null" && ports != "[]"
}
