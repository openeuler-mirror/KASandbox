package engine

import "sync"

type podTracker struct {
	mu   sync.Mutex
	pods map[string]*podInfo
}

func newPodTracker() *podTracker {
	return &podTracker{pods: make(map[string]*podInfo)}
}

func (t *podTracker) Add(sandboxID string, info *podInfo) {
	t.mu.Lock()
	t.pods[sandboxID] = info
	t.mu.Unlock()
}

func (t *podTracker) Get(sandboxID string) (*podInfo, bool) {
	t.mu.Lock()
	p, ok := t.pods[sandboxID]
	t.mu.Unlock()
	return p, ok
}

func (t *podTracker) Delete(sandboxID string) {
	t.mu.Lock()
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

