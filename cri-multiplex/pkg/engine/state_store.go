package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type RouteRecord struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	Engine    EngineType `json:"engine"`
	UpdatedAt time.Time  `json:"updated_at"`
}

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

	HostIP       string        `json:"host_ip,omitempty"`
	HostPort     int           `json:"host_port,omitempty"`
	PodIP        string        `json:"pod_ip,omitempty"`
	CNIEnabled   bool          `json:"cni_enabled"`
	CNIRecord    *CNIRecord    `json:"cni_record,omitempty"`
	PortMappings []PortMapping `json:"port_mappings,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

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

	CNIEnabled   bool       `json:"cni_enabled"`
	CNIRecord    *CNIRecord `json:"cni_record,omitempty"`
	GuestIP      string     `json:"guest_ip,omitempty"`
	GuestGateway string     `json:"guest_gateway,omitempty"`
	GuestPrefix  string     `json:"guest_prefix,omitempty"`
	TapName      string     `json:"tap_name,omitempty"`

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

type persistedE2BState struct {
	Pods   []E2BPodState `json:"pods,omitempty"`
	Images []string      `json:"images,omitempty"`
}

type persistedAndroidState struct {
	Pods []AndroidPodState `json:"pods,omitempty"`
}

type persistedState struct {
	Version int                   `json:"version"`
	Routes  []RouteRecord         `json:"routes,omitempty"`
	E2B     persistedE2BState     `json:"e2b"`
	Android persistedAndroidState `json:"android"`
}

type StateStore interface {
	SaveRoute(RouteRecord) error
	DeleteRoute(kind, id string) error
	LoadRoutes() ([]RouteRecord, error)

	SaveE2BPod(E2BPodState) error
	DeleteE2BPod(sandboxID string) error
	LoadE2BPods() ([]E2BPodState, error)

	SaveE2BImage(imageRef string) error
	DeleteE2BImage(imageRef string) error
	LoadE2BImages() ([]string, error)

	SaveAndroidPod(AndroidPodState) error
	DeleteAndroidPod(sandboxID string) error
	LoadAndroidPods() ([]AndroidPodState, error)
}

type JSONStateStore struct {
	mu       sync.Mutex
	path     string
	state    persistedState
	disabled bool
}

func NewJSONStateStore(dir string) (*JSONStateStore, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	store := &JSONStateStore{
		path: filepath.Join(dir, "state.json"),
		state: persistedState{
			Version: 1,
		},
	}
	if data, err := os.ReadFile(store.path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &store.state); err != nil {
			log.Printf("[state] warning: failed to load %s: %v", store.path, err)
			store.state = persistedState{Version: 1}
		}
	} else if err != nil && !os.IsNotExist(err) {
		log.Printf("[state] warning: failed to read %s: %v", store.path, err)
	}
	if store.state.Version == 0 {
		store.state.Version = 1
	}
	return store, nil
}

func (s *JSONStateStore) SaveRoute(r RouteRecord) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	r.UpdatedAt = time.Now()
	s.state.Routes = upsertRouteRecord(s.state.Routes, r)
	return s.flushLocked()
}

func (s *JSONStateStore) DeleteRoute(kind, id string) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	s.state.Routes = deleteRouteRecord(s.state.Routes, kind, id)
	return s.flushLocked()
}

func (s *JSONStateStore) LoadRoutes() ([]RouteRecord, error) {
	if s == nil || s.disabled {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRouteRecords(s.state.Routes), nil
}

func (s *JSONStateStore) SaveE2BPod(p E2BPodState) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	p.UpdatedAt = time.Now()
	s.state.E2B.Pods = upsertE2BPodState(s.state.E2B.Pods, p)
	return s.flushLocked()
}

func (s *JSONStateStore) DeleteE2BPod(sandboxID string) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	s.state.E2B.Pods = deleteE2BPodState(s.state.E2B.Pods, sandboxID)
	return s.flushLocked()
}

func (s *JSONStateStore) LoadE2BPods() ([]E2BPodState, error) {
	if s == nil || s.disabled {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneE2BPodStates(s.state.E2B.Pods), nil
}

func (s *JSONStateStore) SaveE2BImage(imageRef string) error {
	if s == nil || s.disabled || imageRef == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	s.state.E2B.Images = upsertString(s.state.E2B.Images, imageRef)
	sort.Strings(s.state.E2B.Images)
	return s.flushLocked()
}

func (s *JSONStateStore) DeleteE2BImage(imageRef string) error {
	if s == nil || s.disabled || imageRef == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	s.state.E2B.Images = deleteString(s.state.E2B.Images, imageRef)
	return s.flushLocked()
}

func (s *JSONStateStore) LoadE2BImages() ([]string, error) {
	if s == nil || s.disabled {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStrings(s.state.E2B.Images), nil
}

func (s *JSONStateStore) SaveAndroidPod(p AndroidPodState) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	p.UpdatedAt = time.Now()
	s.state.Android.Pods = upsertAndroidPodState(s.state.Android.Pods, p)
	return s.flushLocked()
}

func (s *JSONStateStore) DeleteAndroidPod(sandboxID string) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Version = 1
	s.state.Android.Pods = deleteAndroidPodState(s.state.Android.Pods, sandboxID)
	return s.flushLocked()
}

func (s *JSONStateStore) LoadAndroidPods() ([]AndroidPodState, error) {
	if s == nil || s.disabled {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAndroidPodStates(s.state.Android.Pods), nil
}

func (s *JSONStateStore) flushLocked() error {
	if s == nil || s.disabled {
		return nil
	}
	s.state.Version = 1
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func upsertRouteRecord(records []RouteRecord, r RouteRecord) []RouteRecord {
	out := make([]RouteRecord, 0, len(records)+1)
	replaced := false
	for _, item := range records {
		if item.Kind == r.Kind && item.ID == r.ID {
			out = append(out, r)
			replaced = true
		} else {
			out = append(out, item)
		}
	}
	if !replaced {
		out = append(out, r)
	}
	return out
}

func deleteRouteRecord(records []RouteRecord, kind, id string) []RouteRecord {
	out := make([]RouteRecord, 0, len(records))
	for _, item := range records {
		if item.Kind == kind && item.ID == id {
			continue
		}
		out = append(out, item)
	}
	return out
}

func upsertE2BPodState(records []E2BPodState, p E2BPodState) []E2BPodState {
	out := make([]E2BPodState, 0, len(records)+1)
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

func deleteE2BPodState(records []E2BPodState, sandboxID string) []E2BPodState {
	out := make([]E2BPodState, 0, len(records))
	for _, item := range records {
		if item.SandboxID == sandboxID {
			continue
		}
		out = append(out, item)
	}
	return out
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

func upsertString(values []string, v string) []string {
	for _, item := range values {
		if item == v {
			return values
		}
	}
	return append(values, v)
}

func deleteString(values []string, v string) []string {
	out := make([]string, 0, len(values))
	for _, item := range values {
		if item == v {
			continue
		}
		out = append(out, item)
	}
	return out
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneRouteRecords(values []RouteRecord) []RouteRecord {
	if len(values) == 0 {
		return nil
	}
	out := make([]RouteRecord, len(values))
	copy(out, values)
	return out
}

func cloneE2BPodStates(values []E2BPodState) []E2BPodState {
	if len(values) == 0 {
		return nil
	}
	out := make([]E2BPodState, len(values))
	for i, v := range values {
		out[i] = v
		out[i].Labels = copyStringMap(v.Labels)
		out[i].Annotations = copyStringMap(v.Annotations)
		out[i].ContainerLabels = copyStringMap(v.ContainerLabels)
		out[i].ContainerAnnotations = copyStringMap(v.ContainerAnnotations)
		out[i].ContainerCommand = append([]string(nil), v.ContainerCommand...)
		out[i].ContainerArgs = append([]string(nil), v.ContainerArgs...)
		out[i].PortMappings = append([]PortMapping(nil), v.PortMappings...)
		if v.CNIRecord != nil {
			rec := *v.CNIRecord
			rec.DNS = append([]string(nil), v.CNIRecord.DNS...)
			rec.ResultJSON = append([]byte(nil), v.CNIRecord.ResultJSON...)
			out[i].CNIRecord = &rec
		}
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
