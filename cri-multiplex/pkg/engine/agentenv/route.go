package agentenv

import (
	"fmt"
	"sync"
	"time"

	"github.com/cri-multiplex/pkg/engine"
)

// ID 对照仅供 AgentENV 适配使用；公共路由仍只保存 CRI ID 与后端。
type podRoute struct {
	Backend          engine.EngineType
	CRISandboxID     string
	ProductSandboxID string
	NativeSandboxID  string
	UpdatedAt        time.Time
}

type routeTable struct {
	mu      sync.Mutex
	store   engine.StateStore
	entries []podRoute
}

func newRouteTable(store engine.StateStore) *routeTable { return &routeTable{store: store} }

func (t *routeTable) Restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.store == nil {
		return nil
	}
	var entries []podRoute
	if _, err := t.store.LoadBackendState("agentenv.id_map", &entries); err != nil {
		return err
	}
	t.entries = entries
	return nil
}

func (t *routeTable) save() error {
	if t.store == nil {
		return nil
	}
	return t.store.SaveBackendState("agentenv.id_map", t.entries)
}

func (t *routeTable) PutPodRoute(route podRoute) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if route.CRISandboxID == "" && route.ProductSandboxID == "" && route.NativeSandboxID == "" {
		return fmt.Errorf("empty AgentENV identity")
	}
	matched := -1
	for i, old := range t.entries {
		if (route.CRISandboxID != "" && route.CRISandboxID == old.CRISandboxID) ||
			(route.ProductSandboxID != "" && route.ProductSandboxID == old.ProductSandboxID) ||
			(route.NativeSandboxID != "" && route.NativeSandboxID == old.NativeSandboxID) {
			if matched >= 0 {
				return fmt.Errorf("AgentENV identities belong to different sandboxes")
			}
			matched = i
		}
	}
	for i, old := range t.entries {
		if i == matched {
			continue
		}
		for _, id := range []string{route.CRISandboxID, route.ProductSandboxID, route.NativeSandboxID} {
			if id != "" && (id == old.CRISandboxID || id == old.ProductSandboxID || id == old.NativeSandboxID) {
				return fmt.Errorf("AgentENV identity is already in use")
			}
		}
	}
	for i, old := range t.entries {
		if (route.CRISandboxID != "" && old.CRISandboxID == route.CRISandboxID) || (route.ProductSandboxID != "" && old.ProductSandboxID == route.ProductSandboxID) || (route.NativeSandboxID != "" && old.NativeSandboxID == route.NativeSandboxID) {
			for _, pair := range [][2]string{{old.CRISandboxID, route.CRISandboxID}, {old.ProductSandboxID, route.ProductSandboxID}, {old.NativeSandboxID, route.NativeSandboxID}} {
				if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
					return fmt.Errorf("AgentENV identity conflict")
				}
			}
			if old.Backend != route.Backend {
				return fmt.Errorf("AgentENV backend conflict")
			}
			if route.CRISandboxID == "" {
				route.CRISandboxID = old.CRISandboxID
			}
			if route.ProductSandboxID == "" {
				route.ProductSandboxID = old.ProductSandboxID
			}
			if route.NativeSandboxID == "" {
				route.NativeSandboxID = old.NativeSandboxID
			}
			route.UpdatedAt = time.Now()
			t.entries[i] = route
			return t.save()
		}
	}
	route.UpdatedAt = time.Now()
	t.entries = append(t.entries, route)
	return t.save()
}

func (t *routeTable) lookup(id string, field int) (podRoute, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		return podRoute{}, false
	}
	for _, r := range t.entries {
		ids := []string{r.CRISandboxID, r.ProductSandboxID, r.NativeSandboxID}
		if field >= 0 && ids[field] == id {
			return r, true
		}
		if field < 0 {
			for _, value := range ids {
				if value == id {
					return r, true
				}
			}
		}
	}
	return podRoute{}, false
}
func (t *routeTable) PodRouteByCRI(id string) (podRoute, bool)     { return t.lookup(id, 0) }
func (t *routeTable) PodRouteByProduct(id string) (podRoute, bool) { return t.lookup(id, 1) }
func (t *routeTable) PodRouteByNative(id string) (podRoute, bool)  { return t.lookup(id, 2) }
func (t *routeTable) PodRoute(id string) (podRoute, bool)          { return t.lookup(id, -1) }
func (t *routeTable) BindPodRoute(product, cri string, backend engine.EngineType) (podRoute, bool, error) {
	r, ok := t.PodRouteByProduct(product)
	if !ok {
		return r, false, nil
	}
	if r.Backend != backend || (r.CRISandboxID != "" && r.CRISandboxID != cri) {
		return r, true, fmt.Errorf("AgentENV identity conflict")
	}
	r.CRISandboxID = cri
	err := t.PutPodRoute(r)
	return r, true, err
}
func (t *routeTable) DeleteSandbox(id string) error {
	if id == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]podRoute, 0, len(t.entries))
	for _, r := range t.entries {
		if r.CRISandboxID != id && r.ProductSandboxID != id && r.NativeSandboxID != id {
			out = append(out, r)
		}
	}
	t.entries = out
	return t.save()
}
func (t *routeTable) Snapshot() []podRoute {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]podRoute(nil), t.entries...)
}
