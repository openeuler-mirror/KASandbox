package agentenv

import (
	"context"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

func (e *Engine) SubscribeContainerEvents(ctx context.Context) (<-chan *runtime.ContainerEventResponse, error) {
	if e.events == nil {
		e.events = engine.NewEventBroker()
	}
	return e.events.Subscribe(ctx)
}

func (e *Engine) publishContainerEvent(containerID string, eventType runtime.ContainerEventType) {
	record, ok := e.getContainer(containerID)
	if !ok {
		return
	}
	e.eventMu.Lock()
	if e.lastContainerEvent == nil {
		e.lastContainerEvent = make(map[string]runtime.ContainerEventType)
	}
	if previous, exists := e.lastContainerEvent[containerID]; exists && previous == eventType {
		e.eventMu.Unlock()
		return
	}
	e.lastContainerEvent[containerID] = eventType
	e.eventMu.Unlock()

	pod, ok := e.getPod(record.CRISandboxID)
	if !ok {
		return
	}
	containers := make([]*runtime.ContainerStatus, 0)
	for _, candidate := range e.listContainers() {
		if candidate.CRISandboxID != record.CRISandboxID {
			continue
		}
		if candidate.ContainerID == record.ContainerID || candidate.State != containerStateRemoved {
			containers = append(containers, containerStatusFromRecord(candidate))
		}
	}
	network := &runtime.PodSandboxNetworkStatus{}
	if podIP := podIPFromRecord(pod); podIP != "" {
		network.Ip = podIP
	}
	event := &runtime.ContainerEventResponse{
		ContainerId:        containerID,
		ContainerEventType: eventType,
		CreatedAt:          time.Now().UnixNano(),
		PodSandboxStatus: &runtime.PodSandboxStatus{
			Id:             pod.CRISandboxID,
			Metadata:       podMetadata(pod),
			State:          criState(pod.State),
			CreatedAt:      pod.CreatedAt.UnixNano(),
			Network:        network,
			Labels:         cloneStringMap(pod.Labels),
			Annotations:    cloneStringMap(pod.Annotations),
			RuntimeHandler: string(engine.EngineTypeAgentENV),
		},
		ContainersStatuses: containers,
	}
	e.events.Publish(event)
}
