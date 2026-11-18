package agentenv

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

// Reconcile 对齐 AgentENV 持久投影、Runtime、原生网络事实 与 RouteTable。
func (e *Engine) Reconcile(ctx context.Context, opts engine.ReconcileOptions) error {
	if e == nil {
		return nil
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if err := e.ensureClients(); err != nil {
		return err
	}
	response, err := e.lifecycle.List(ctx, &runtimeapi.SandboxListRequest{IncludePaused: true})
	if err != nil {
		return e.mapRuntimeError(err)
	}
	byNative := make(map[string]*runtimeapi.SandboxInfo, len(response.GetSandboxes()))
	byProduct := make(map[string]*runtimeapi.SandboxInfo, len(response.GetSandboxes()))
	for _, info := range response.GetSandboxes() {
		if info.GetNativeSandboxId() != "" {
			byNative[info.GetNativeSandboxId()] = info
		}
		if info.GetProductSandboxId() != "" {
			byProduct[info.GetProductSandboxId()] = info
		}
	}

	var errs []error
	now := time.Now()
	for _, record := range e.listPods() {
		if err := e.reconcilePod(ctx, record, byNative, byProduct, opts, now); err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", record.CRISandboxID, err))
		}
	}
	if err := e.reconcileOrphanRoutes(opts, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Engine) reconcilePod(ctx context.Context, record podRecord, byNative, byProduct map[string]*runtimeapi.SandboxInfo, opts engine.ReconcileOptions, now time.Time) error {
	if record.State == criStateCleanupPending {
		if opts.DryRun {
			log.Printf("[AgentENV] dry-run reclaim cleanup-pending cri=%s native=%s",
				record.CRISandboxID, record.NativeSandboxID)
			return nil
		}
		return e.rollbackPodCreate(ctx, record)
	}

	nativeID := record.NativeSandboxID
	info := byNative[nativeID]
	if nativeID == "" && record.ProductSandboxID != "" {
		if found := byProduct[record.ProductSandboxID]; found != nil {
			nativeID = found.GetNativeSandboxId()
			info = found
		}
	}
	if nativeID == "" || info == nil {
		if !pastAgentENVGrace(record, opts.GracePeriod, now) {
			return nil
		}
		if opts.DryRun {
			log.Printf("[AgentENV] dry-run reclaim expired sandbox cri=%s native=%s state=%s",
				record.CRISandboxID, record.NativeSandboxID, record.State)
			return nil
		}
		return e.rollbackPodCreate(ctx, record)
	}

	if record.NativeSandboxID != nativeID {
		record.NativeSandboxID = nativeID
		if err := e.putPod(record); err != nil {
			return fmt.Errorf("persist recovered native id: %w", err)
		}
	}
	if info.GetPhase() != runtimeapi.SandboxPhase_RUNNING {
		return e.markPodNotReady(record, fmt.Sprintf("runtime phase is %s", info.GetPhase()))
	}

	if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
		return e.markPodNotReady(record, err.Error())
	}
	networkReady, err := e.podNetworkReady(record)
	if err != nil {
		return e.markPodNotReady(record, err.Error())
	}
	if !networkReady {
		reason := "Runtime network is unavailable"
		if pastAgentENVGrace(record, opts.GracePeriod, now) && !opts.DryRun {
			if cleanupErr := e.rollbackPodCreate(ctx, record); cleanupErr != nil {
				return errors.Join(errors.New(reason), cleanupErr)
			}
			return nil
		}
		return e.markPodNotReady(record, reason)
	}
	if err := e.ensurePodRoute(record); err != nil {
		return e.markPodNotReady(record, err.Error())
	}
	record.State = criStateReady
	record.LastError = ""
	return e.putPod(record)
}

func (e *Engine) markPodNotReady(record podRecord, reason string) error {
	if record.State == criStateNotReady && record.LastError == reason {
		return nil
	}
	record.State = criStateNotReady
	record.LastError = reason
	return e.putPod(record)
}

func (e *Engine) reconcileOrphanRoutes(opts engine.ReconcileOptions, now time.Time) error {
	pods := e.listPods()
	knownCRI := make(map[string]bool, len(pods))
	knownProduct := make(map[string]bool, len(pods))
	knownNative := make(map[string]bool, len(pods))
	for _, record := range pods {
		knownCRI[record.CRISandboxID] = true
		knownProduct[record.ProductSandboxID] = true
		knownNative[record.NativeSandboxID] = true
	}
	var errs []error
	for _, route := range e.routes.Snapshot() {
		if route.Engine != engine.EngineTypeAgentENV {
			continue
		}
		if knownCRI[route.CRISandboxID] || knownProduct[route.ProductSandboxID] || knownNative[route.NativeSandboxID] {
			continue
		}
		if !route.UpdatedAt.IsZero() && route.UpdatedAt.Add(opts.GracePeriod).After(now) {
			continue
		}
		id := route.CRISandboxID
		if id == "" {
			id = route.ID
		}
		if opts.DryRun {
			log.Printf("[AgentENV] dry-run delete orphan route id=%s", id)
			continue
		}
		if err := e.routes.DeleteSandbox(id); err != nil {
			errs = append(errs, fmt.Errorf("delete orphan route %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func pastAgentENVGrace(record podRecord, grace time.Duration, now time.Time) bool {
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = record.CreateStartedAt
	}
	if updatedAt.IsZero() {
		updatedAt = record.CreatedAt
	}
	return updatedAt.IsZero() || !updatedAt.Add(grace).After(now)
}
