package e2b

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

// Reconcile 把 e2b 后端的运行态与持久态对齐：运行时、路由、netns 与 HostPort 规则。
func (e *Engine) Reconcile(ctx context.Context, opts engine.ReconcileOptions) error {
	if e == nil {
		return nil
	}
	var errs []error
	if err := e.reconcileRuntimeState(ctx, opts); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileRouteTable(); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileOrphanNetNS(opts); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileHostPortRules(ctx, opts); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileRuntimeState 回收"已停止/已删除/ orchestrator 侧已消失"的 e2b sandbox。
func (e *Engine) reconcileRuntimeState(ctx context.Context, opts engine.ReconcileOptions) error {
	if e.stateStore == nil {
		return nil
	}
	states, err := e.loadPods()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		return nil
	}
	active, listErr := e.activeSandboxes(ctx)
	if listErr != nil {
		log.Printf("[GrpcE2BEngine] reconcile deferred: orchestrator list failed: %v", listErr)
		return nil
	}

	var errs []error
	for _, state := range states {
		if !pastGrace(state.UpdatedAt, opts.GracePeriod, e.currentTime()) {
			continue
		}
		reason := ""
		switch {
		case state.State == stateRemoved || state.State == stateStopped:
			reason = "non-active-state"
		case state.State == stateRunning && !active[state.E2BSandboxID] && !active[state.SandboxID]:
			reason = "missing-orchestrator-sandbox"
		default:
			continue
		}
		if err := e.CleanupSandbox(ctx, state.SandboxID, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) activeSandboxes(ctx context.Context) (map[string]bool, error) {
	active := map[string]bool{}
	if err := e.ensureConn(); err != nil {
		return active, err
	}
	resp, err := e.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return active, err
	}
	for id := range activeSandboxIDs(resp.Sandboxes) {
		active[id] = true
	}
	return active, nil
}

// reconcileRouteTable 只修复 e2b 自己的路由：补齐存活 pod 的路由，删除没有对应 state 的 e2b 路由。
// 绝不触碰其它后端（尤其 android）的路由。
func (e *Engine) reconcileRouteTable() error {
	if e.stateStore == nil {
		return nil
	}
	routes, err := e.stateStore.LoadRoutes()
	if err != nil {
		return err
	}
	pods, err := e.loadPods()
	if err != nil {
		return err
	}

	wanted := make(map[string]engine.RouteRecord)
	for _, pod := range pods {
		if pod.SandboxID == "" || pod.State == stateRemoved {
			continue
		}
		wanted["pod\x00"+pod.SandboxID] = engine.RouteRecord{Kind: "pod", ID: pod.SandboxID, Engine: engine.EngineTypeE2B}
		if pod.ContainerName != "" {
			key := "container\x00" + pod.SandboxID + "-c"
			wanted[key] = engine.RouteRecord{
				Kind:      "container",
				ID:        pod.SandboxID + "-c",
				Engine:    engine.EngineTypeE2B,
				SandboxID: pod.SandboxID,
			}
		}
	}

	present := make(map[string]bool, len(wanted))
	var errs []error
	for _, route := range routes {
		if route.Engine != engine.EngineTypeE2B {
			continue
		}
		key := route.Kind + "\x00" + route.ID
		if _, ok := wanted[key]; !ok {
			log.Printf("[GrpcE2BEngine] pruning orphan route kind=%s id=%s", route.Kind, route.ID)
			if err := e.stateStore.DeleteRoute(route.Kind, route.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		present[key] = true
	}
	for key, rec := range wanted {
		if present[key] {
			continue
		}
		if err := e.stateStore.SaveRoute(rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileOrphanNetNS 删除没有对应 sandbox 的 e2b netns，跳过 state 中活跃的与 pending 的。
func (e *Engine) reconcileOrphanNetNS(opts engine.ReconcileOptions) error {
	if !e.cniConfig.Enabled {
		return nil
	}
	active := map[string]bool{}
	if e.stateStore != nil {
		pods, err := e.loadPods()
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.CNIRecord == nil {
				continue
			}
			active[pod.CNIRecord.NetNSName] = true
			active[filepath.Base(pod.CNIRecord.NetNSPath)] = true
		}
	}

	dir := e.cniConfig.NetNSDir
	prefix := defaultString(e.cniConfig.NetNSPrefix, "e2b-")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || active[name] || e.isPendingNetNS(name) {
			continue
		}
		if opts.DryRun {
			log.Printf("[GrpcE2BEngine] dry-run orphan netns name=%s", name)
			continue
		}
		if err := e.hostOps.DeleteNamedNetNS(name); err != nil && !isCleanupNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileHostPortRules 只删除带 cri-multiplex:hostport: 归属注释、且不再对应任何存活 sandbox 的规则。
func (e *Engine) reconcileHostPortRules(ctx context.Context, opts engine.ReconcileOptions) error {
	active := map[string]bool{}
	if e.stateStore != nil {
		pods, err := e.loadPods()
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.HostIP == "" {
				continue
			}
			for _, mapping := range pod.PortMappings {
				active[util.HostPortRuleComment(e.nodeIP, mapping.HostPort, pod.HostIP, mapping.SandboxPort)] = true
			}
			if len(pod.PortMappings) == 0 && pod.HostPort > 0 {
				active[util.HostPortRuleComment(e.nodeIP, pod.HostPort, pod.HostIP, envdSandboxPort)] = true
			}
		}
	}

	var errs []error
	if e.hostOps.ListIPTables != nil {
		data, err := e.hostOps.ListIPTables(ctx)
		if err == nil {
			for _, rule := range util.ParseIPTablesOwnerRules(data) {
				if active[rule.Comment] {
					continue
				}
				if opts.DryRun {
					log.Printf("[GrpcE2BEngine] dry-run orphan iptables rule comment=%s", rule.Comment)
					continue
				}
				if err := e.hostOps.DeleteIPTables(ctx, rule); err != nil && !isCleanupNotFound(err) {
					errs = append(errs, err)
				}
			}
		} else if !isCommandUnavailable(err) {
			errs = append(errs, err)
		}
	}
	if e.hostOps.ListNFTables != nil {
		data, err := e.hostOps.ListNFTables(ctx)
		if err == nil {
			for _, rule := range util.ParseNFTablesOwnerRules(data) {
				if active[rule.Comment] {
					continue
				}
				if opts.DryRun {
					log.Printf("[GrpcE2BEngine] dry-run orphan nftables rule comment=%s", rule.Comment)
					continue
				}
				if err := e.hostOps.DeleteNFTables(ctx, rule); err != nil && !isCleanupNotFound(err) {
					errs = append(errs, err)
				}
			}
		} else if !isCommandUnavailable(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) currentTime() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

func pastGrace(updatedAt time.Time, grace time.Duration, now time.Time) bool {
	return updatedAt.IsZero() || !updatedAt.Add(grace).After(now)
}

func isCommandUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return errors.Is(err, os.ErrNotExist) || strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "operation not permitted") || strings.Contains(msg, "permission denied")
}
