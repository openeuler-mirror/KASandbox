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
