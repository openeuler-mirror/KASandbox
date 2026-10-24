package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RouteRecord struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	Engine    EngineType `json:"engine"`
	SandboxID string     `json:"sandbox_id,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type CleanupTask struct {
	ID           string     `json:"id"`
	Runtime      EngineType `json:"runtime"`
	SandboxID    string     `json:"sandbox_id"`
	ResourceType string     `json:"resource_type"`
	Reason       string     `json:"reason"`
	Attempts     int        `json:"attempts"`
	LastError    string     `json:"last_error,omitempty"`
	NextRetryAt  time.Time  `json:"next_retry_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// persistedState 的固定字段只有 version/routes/cleanup_tasks；其余顶层键全部原样保存在
// Sections 里（e2b、android、e2b_operations 等），保证旧版本回滚后仍能读到同样的布局。
type persistedState struct {
	Version      int
	Routes       []RouteRecord
	CleanupTasks []CleanupTask
	Sections     map[string]json.RawMessage
}

func (p persistedState) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(p.Sections)+3)
	for key, raw := range p.Sections {
		out[key] = raw
	}
	version, err := json.Marshal(p.Version)
	if err != nil {
		return nil, err
	}
	out["version"] = version
	if len(p.Routes) > 0 {
		raw, err := json.Marshal(p.Routes)
		if err != nil {
			return nil, err
		}
		out["routes"] = raw
	}
	if len(p.CleanupTasks) > 0 {
		raw, err := json.Marshal(p.CleanupTasks)
		if err != nil {
			return nil, err
		}
		out["cleanup_tasks"] = raw
	}
	return json.Marshal(out)
}

func (p *persistedState) UnmarshalJSON(data []byte) error {
	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var sections map[string]json.RawMessage
	for key, value := range raw {
		switch key {
		case "version":
			if err := json.Unmarshal(value, &p.Version); err != nil {
				return err
			}
		case "routes":
			if err := json.Unmarshal(value, &p.Routes); err != nil {
				return err
			}
		case "cleanup_tasks":
			if err := json.Unmarshal(value, &p.CleanupTasks); err != nil {
				return err
			}
		default:
			if sections == nil {
				sections = make(map[string]json.RawMessage, len(raw))
			}
			sections[key] = value
		}
	}
	p.Sections = sections
	return nil
}

type StateStore interface {
	SaveRoute(RouteRecord) error
	DeleteRoute(kind, id string) error
	LoadRoutes() ([]RouteRecord, error)

	SaveCleanupTask(CleanupTask) error
	DeleteCleanupTask(id string) error
	LoadCleanupTasks() ([]CleanupTask, error)

	// 通用后端状态 KV：点分路径只支持 1 段（顶层键）或 2 段（<section>.<key>）。
	SaveBackendState(path string, value any) error
	LoadBackendState(path string, dst any) (bool, error)
	DeleteBackendState(path string) error
}

type JSONStateStore struct {
	mu       sync.Mutex
	path     string
	state    persistedState
	disabled bool
	// Both fields are protected by mu. A single writer preserves snapshot
	// order while later mutations accumulate into the next pending batch.
	pending  *stateWriteBatch
	flushing bool
	// Set at construction; tests can wrap the real writer before starting calls.
	writeSnapshot func([]byte) error
}

type stateWriteBatch struct {
	done chan struct{}
	err  error // published by closing done
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
	store.writeSnapshot = func(data []byte) error { return writeStateSnapshot(store.path, data) }
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

func (s *JSONStateStore) SaveCleanupTask(task CleanupTask) error {
	if s == nil || s.disabled || task.ID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	s.state.CleanupTasks = upsertCleanupTask(s.state.CleanupTasks, task)
	return s.flushLocked()
}

func (s *JSONStateStore) DeleteCleanupTask(id string) error {
	if s == nil || s.disabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.CleanupTasks = deleteCleanupTask(s.state.CleanupTasks, id)
	return s.flushLocked()
}

func (s *JSONStateStore) LoadCleanupTasks() ([]CleanupTask, error) {
	if s == nil || s.disabled {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CleanupTask, len(s.state.CleanupTasks))
	copy(out, s.state.CleanupTasks)
	return out, nil
}

// splitBackendStatePath 把点分路径拆成 (section, key)；只支持 1 段或 2 段。
func splitBackendStatePath(path string) (string, string, error) {
	parts := strings.Split(path, ".")
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return "", "", fmt.Errorf("empty backend state path")
		}
		return parts[0], "", nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid backend state path %q", path)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("invalid backend state path %q: only <section> or <section>.<key> is supported", path)
	}
}

func (s *JSONStateStore) SaveBackendState(path string, value any) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveBackendStateLocked(path, value)
}

func (s *JSONStateStore) saveBackendStateLocked(path string, value any) error {
	if err := s.setBackendStateLocked(path, value); err != nil {
		return err
	}
	return s.flushLocked()
}

// setBackendStateLocked 只改内存，不落盘；调用方决定何时 flush。
func (s *JSONStateStore) setBackendStateLocked(path string, value any) error {
	section, key, err := splitBackendStatePath(path)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal backend state %s: %w", path, err)
	}
	s.state.Version = 1
	if s.state.Sections == nil {
		s.state.Sections = make(map[string]json.RawMessage)
	}
	if key == "" {
		s.state.Sections[section] = data
		return nil
	}

	children := make(map[string]json.RawMessage)
	if raw, ok := s.state.Sections[section]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &children); err != nil {
			return fmt.Errorf("decode backend section %s: %w", section, err)
		}
	}
	children[key] = data
	sectionData, err := json.Marshal(children)
	if err != nil {
		return fmt.Errorf("encode backend section %s: %w", section, err)
	}
	s.state.Sections[section] = sectionData
	return nil
}

// LoadBackendState 读取 "<section>" 或 "<section>.<key>"；不存在时返回 (false, nil)。
func (s *JSONStateStore) LoadBackendState(path string, dst any) (bool, error) {
	if s == nil || s.disabled {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadBackendStateLocked(path, dst)
}

func (s *JSONStateStore) loadBackendStateLocked(path string, dst any) (bool, error) {
	section, key, err := splitBackendStatePath(path)
	if err != nil {
		return false, err
	}
	raw, ok := s.state.Sections[section]
	if !ok || len(raw) == 0 {
		return false, nil
	}
	if key == "" {
		if err := json.Unmarshal(raw, dst); err != nil {
			return false, fmt.Errorf("decode backend state %s: %w", path, err)
		}
		return true, nil
	}
	var children map[string]json.RawMessage
	if err := json.Unmarshal(raw, &children); err != nil {
		return false, fmt.Errorf("decode backend section %s: %w", section, err)
	}
	value, ok := children[key]
	if !ok || len(value) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(value, dst); err != nil {
		return false, fmt.Errorf("decode backend state %s: %w", path, err)
	}
	return true, nil
}

// DeleteBackendState 删除 "<section>" 或 "<section>.<key>"；子对象空了就连顶层 section 一起删。
func (s *JSONStateStore) DeleteBackendState(path string) error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteBackendStateLocked(path)
}

func (s *JSONStateStore) deleteBackendStateLocked(path string) error {
	section, key, err := splitBackendStatePath(path)
	if err != nil {
		return err
	}
	if s.state.Sections == nil {
		return nil
	}
	if key == "" {
		if _, ok := s.state.Sections[section]; !ok {
			return nil
		}
		delete(s.state.Sections, section)
		return s.flushLocked()
	}

	raw, ok := s.state.Sections[section]
	if !ok || len(raw) == 0 {
		return nil
	}
	children := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &children); err != nil {
		return fmt.Errorf("decode backend section %s: %w", section, err)
	}
	if _, ok := children[key]; !ok {
		return nil
	}
	delete(children, key)
	if len(children) == 0 {
		delete(s.state.Sections, section)
		return s.flushLocked()
	}
	sectionData, err := json.Marshal(children)
	if err != nil {
		return fmt.Errorf("encode backend section %s: %w", section, err)
	}
	s.state.Sections[section] = sectionData
	return s.flushLocked()
}

// flushLocked must be called with mu held, after updating state. It releases
// mu while waiting for the snapshot covering this mutation, then reacquires
// it before returning. Each caller receives its own batch's write result.
// There is no timer or asynchronous acknowledgement: calls still wait for
// file sync, rename and directory sync; only concurrent writes are combined.
func (s *JSONStateStore) flushLocked() error {
	if s == nil || s.disabled {
		return nil
	}
	if s.pending == nil {
		s.pending = &stateWriteBatch{done: make(chan struct{})}
	}
	batch := s.pending
	if !s.flushing {
		s.flushing = true
		go s.flushBatches()
	}
	s.mu.Unlock()
	<-batch.done
	s.mu.Lock()
	return batch.err
}

func (s *JSONStateStore) flushBatches() {
	for {
		s.mu.Lock()
		batch := s.pending
		if batch == nil {
			s.flushing = false
			s.mu.Unlock()
			return
		}
		s.pending = nil
		s.state.Version = 1
		// Marshal while holding mu so later mutations cannot change this
		// snapshot. Disk I/O uses only the resulting independent byte slice.
		data, err := json.MarshalIndent(s.state, "", "  ")
		s.mu.Unlock()
		if err == nil {
			err = s.writeSnapshot(data)
		}
		batch.err = err
		close(batch.done)
	}
}

// writeStateSnapshot replaces the JSON atomically and synchronizes both the
// file and its directory before reporting success.
func writeStateSnapshot(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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

func upsertCleanupTask(tasks []CleanupTask, task CleanupTask) []CleanupTask {
	out := make([]CleanupTask, 0, len(tasks)+1)
	replaced := false
	for _, item := range tasks {
		if item.ID == task.ID {
			if task.CreatedAt.IsZero() {
				task.CreatedAt = item.CreatedAt
			}
			out = append(out, task)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, task)
	}
	return out
}

func deleteCleanupTask(tasks []CleanupTask, id string) []CleanupTask {
	out := make([]CleanupTask, 0, len(tasks))
	for _, task := range tasks {
		if task.ID != id {
			out = append(out, task)
		}
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

func (s *JSONStateStore) PathForTest() string {
	if s == nil {
		return ""
	}
	return s.path
}
