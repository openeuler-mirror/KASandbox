package android

import (
	"time"

	"github.com/cri-multiplex/pkg/engine/util"
)

type AndroidPodState struct {
	SandboxID       string              `json:"sandbox_id"`
	PodUID          string              `json:"pod_uid"`
	Name            string              `json:"name"`
	Namespace       string              `json:"namespace"`
	Labels          map[string]string   `json:"labels,omitempty"`
	Annotations     map[string]string   `json:"annotations,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	StartedAt       time.Time           `json:"started_at"`
	StoppedAt       time.Time           `json:"stopped_at"`
	State           androidSandboxState `json:"state"`
	ArtifactsDir    string              `json:"artifacts_dir"`
	WorkDir         string              `json:"work_dir"`
	InstanceID      string              `json:"instance_id"`
	BaseInstanceNum int                 `json:"base_instance_num"`
	NodeIP          string              `json:"node_ip"`
	ADBPort         int                 `json:"adb_port"`
	WebRTCPort      int                 `json:"webrtc_port"`
	LaunchPID       int                 `json:"launch_pid"`
	LaunchPGID      int                 `json:"launch_pgid"`
	LaunchLogPath   string              `json:"launch_log_path"`

	CNIEnabled   bool            `json:"cni_enabled"`
	CNIRecord    *util.CNIRecord `json:"cni_record,omitempty"`
	GuestIP      string          `json:"guest_ip,omitempty"`
	GuestGateway string          `json:"guest_gateway,omitempty"`
	GuestPrefix  string          `json:"guest_prefix,omitempty"`
	TapName      string          `json:"tap_name,omitempty"`

	ContainerID          string                `json:"container_id,omitempty"`
	ContainerName        string                `json:"container_name,omitempty"`
	ContainerAttempt     uint32                `json:"container_attempt"`
	ContainerImage       string                `json:"container_image,omitempty"`
	ContainerImageRef    string                `json:"container_image_ref,omitempty"`
	ContainerState       androidContainerState `json:"container_state"`
	ContainerCreatedAt   time.Time             `json:"container_created_at"`
	ContainerStartedAt   time.Time             `json:"container_started_at"`
	ContainerFinishedAt  time.Time             `json:"container_finished_at"`
	ContainerExitCode    int32                 `json:"container_exit_code"`
	ContainerLabels      map[string]string     `json:"container_labels,omitempty"`
	ContainerAnnotations map[string]string     `json:"container_annotations,omitempty"`
	LogPath              string                `json:"log_path,omitempty"`
	FullLogPath          string                `json:"full_log_path,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

func (e *Engine) loadPods() ([]AndroidPodState, error) {
	if e == nil || e.stateStore == nil {
		return nil, nil
	}
	var pods []AndroidPodState
	if _, err := e.stateStore.LoadBackendState("android.pods", &pods); err != nil {
		return nil, err
	}
	return pods, nil
}

func (e *Engine) savePod(p AndroidPodState) error {
	if e == nil || e.stateStore == nil {
		return nil
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	pods, err := e.loadPods()
	if err != nil {
		return err
	}
	p.UpdatedAt = time.Now()
	return e.stateStore.SaveBackendState("android.pods", upsertAndroidPodState(pods, p))
}

func (e *Engine) deletePod(sandboxID string) error {
	if e == nil || e.stateStore == nil {
		return nil
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	pods, err := e.loadPods()
	if err != nil {
		return err
	}
	return e.stateStore.SaveBackendState("android.pods", deleteAndroidPodState(pods, sandboxID))
}

func upsertAndroidPodState(records []AndroidPodState, p AndroidPodState) []AndroidPodState {
	out := make([]AndroidPodState, 0, len(records)+1)
	replaced := false
	for _, item := range records {
		if item.SandboxID == p.SandboxID {
			out = append(out, p)
			replaced = true
		} else {
			out = append(out, item)
		}
	}
	if !replaced {
		out = append(out, p)
	}
	return out
}

func deleteAndroidPodState(records []AndroidPodState, sandboxID string) []AndroidPodState {
	out := make([]AndroidPodState, 0, len(records))
	for _, item := range records {
		if item.SandboxID == sandboxID {
			continue
		}
		out = append(out, item)
	}
	return out
}

func cloneAndroidPodStates(values []AndroidPodState) []AndroidPodState {
	if len(values) == 0 {
		return nil
	}
	out := make([]AndroidPodState, len(values))
	for i, v := range values {
		out[i] = v
		out[i].Labels = copyStringMap(v.Labels)
		out[i].Annotations = copyStringMap(v.Annotations)
		out[i].ContainerLabels = copyStringMap(v.ContainerLabels)
		out[i].ContainerAnnotations = copyStringMap(v.ContainerAnnotations)
		if v.CNIRecord != nil {
			rec := *v.CNIRecord
			rec.DNS = append([]string(nil), v.CNIRecord.DNS...)
			rec.ResultJSON = append([]byte(nil), v.CNIRecord.ResultJSON...)
			out[i].CNIRecord = &rec
		}
	}
	return out
}

// PathForTest 暴露 state.json 路径给跨包测试（不使用内部字段）。

func androidPodStateFromRecords(pod *AndroidSandboxRecord, container *AndroidContainerRecord) AndroidPodState {
	if pod == nil {
		return AndroidPodState{}
	}
	state := AndroidPodState{
		SandboxID:       pod.CRISandboxID,
		PodUID:          pod.PodUID,
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		Labels:          copyStringMap(pod.Labels),
		Annotations:     copyStringMap(pod.Annotations),
		CreatedAt:       pod.CreatedAt,
		StartedAt:       pod.StartedAt,
		StoppedAt:       pod.StoppedAt,
		State:           pod.State,
		ArtifactsDir:    pod.ArtifactsDir,
		WorkDir:         pod.WorkDir,
		InstanceID:      pod.InstanceID,
		BaseInstanceNum: pod.BaseInstanceNum,
		NodeIP:          pod.NodeIP,
		ADBPort:         pod.ADBPort,
		WebRTCPort:      pod.WebRTCPort,
		LaunchPID:       pod.LaunchPID,
		LaunchPGID:      pod.LaunchPGID,
		LaunchLogPath:   pod.LaunchLogPath,
		CNIEnabled:      pod.CNIRecord != nil,
		CNIRecord:       cloneCNIRecord(pod.CNIRecord),
		GuestIP:         pod.GuestIP,
		GuestGateway:    pod.GuestGateway,
		GuestPrefix:     pod.GuestPrefix,
		TapName:         pod.TapName,
	}
	if container != nil {
		state.ContainerID = container.ContainerID
		state.ContainerName = container.Name
		state.ContainerAttempt = container.Attempt
		state.ContainerImage = container.Image
		state.ContainerImageRef = container.ImageRef
		state.ContainerState = container.State
		state.ContainerCreatedAt = container.CreatedAt
		state.ContainerStartedAt = container.StartedAt
		state.ContainerFinishedAt = container.FinishedAt
		state.ContainerExitCode = container.ExitCode
		state.ContainerLabels = copyStringMap(container.Labels)
		state.ContainerAnnotations = copyStringMap(container.Annotations)
		state.LogPath = container.LogPath
		state.FullLogPath = container.FullLogPath
	}
	return state
}

func androidSandboxRecordFromState(state AndroidPodState) *AndroidSandboxRecord {
	return &AndroidSandboxRecord{
		CRISandboxID:    state.SandboxID,
		PodUID:          state.PodUID,
		Name:            state.Name,
		Namespace:       state.Namespace,
		ArtifactsDir:    state.ArtifactsDir,
		WorkDir:         state.WorkDir,
		InstanceID:      state.InstanceID,
		BaseInstanceNum: state.BaseInstanceNum,
		NodeIP:          state.NodeIP,
		ADBPort:         state.ADBPort,
		WebRTCPort:      state.WebRTCPort,
		LaunchPID:       state.LaunchPID,
		LaunchPGID:      state.LaunchPGID,
		LaunchLogPath:   state.LaunchLogPath,
		CNIRecord:       cloneCNIRecord(state.CNIRecord),
		PodIP:           cniPodIP(state.CNIRecord),
		NetNSName:       cniNetNSName(state.CNIRecord),
		NetNSPath:       cniNetNSPath(state.CNIRecord),
		GuestIP:         state.GuestIP,
		GuestGateway:    state.GuestGateway,
		GuestPrefix:     state.GuestPrefix,
		TapName:         state.TapName,
		State:           state.State,
		CreatedAt:       state.CreatedAt,
		StartedAt:       state.StartedAt,
		StoppedAt:       state.StoppedAt,
		Labels:          copyStringMap(state.Labels),
		Annotations:     copyStringMap(state.Annotations),
	}
}

func androidContainerRecordFromState(state AndroidPodState) *AndroidContainerRecord {
	if state.ContainerID == "" {
		return nil
	}
	return &AndroidContainerRecord{
		ContainerID:  state.ContainerID,
		CRISandboxID: state.SandboxID,
		Name:         state.ContainerName,
		Attempt:      state.ContainerAttempt,
		Image:        state.ContainerImage,
		ImageRef:     state.ContainerImageRef,
		State:        state.ContainerState,
		CreatedAt:    state.ContainerCreatedAt,
		StartedAt:    state.ContainerStartedAt,
		FinishedAt:   state.ContainerFinishedAt,
		ExitCode:     state.ContainerExitCode,
		Labels:       copyStringMap(state.ContainerLabels),
		Annotations:  copyStringMap(state.ContainerAnnotations),
		LogPath:      state.LogPath,
		FullLogPath:  state.FullLogPath,
	}
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
