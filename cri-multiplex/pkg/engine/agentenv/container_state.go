package agentenv

import (
	"fmt"
	"sort"
	"time"
)

func validContainerState(state string) bool {
	switch state {
	case containerStateCreated, containerStateRunning, containerStateExited, containerStateRemoved:
		return true
	default:
		return false
	}
}

func normalizeContainerRecord(record *containerRecord) {
	if record == nil {
		return
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	record.Labels = cloneStringMap(record.Labels)
	record.Annotations = cloneStringMap(record.Annotations)
}

func cloneContainerRecord(record *containerRecord) *containerRecord {
	if record == nil {
		return nil
	}
	out := *record
	normalizeContainerRecord(&out)
	return &out
}

func (e *Engine) getContainer(containerID string) (containerRecord, bool) {
	if e == nil || containerID == "" {
		return containerRecord{}, false
	}
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	record, ok := e.containers[containerID]
	if !ok {
		return containerRecord{}, false
	}
	return *cloneContainerRecord(record), true
}

func (e *Engine) listContainers() []containerRecord {
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	out := make([]containerRecord, 0, len(e.containers))
	for _, record := range e.containers {
		out = append(out, *cloneContainerRecord(record))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ContainerID < out[j].ContainerID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (e *Engine) putContainer(record containerRecord) error {
	if record.ContainerID == "" || record.CRISandboxID == "" {
		return fmt.Errorf("agentenv containers: container and sandbox id are required")
	}
	if !validContainerState(record.State) {
		return fmt.Errorf("agentenv containers: invalid state %q", record.State)
	}
	normalizeContainerRecord(&record)
	e.podMu.Lock()
	defer e.podMu.Unlock()
	previous, existed := e.containers[record.ContainerID]
	e.containers[record.ContainerID] = &record
	if err := e.persistPodStateLocked(func() {
		if existed {
			e.containers[record.ContainerID] = previous
		} else {
			delete(e.containers, record.ContainerID)
		}
	}); err != nil {
		return fmt.Errorf("agentenv containers: persist %s: %w", record.ContainerID, err)
	}
	return nil
}

// mutateContainer 在一次持久化事务中更新容器投影。
func (e *Engine) mutateContainer(containerID string, mutate func(*containerRecord)) (containerRecord, bool, error) {
	e.podMu.Lock()
	defer e.podMu.Unlock()
	previous, ok := e.containers[containerID]
	if !ok {
		return containerRecord{}, false, nil
	}
	next := cloneContainerRecord(previous)
	mutate(next)
	next.UpdatedAt = time.Now()
	if !validContainerState(next.State) {
		return containerRecord{}, true, fmt.Errorf("agentenv containers: invalid state %q", next.State)
	}
	e.containers[containerID] = next
	if err := e.persistPodStateLocked(func() {
		e.containers[containerID] = previous
	}); err != nil {
		return containerRecord{}, true, fmt.Errorf("agentenv containers: persist %s: %w", containerID, err)
	}
	return *cloneContainerRecord(next), true, nil
}
