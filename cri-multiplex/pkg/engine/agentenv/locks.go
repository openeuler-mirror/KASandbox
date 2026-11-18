package agentenv

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// keyedLockEntry 是一个可被多个等待者引用的按 key 锁。
// refs 由 keyedLockSet.mu 保护，token 用于支持等待时响应 context 取消。
type keyedLockEntry struct {
	token chan struct{}
	refs  int
}

// keyedLockSet 为不同 sandbox 提供互不阻塞的锁，同时保证同一 sandbox 串行。
type keyedLockSet struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

// lock 按排序后的 key 顺序获取全部锁，避免多 key 操作发生死锁。
func (s *keyedLockSet) lock(ctx context.Context, keys ...string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keys = normalizeLockKeys(keys)
	acquired := make([]struct {
		key   string
		entry *keyedLockEntry
	}, 0, len(keys))
	for _, key := range keys {
		entry, err := s.acquire(ctx, key)
		if err != nil {
			s.unlockEntries(acquired)
			return nil, err
		}
		acquired = append(acquired, struct {
			key   string
			entry *keyedLockEntry
		}{key: key, entry: entry})
	}
	return func() { s.unlockEntries(acquired) }, nil
}

func (s *keyedLockSet) acquire(ctx context.Context, key string) (*keyedLockEntry, error) {
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[string]*keyedLockEntry)
	}
	entry := s.entries[key]
	if entry == nil {
		entry = &keyedLockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		s.entries[key] = entry
	}
	entry.refs++
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		s.releaseRef(key, entry)
		return nil, ctx.Err()
	case <-entry.token:
		return entry, nil
	}
}

func (s *keyedLockSet) releaseRef(key string, entry *keyedLockEntry) {
	s.mu.Lock()
	entry.refs--
	if entry.refs == 0 && s.entries[key] == entry {
		delete(s.entries, key)
	}
	s.mu.Unlock()
}

func (s *keyedLockSet) unlockEntries(entries []struct {
	key   string
	entry *keyedLockEntry
}) {
	for i := len(entries) - 1; i >= 0; i-- {
		item := entries[i]
		item.entry.token <- struct{}{}
		s.releaseRef(item.key, item.entry)
	}
}

func normalizeLockKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// sandboxLockIDs 把调用方可能提供的 CRI、product 或 native ID 都纳入锁集合。
// 同一个 CRI/product 组合的不同操作会共享至少一个 key。
func (e *Engine) sandboxOperationIDs(sandboxID string) []string {
	if e == nil {
		return nil
	}
	ids := []string{sandboxID}
	if record, ok := e.getPod(sandboxID); ok {
		ids = append(ids, record.CRISandboxID, record.ProductSandboxID, record.NativeSandboxID)
	}
	if route, ok := e.routes.PodRoute(sandboxID); ok {
		ids = append(ids, route.CRISandboxID, route.ProductSandboxID, route.NativeSandboxID)
	}
	return ids
}

// lockSandbox 在 lifecycle barrier 内获取 sandbox 级锁。
// 不同 sandbox 可以并行，同一 sandbox 的创建、停止、删除和清理保持串行。
func (e *Engine) lockSandbox(ctx context.Context, ids ...string) (func(), error) {
	if e == nil {
		return func() {}, nil
	}
	e.lifecycleMu.RLock()
	unlock, err := e.sandboxOps.lock(ctx, ids...)
	if err != nil {
		e.lifecycleMu.RUnlock()
		return nil, err
	}
	return func() {
		unlock()
		e.lifecycleMu.RUnlock()
	}, nil
}
