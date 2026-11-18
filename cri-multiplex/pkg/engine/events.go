package engine

import (
	"context"
	"sync"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const containerEventBuffer = 256

// ContainerEventSource 是可向 MuxServer 提供 CRI container events 的后端。
type ContainerEventSource interface {
	SubscribeContainerEvents(context.Context) (<-chan *runtime.ContainerEventResponse, error)
}

// EventBroker 将单个后端的生命周期事件分发给多个 GetContainerEvents 订阅者。
type EventBroker struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan *runtime.ContainerEventResponse
	closed      bool
}

func NewEventBroker() *EventBroker {
	return &EventBroker{subscribers: make(map[uint64]chan *runtime.ContainerEventResponse)}
}

func (b *EventBroker) Subscribe(ctx context.Context) (<-chan *runtime.ContainerEventResponse, error) {
	events := make(chan *runtime.ContainerEventResponse, containerEventBuffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(events)
		return events, nil
	}
	b.nextID++
	id := b.nextID
	b.subscribers[id] = events
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if current, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(current)
		}
		b.mu.Unlock()
	}()
	return events, nil
}

// Publish 保持调用方顺序。订阅者过慢时关闭该订阅，让客户端重连并避免静默丢事件。
func (b *EventBroker) Publish(event *runtime.ContainerEventResponse) {
	if event == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for id, subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
			delete(b.subscribers, id)
			close(subscriber)
		}
	}
}

func (b *EventBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, subscriber := range b.subscribers {
		delete(b.subscribers, id)
		close(subscriber)
	}
}
