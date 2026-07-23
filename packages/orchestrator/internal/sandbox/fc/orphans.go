package fc

import (
	"context"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	orphanTermTimeout = 1 * time.Second
	orphanKillWait    = 2 * time.Second
)

// KillOrphanedProcesses terminates leftover Firecracker processes that still
// hold /dev/nbdX open after abrupt sandbox/orchestrator stops.
func KillOrphanedProcesses(ctx context.Context) int {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		logger.L().Warn(ctx, "failed to list processes while killing orphaned firecracker", zap.Error(err))

		return 0
	}

	var targets []*process.Process
	for _, p := range procs {
		cmdline, err := p.CmdlineWithContext(ctx)
		if err != nil || cmdline == "" || !isFirecrackerCmdline(cmdline) {
			continue
		}
		targets = append(targets, p)
	}
	if len(targets) == 0 {
		return 0
	}

	logger.L().Warn(ctx, "killing orphaned firecracker processes", zap.Int("count", len(targets)))
	for _, p := range targets {
		_ = p.SendSignalWithContext(ctx, syscall.SIGTERM)
	}

	deadline := time.Now().Add(orphanTermTimeout)
	for time.Now().Before(deadline) && anyAlive(ctx, targets) {
		time.Sleep(50 * time.Millisecond)
	}

	killed := 0
	for _, p := range targets {
		alive, err := p.IsRunningWithContext(ctx)
		if err != nil || !alive {
			killed++

			continue
		}
		if err := p.KillWithContext(ctx); err == nil {
			killed++
		}
	}

	waitDeadline := time.Now().Add(orphanKillWait)
	for time.Now().Before(waitDeadline) && anyAlive(ctx, targets) {
		time.Sleep(50 * time.Millisecond)
	}

	return killed
}

func isFirecrackerCmdline(cmdline string) bool {
	return strings.Contains(strings.ToLower(cmdline), FirecrackerBinaryName) &&
		strings.Contains(cmdline, "--api-sock")
}

func anyAlive(ctx context.Context, targets []*process.Process) bool {
	for _, p := range targets {
		if alive, err := p.IsRunningWithContext(ctx); err == nil && alive {
			return true
		}
	}

	return false
}
