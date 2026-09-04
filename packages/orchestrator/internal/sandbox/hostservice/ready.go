package hostservice

import "context"

// ProcessAlive checks initial process liveness when no readiness protocol exists.
type ProcessAlive struct{}

func (r *ProcessAlive) Check(context.Context) error { return nil }

func (r *ProcessAlive) String() string { return "process-alive" }
