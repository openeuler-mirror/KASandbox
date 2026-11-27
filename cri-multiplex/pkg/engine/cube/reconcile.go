package cube

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/cri-multiplex/pkg/engine"
)

// Reconcile reclaims Cubelet sandboxes that have no local CRI record so they
// cannot become permanent orphans after state loss (KASandbox #71).
func (e *Engine) Reconcile(ctx context.Context, opts engine.ReconcileOptions) error {
	if e == nil || !e.cfg.Enabled || e.client == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	listed, err := e.client.List(listCtx)
	if err != nil {
		log.Printf("[CubeEngine] reconcile list skipped: %v", err)
		return nil
	}

	known := e.knownCubeletIDs()
	now := time.Now()
	var (
		errs    []error
		reclaim []string
	)

	e.orphanMu.Lock()
	for id := range e.orphanSeen {
		if known[id] {
			delete(e.orphanSeen, id)
		}
	}
	for _, item := range listed {
		id := item.ID
		if id == "" || known[id] {
			continue
		}
		first, ok := e.orphanSeen[id]
		if !ok {
			e.orphanSeen[id] = now
			continue
		}
		if opts.GracePeriod > 0 && now.Sub(first) < opts.GracePeriod {
			continue
		}
		reclaim = append(reclaim, id)
	}
	e.orphanMu.Unlock()

	for _, id := range reclaim {
		if opts.DryRun {
			log.Printf("[CubeEngine] dry-run orphan cubelet sandbox=%s", id)
			continue
		}
		log.Printf("[CubeEngine] reclaiming orphan cubelet sandbox=%s", id)
		if err := e.destroyCubeletSandbox(ctx, id, "reconcile-orphan"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
