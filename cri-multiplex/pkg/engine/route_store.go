package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// RouteTable 是进程内的路由与三套 Sandbox ID 映射的唯一权威。
// 写路径先更新内存再持久化，因此持久化失败不会影响当前进程内路由。
type RouteTable struct {
	mu         sync.RWMutex
	store      StateStore
	pods       map[string]PodRoute
	containers map[string]containerRoute
	byCRI      map[string]string
	byProduct  map[string]string
	byNative   map[string]string
}

// PodRoute 保存 CRI、产品和 native runtime 三套 ID 的 Pod 级映射。
// 三条索引都可以为空，但同一个 Route 至少需要一个 ID。
type PodRoute struct {
	Backend          EngineType
	CRISandboxID     string
	ProductSandboxID string
	NativeSandboxID  string
}

type containerRoute struct {
	backend   EngineType
	sandboxID string
}

// NewRouteTable 创建路由表；store 为 nil 时只在内存维护。
func NewRouteTable(store StateStore) *RouteTable {
	return &RouteTable{
		store:      store,
		pods:       make(map[string]PodRoute),
		containers: make(map[string]containerRoute),
		byCRI:      make(map[string]string),
		byProduct:  make(map[string]string),
		byNative:   make(map[string]string),
	}
}

// SetPod 记录 pod sandbox 的后端归属。如果 Pod 已有 product/native 映射，
// 该方法只补全 backend，不覆盖已有 ID，供 Mux 在 backend 调用完成后落路由使用。
func (t *RouteTable) SetPod(id string, backend EngineType) error {
	return t.SetPodRoute(id, backend, "", "")
}

// SetPodRoute 记录 Pod 的三套 ID 映射。id 是 CRI PodSandbox ID；
// productID 是控制面产品 ID；nativeID 是低级运行时的权威 UUID。
func (t *RouteTable) SetPodRoute(id string, backend EngineType, productID, nativeID string) error {
	if id == "" {
		return errors.New("route table: empty pod sandbox id")
	}
	return t.PutPodRoute(PodRoute{
		Backend:          backend,
		CRISandboxID:     id,
		ProductSandboxID: productID,
		NativeSandboxID:  nativeID,
	})
}

// PutPodRoute 幂等写入 Pod 映射。已有任意一条 ID 索引时合并同一映射；
// 如果不同 ID 指向不同 Route，则拒绝写入，避免静默改写已有绑定。
func (t *RouteTable) PutPodRoute(route PodRoute) error {
	t.mu.Lock()
	key, merged, changed, err := t.putPodRouteLocked(route)
	rec := podRouteRecord(key, merged)
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return t.save(rec)
}

// PutPodRouteAndBackendState 在更新 Pod route 的同时写入一份 backend state。
// 支持组合写入的 StateStore 会用单个 snapshot 完成两个 mutation；否则退化为
// 原有 SaveRoute + SaveBackendState，保持对测试和第三方 store 的兼容性。
func (t *RouteTable) PutPodRouteAndBackendState(route PodRoute, path string, value any) error {
	t.mu.Lock()
	key, merged, changed, err := t.putPodRouteLocked(route)
	rec := podRouteRecord(key, merged)
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if t.store == nil {
		return nil
	}
	if store, ok := t.store.(routeAndBackendStateStore); ok {
		rec.UpdatedAt = time.Now()
		return store.SaveRouteAndBackendState(rec, path, value)
	}
	if changed {
		if err := t.save(rec); err != nil {
			return err
		}
	}
	return t.store.SaveBackendState(path, value)
}

// BindPodRoute 将 CRI ID 绑定到已有的 product/native 映射，并返回合并后的 Route。
func (t *RouteTable) BindPodRoute(productID, criID string, backend EngineType) (PodRoute, bool, error) {
	if productID == "" || criID == "" {
		return PodRoute{}, false, nil
	}
	t.mu.RLock()
	route, ok := t.podRouteByProductLocked(productID)
	t.mu.RUnlock()
	if !ok {
		return PodRoute{}, false, nil
	}
	route.CRISandboxID = criID
	if backend != "" {
		route.Backend = backend
	}
	if err := t.PutPodRoute(route); err != nil {
		return PodRoute{}, false, err
	}
	return route, true, nil
}

// PodRoute 按任一已登记 ID 返回 Pod 映射，优先按 CRI ID 查询。
func (t *RouteTable) PodRoute(id string) (PodRoute, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	key, ok := t.findPodKeyLocked(id)
	if !ok {
		return PodRoute{}, false
	}
	route, ok := t.pods[key]
	return route, ok
}

// PodRouteByCRI 按 CRI Sandbox ID 查询映射。
func (t *RouteTable) PodRouteByCRI(id string) (PodRoute, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.podRouteByCRILocked(id)
}

// PodRouteByProduct 按产品 Sandbox ID 查询映射。
func (t *RouteTable) PodRouteByProduct(id string) (PodRoute, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.podRouteByProductLocked(id)
}

// PodRouteByNative 按 native runtime UUID 查询映射。
func (t *RouteTable) PodRouteByNative(id string) (PodRoute, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.podRouteByNativeLocked(id)
}

func (t *RouteTable) podRouteByCRILocked(id string) (PodRoute, bool) {
	if id == "" {
		return PodRoute{}, false
	}
	key, ok := t.byCRI[id]
	if !ok {
		return PodRoute{}, false
	}
	route, ok := t.pods[key]
	return route, ok
}

func (t *RouteTable) podRouteByProductLocked(id string) (PodRoute, bool) {
	if id == "" {
		return PodRoute{}, false
	}
	key, ok := t.byProduct[id]
	if !ok {
		return PodRoute{}, false
	}
	route, ok := t.pods[key]
	return route, ok
}

func (t *RouteTable) podRouteByNativeLocked(id string) (PodRoute, bool) {
	if id == "" {
		return PodRoute{}, false
	}
	key, ok := t.byNative[id]
	if !ok {
		return PodRoute{}, false
	}
	route, ok := t.pods[key]
	return route, ok
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
	route, ok := t.PodRoute(id)
	return route.Backend, ok
}

// ContainerBackend 返回容器的后端。
func (t *RouteTable) ContainerBackend(id string) (EngineType, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	route, ok := t.containers[id]
	return route.backend, ok
}

// DeleteSandbox 删除 Pod 路由及其名下所有容器路由。
// sandboxID 可以是 CRI、product 或 native ID；旧容器记录没有 SandboxID 时，
// 继续使用 "<sandboxID>-" 前缀兜底识别。内存无论持久化是否成功都会清空。
func (t *RouteTable) DeleteSandbox(sandboxID string) error {
	if sandboxID == "" {
		return nil
	}

	var stale []RouteRecord
	t.mu.Lock()
	key, found := t.findPodKeyLocked(sandboxID)
	var pod PodRoute
	if found {
		pod = t.pods[key]
		delete(t.pods, key)
		stale = append(stale, podRouteRecord(key, pod))
	}
	t.rebuildPodIndexesLocked()

	targets := nonEmptyStrings(sandboxID, pod.CRISandboxID, pod.ProductSandboxID, pod.NativeSandboxID)
	for id, route := range t.containers {
		belongs := false
		for _, target := range targets {
			if route.sandboxID == target || strings.HasPrefix(id, target+"-") {
				belongs = true
				break
			}
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

	pods := make(map[string]PodRoute, len(records))
	containers := make(map[string]containerRoute, len(records))
	for _, rec := range records {
		switch rec.Kind {
		case "pod":
			key, route, err := podRouteFromRecord(rec)
			if err != nil {
				return err
			}
			if existing, ok := pods[key]; ok {
				merged, err := mergePodRoute(existing, route)
				if err != nil {
					return fmt.Errorf("route table: restore pod %s: %w", key, err)
				}
				route = merged
			}
			pods[key] = route
		case "container":
			containers[rec.ID] = containerRoute{backend: rec.Engine, sandboxID: rec.SandboxID}
		default:
			klog.Warningf("route table: ignoring unknown route kind %q (id=%s)", rec.Kind, rec.ID)
		}
	}

	t.mu.Lock()
	t.pods = pods
	t.containers = containers
	err = t.rebuildPodIndexesLocked()
	t.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// Snapshot 返回按 Kind、ID 排序的路由快照，便于排障与断言。
func (t *RouteTable) Snapshot() []RouteRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	records := make([]RouteRecord, 0, len(t.pods)+len(t.containers))
	for key, route := range t.pods {
		records = append(records, podRouteRecord(key, route))
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

func (t *RouteTable) putPodRouteLocked(route PodRoute) (string, PodRoute, bool, error) {
	if err := validatePodRoute(route); err != nil {
		return "", PodRoute{}, false, err
	}

	key, err := t.findExistingPodKeyLocked(route)
	if err != nil {
		return "", PodRoute{}, false, err
	}
	existing, existed := t.pods[key]
	if key != "" {
		merged, err := mergePodRoute(existing, route)
		if err != nil {
			return "", PodRoute{}, false, err
		}
		route = merged
	} else {
		key = podRouteKey(route)
	}
	if key == "" {
		return "", PodRoute{}, false, errors.New("route table: empty pod route key")
	}
	if existed && existing == route {
		return key, route, false, nil
	}

	for _, indexed := range []struct {
		id  string
		dst map[string]string
	}{
		{route.CRISandboxID, t.byCRI},
		{route.ProductSandboxID, t.byProduct},
		{route.NativeSandboxID, t.byNative},
	} {
		if indexed.id == "" {
			continue
		}
		if existingKey, ok := indexed.dst[indexed.id]; ok && existingKey != key {
			return "", PodRoute{}, false, fmt.Errorf("route table: id %s already belongs to another pod route", indexed.id)
		}
	}

	t.pods[key] = route
	if err := t.rebuildPodIndexesLocked(); err != nil {
		return "", PodRoute{}, false, err
	}
	return key, route, true, nil
}

func (t *RouteTable) findExistingPodKeyLocked(route PodRoute) (string, error) {
	keys := make(map[string]struct{})
	for _, indexed := range []struct {
		id  string
		dst map[string]string
	}{
		{route.CRISandboxID, t.byCRI},
		{route.ProductSandboxID, t.byProduct},
		{route.NativeSandboxID, t.byNative},
	} {
		if indexed.id == "" {
			continue
		}
		if key, ok := indexed.dst[indexed.id]; ok {
			keys[key] = struct{}{}
		}
	}
	if len(keys) > 1 {
		return "", errors.New("route table: ids resolve to different pod routes")
	}
	for key := range keys {
		return key, nil
	}
	return "", nil
}

func (t *RouteTable) findPodKeyLocked(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	if key, ok := t.byCRI[id]; ok {
		return key, true
	}
	if key, ok := t.byProduct[id]; ok {
		return key, true
	}
	if key, ok := t.byNative[id]; ok {
		return key, true
	}
	return "", false
}

func (t *RouteTable) rebuildPodIndexesLocked() error {
	byCRI := make(map[string]string, len(t.pods))
	byProduct := make(map[string]string, len(t.pods))
	byNative := make(map[string]string, len(t.pods))
	for key, route := range t.pods {
		for _, indexed := range []struct {
			id  string
			dst map[string]string
		}{
			{route.CRISandboxID, byCRI},
			{route.ProductSandboxID, byProduct},
			{route.NativeSandboxID, byNative},
		} {
			if indexed.id == "" {
				continue
			}
			if existingKey, ok := indexed.dst[indexed.id]; ok && existingKey != key {
				return fmt.Errorf("route table: duplicate sandbox id %s", indexed.id)
			}
			indexed.dst[indexed.id] = key
		}
	}
	t.byCRI = byCRI
	t.byProduct = byProduct
	t.byNative = byNative
	return nil
}

func validatePodRoute(route PodRoute) error {
	if route.Backend == "" {
		return errors.New("route table: empty backend")
	}
	if len(routeIDs(route)) == 0 {
		return errors.New("route table: CRI, product or native sandbox id is required")
	}
	return nil
}

func mergePodRoute(existing, incoming PodRoute) (PodRoute, error) {
	if err := mergeRouteField("backend", string(existing.Backend), string(incoming.Backend), func(value string) { existing.Backend = EngineType(value) }); err != nil {
		return PodRoute{}, err
	}
	if err := mergeRouteField("CRI sandbox id", existing.CRISandboxID, incoming.CRISandboxID, func(value string) { existing.CRISandboxID = value }); err != nil {
		return PodRoute{}, err
	}
	if err := mergeRouteField("product sandbox id", existing.ProductSandboxID, incoming.ProductSandboxID, func(value string) { existing.ProductSandboxID = value }); err != nil {
		return PodRoute{}, err
	}
	if err := mergeRouteField("native sandbox id", existing.NativeSandboxID, incoming.NativeSandboxID, func(value string) { existing.NativeSandboxID = value }); err != nil {
		return PodRoute{}, err
	}
	return existing, nil
}

func mergeRouteField(name, existing, incoming string, set func(string)) error {
	if incoming == "" {
		return nil
	}
	if existing != "" && existing != incoming {
		return fmt.Errorf("route table: %s conflict: existing %s, incoming %s", name, existing, incoming)
	}
	set(incoming)
	return nil
}

func podRouteFromRecord(rec RouteRecord) (string, PodRoute, error) {
	route := PodRoute{
		Backend:          rec.Engine,
		CRISandboxID:     rec.CRISandboxID,
		ProductSandboxID: rec.ProductSandboxID,
		NativeSandboxID:  rec.NativeSandboxID,
	}
	// 旧记录只有 ID 字段：当 ID 同时不是 product/native 时，按历史语义视为 CRI ID。
	if route.CRISandboxID == "" && rec.ID != route.ProductSandboxID && rec.ID != route.NativeSandboxID {
		route.CRISandboxID = rec.ID
	}
	if err := validatePodRoute(route); err != nil {
		return "", PodRoute{}, fmt.Errorf("route table: invalid pod record %s: %w", rec.ID, err)
	}
	key := rec.ID
	if key == "" {
		key = podRouteKey(route)
	}
	return key, route, nil
}

func podRouteKey(route PodRoute) string {
	for _, id := range []string{route.CRISandboxID, route.ProductSandboxID, route.NativeSandboxID} {
		if id != "" {
			return id
		}
	}
	return ""
}

func podRouteRecord(key string, route PodRoute) RouteRecord {
	return RouteRecord{
		Kind:             "pod",
		ID:               key,
		Engine:           route.Backend,
		CRISandboxID:     route.CRISandboxID,
		ProductSandboxID: route.ProductSandboxID,
		NativeSandboxID:  route.NativeSandboxID,
	}
}

func routeIDs(route PodRoute) []string {
	return nonEmptyStrings(route.CRISandboxID, route.ProductSandboxID, route.NativeSandboxID)
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (t *RouteTable) save(rec RouteRecord) error {
	if t.store == nil {
		return nil
	}
	rec.UpdatedAt = time.Now()
	return t.store.SaveRoute(rec)
}
