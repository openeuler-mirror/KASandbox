package engine

import "sync"

type podTracker struct {
	mu    sync.Mutex
	pods  map[string]*podInfo
	byE2B map[string]string // e2bSandboxID -> CRI sandboxID 反向索引
}

func newPodTracker() *podTracker {
	return &podTracker{
		pods:  make(map[string]*podInfo),
		byE2B: make(map[string]string),
	}
}

func (t *podTracker) Add(sandboxID string, info *podInfo) {
	t.mu.Lock()
	if old, ok := t.pods[sandboxID]; ok && old.e2bSandboxID != "" {
		delete(t.byE2B, old.e2bSandboxID)
	}
	t.pods[sandboxID] = info
	if info != nil && info.e2bSandboxID != "" {
		t.byE2B[info.e2bSandboxID] = sandboxID
	}
	t.mu.Unlock()
}

func (t *podTracker) Get(sandboxID string) (*podInfo, bool) {
	t.mu.Lock()
	p, ok := t.pods[sandboxID]
	t.mu.Unlock()
	return p, ok
}

// GetByE2B 按 e2bSandboxID 反查 pod
func (t *podTracker) GetByE2B(e2bSandboxID string) (*podInfo, bool) {
	t.mu.Lock()
	sandboxID, ok := t.byE2B[e2bSandboxID]
	if !ok {
		t.mu.Unlock()
		return nil, false
	}
	p, ok := t.pods[sandboxID]
	t.mu.Unlock()
	return p, ok
}

func (t *podTracker) Delete(sandboxID string) {
	t.mu.Lock()
	if p, ok := t.pods[sandboxID]; ok && p.e2bSandboxID != "" {
		delete(t.byE2B, p.e2bSandboxID)
	}
	delete(t.pods, sandboxID)
	t.mu.Unlock()
}

// List 返回所有非 Removed 的 pod（用于 ListPodSandbox / ListContainers）
func (t *podTracker) List() []*podInfo {
	t.mu.Lock()
	out := make([]*podInfo, 0, len(t.pods))
	for _, v := range t.pods {
		if v.state != stateRemoved {
			out = append(out, v)
		}
	}
	t.mu.Unlock()
	return out
}
