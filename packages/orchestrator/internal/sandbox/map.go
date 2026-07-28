package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/e2b-dev/infra/packages/shared/pkg/smap"
)

type MapSubscriber interface {
	OnInsert(sandbox *Sandbox)
	OnRemove(sandboxID string)
}

type Map struct {
	sandboxes *smap.Map[*Sandbox]

	subs     []MapSubscriber
	subsLock sync.RWMutex
}

func (m *Map) Subscribe(subscriber MapSubscriber) {
	m.subsLock.Lock()
	defer m.subsLock.Unlock()

	m.subs = append(m.subs, subscriber)
}

func (m *Map) trigger(fn func(MapSubscriber)) {
	m.subsLock.RLock()
	defer m.subsLock.RUnlock()

	for _, subscriber := range m.subs {
		fn(subscriber)
	}
}

func (m *Map) Items() map[string]*Sandbox {
	return m.sandboxes.Items()
}

func (m *Map) Count() int {
	return m.sandboxes.Count()
}

func (m *Map) Get(sandboxID string) (*Sandbox, bool) {
	return m.sandboxes.Get(sandboxID)
}

func (m *Map) GetByHostPort(hostPort string) (*Sandbox, error) {
	reqIP, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, fmt.Errorf("error parsing remote address %s: %w", hostPort, err)
	}

	for _, sbx := range m.sandboxes.Items() {
		if sbx.Slot.HostIPString() == reqIP {
			return sbx, nil
		}
	}

	return nil, fmt.Errorf("sandbox with address %s not found", hostPort)
}

func (m *Map) Insert(sbx *Sandbox) {
	m.sandboxes.Insert(sbx.Runtime.SandboxID, sbx)

	go m.trigger(func(s MapSubscriber) {
		s.OnInsert(sbx)
	})
}

func (m *Map) InsertIfAbsent(sbx *Sandbox) bool {
	if !m.sandboxes.InsertIfAbsent(sbx.Runtime.SandboxID, sbx) {
		return false
	}

	go m.trigger(func(s MapSubscriber) {
		s.OnInsert(sbx)
	})

	return true
}

func (m *Map) Remove(sandboxID string) {
	m.sandboxes.Remove(sandboxID)

	go m.trigger(func(s MapSubscriber) {
		s.OnRemove(sandboxID)
	})
}

func (m *Map) RemoveByLifecycleID(sandboxID, lifecycleID string) {
	removed := m.sandboxes.RemoveCb(sandboxID, func(_ string, v *Sandbox, exists bool) bool {
		if !exists {
			return false
		}

		if v == nil {
			return false
		}

		return v.LifecycleID == lifecycleID
	})

	if removed {
		go m.trigger(func(s MapSubscriber) {
			s.OnRemove(sandboxID)
		})
	}
}

func NewSandboxesMap() *Map {
	return &Map{sandboxes: smap.New[*Sandbox]()}
}

// CloseAll stops and cleans up every sandbox currently in the map.
func (m *Map) CloseAll(ctx context.Context) error {
	items := m.Items()
	if len(items) == 0 {
		return nil
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs error
	)
	for sandboxID, sbx := range items {
		wg.Add(1)
		go func(sandboxID string, sbx *Sandbox) {
			defer wg.Done()
			if err := sbx.Close(ctx); err != nil {
				mu.Lock()
				errs = errors.Join(errs, fmt.Errorf("close sandbox %s: %w", sandboxID, err))
				mu.Unlock()
			}
			m.Remove(sandboxID)
		}(sandboxID, sbx)
	}
	wg.Wait()

	return errs
}
