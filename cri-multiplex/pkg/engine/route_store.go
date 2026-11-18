package engine

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// RouteTable 是进程内的路由索引：把 CRI 的 pod/container ID 映射到后端类型。
// 写路径先更新内存再持久化，所以持久化失败不影响进程内路由。
type RouteTable struct {
	mu         sync.RWMutex
	store      StateStore
	pods       map[string]EngineType
	containers map[string]containerRoute
}

type containerRoute struct {
	backend   EngineType
	sandboxID string
}

// NewRouteTable 创建路由表；store 为 nil 时只在内存维护。
func NewRouteTable(store StateStore) *RouteTable {
	return &RouteTable{
		store:      store,
		pods:       make(map[string]EngineType),
		containers: make(map[string]containerRoute),
	}
}

// SetPod 记录 pod sandbox 的后端归属。
func (t *RouteTable) SetPod(id string, backend EngineType) error {
	if id == "" {
		return errors.New("route table: empty pod sandbox id")
	}

	t.mu.Lock()
	t.pods[id] = backend
	t.mu.Unlock()

	return t.save(RouteRecord{Kind: "pod", ID: id, Engine: backend})
}

// SetContainer 记录容器归属；sandboxID 是父 sandbox 的 CRI ID，供按 sandbox 删除使用。
func (t *RouteTable) SetContainer(id, sandboxID string, backend EngineType) error {
	if id == "" {
		return errors.New("route table: empty container id")
	}

	t.mu.Lock()
	t.containers[id] = containerRoute{backend: backend, sandboxID: sandboxID}
	t.mu.Unlock()

	return t.save(RouteRecord{Kind: "container", ID: id, Engine: backend, SandboxID: sandboxID})
}

// PodBackend 返回 pod sandbox 的后端。
func (t *RouteTable) PodBackend(id string) (EngineType, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	backend, ok := t.pods[id]

	return backend, ok
}

// ContainerBackend 返回容器的后端。
func (t *RouteTable) ContainerBackend(id string) (EngineType, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	route, ok := t.containers[id]

	return route.backend, ok
}

// DeleteSandbox 删除 pod 路由与它名下所有容器路由。
// 旧记录没有 SandboxID，用 "<sandboxID>-" 前缀兜底识别。
// 内存无论如何都会清空，持久化失败以 errors.Join 返回。
func (t *RouteTable) DeleteSandbox(sandboxID string) error {
	if sandboxID == "" {
		return nil
	}

	var stale []RouteRecord

	t.mu.Lock()
	if backend, ok := t.pods[sandboxID]; ok {
		delete(t.pods, sandboxID)
		stale = append(stale, RouteRecord{Kind: "pod", ID: sandboxID, Engine: backend})
	}
	for id, route := range t.containers {
		belongs := route.sandboxID == sandboxID
		if route.sandboxID == "" {
			belongs = strings.HasPrefix(id, sandboxID+"-")
		}
		if !belongs {
			continue
		}
		delete(t.containers, id)
		stale = append(stale, RouteRecord{Kind: "container", ID: id, Engine: route.backend, SandboxID: route.sandboxID})
	}
	t.mu.Unlock()

	if t.store == nil {
		return nil
	}

	var errs []error
	for _, rec := range stale {
		if err := t.store.DeleteRoute(rec.Kind, rec.ID); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// DeleteContainer 删除单个容器路由（父 sandbox 仍在时使用）。
func (t *RouteTable) DeleteContainer(id string) error {
	if id == "" {
		return nil
	}

	t.mu.Lock()
	_, existed := t.containers[id]
	delete(t.containers, id)
	t.mu.Unlock()

	if !existed || t.store == nil {
		return nil
	}

	return t.store.DeleteRoute("container", id)
}

// Restore 从持久化状态重建路由表；只读取，不派生、不修复。
func (t *RouteTable) Restore() error {
	if t.store == nil {
		return nil
	}

	records, err := t.store.LoadRoutes()
	if err != nil {
		return err
	}

	pods := make(map[string]EngineType, len(records))
	containers := make(map[string]containerRoute, len(records))
	for _, rec := range records {
		switch rec.Kind {
		case "pod":
			pods[rec.ID] = rec.Engine
		case "container":
			containers[rec.ID] = containerRoute{backend: rec.Engine, sandboxID: rec.SandboxID}
		default:
			klog.Warningf("route table: ignoring unknown route kind %q (id=%s)", rec.Kind, rec.ID)
		}
	}

	t.mu.Lock()
	t.pods = pods
	t.containers = containers
	t.mu.Unlock()

	return nil
}

// Snapshot 返回按 Kind、ID 排序的路由快照，便于排障与断言。
func (t *RouteTable) Snapshot() []RouteRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	records := make([]RouteRecord, 0, len(t.pods)+len(t.containers))
	for id, backend := range t.pods {
		records = append(records, RouteRecord{Kind: "pod", ID: id, Engine: backend})
	}
	for id, route := range t.containers {
		records = append(records, RouteRecord{Kind: "container", ID: id, Engine: route.backend, SandboxID: route.sandboxID})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		return records[i].ID < records[j].ID
	})

	return records
}

func (t *RouteTable) save(rec RouteRecord) error {
	if t.store == nil {
		return nil
	}
	rec.UpdatedAt = time.Now()

	return t.store.SaveRoute(rec)
}
