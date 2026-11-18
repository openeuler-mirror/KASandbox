package agentenv

import (
	"context"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func nextContainerEvent(t *testing.T, events <-chan *runtime.ContainerEventResponse) *runtime.ContainerEventResponse {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("container event channel closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for container event")
		return nil
	}
}

func TestContainerLifecyclePublishesOrderedEvents(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := instance.SubscribeContainerEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeContainerEvents: %v", err)
	}
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "event-app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	createdEvent := nextContainerEvent(t, events)
	if createdEvent.GetContainerEventType() != runtime.ContainerEventType_CONTAINER_CREATED_EVENT {
		t.Fatalf("created event type = %v", createdEvent.GetContainerEventType())
	}

	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	startedEvent := nextContainerEvent(t, events)
	if startedEvent.GetContainerEventType() != runtime.ContainerEventType_CONTAINER_STARTED_EVENT {
		t.Fatalf("started event type = %v", startedEvent.GetContainerEventType())
	}
	if startedEvent.GetPodSandboxStatus() == nil || len(startedEvent.GetContainersStatuses()) == 0 {
		t.Fatalf("started event missing status payload: %+v", startedEvent)
	}

	if _, err := instance.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	stoppedEvent := nextContainerEvent(t, events)
	if stoppedEvent.GetContainerEventType() != runtime.ContainerEventType_CONTAINER_STOPPED_EVENT {
		t.Fatalf("stopped event type = %v", stoppedEvent.GetContainerEventType())
	}

	if _, err := instance.RemoveContainer(context.Background(), &runtime.RemoveContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	deletedEvent := nextContainerEvent(t, events)
	if deletedEvent.GetContainerEventType() != runtime.ContainerEventType_CONTAINER_DELETED_EVENT {
		t.Fatalf("deleted event type = %v", deletedEvent.GetContainerEventType())
	}
}
