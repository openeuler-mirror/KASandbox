package stratovirt

import (
	"context"
	"regexp"
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

// SandboxFiles uses fc-{sandboxID}-{randomID}.sock for the shared VMM QMP
// socket naming convention. Requiring this argument prevents us from killing
// StratoVirt processes not managed by this orchestrator.
var orchestratorStratoVirtQMPRe = regexp.MustCompile(`-qmp\s+unix:\S*fc-[^/\s]+-[^/\s]+\.sock(?:,|\s|$)`)

// KillOrphanedProcesses terminates StratoVirt processes left behind by an
// interrupted or failed sandbox startup before NBD orphan reclamation runs.
func KillOrphanedProcesses(ctx context.Context, stratoVirtVersionsDir string) int {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		logger.L().Warn(ctx, "failed to list processes while killing orphaned stratovirt", zap.Error(err))

		return 0
	}

	var targets []*process.Process
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil || name != StratoVirtBinaryName {
			continue
		}
		cmdline, err := p.CmdlineWithContext(ctx)
		if err != nil || cmdline == "" || !isOrchestratorStratoVirtCmdline(cmdline, stratoVirtVersionsDir) {
			continue
		}
		targets = append(targets, p)
	}
	if len(targets) == 0 {
		return 0
	}

	logger.L().Warn(ctx, "killing orphaned stratovirt processes", zap.Int("count", len(targets)))
	for _, p := range targets {
		_ = p.SendSignalWithContext(ctx, syscall.SIGTERM)
	}

	deadline := time.Now().Add(orphanTermTimeout)
	for time.Now().Before(deadline) && anyStratoVirtAlive(ctx, targets) {
		time.Sleep(50 * time.Millisecond)
	}

	killed := 0
	for _, p := range targets {
		alive, err := p.IsRunningWithContext(ctx)
		if err == nil && !alive {
			killed++

			continue
		}
		if err := p.KillWithContext(ctx); err == nil {
			killed++
		}
	}

	waitDeadline := time.Now().Add(orphanKillWait)
	for time.Now().Before(waitDeadline) && anyStratoVirtAlive(ctx, targets) {
		time.Sleep(50 * time.Millisecond)
	}

	return killed
}

func isOrchestratorStratoVirtCmdline(cmdline, stratoVirtVersionsDir string) bool {
	if !strings.Contains(strings.ToLower(cmdline), StratoVirtBinaryName) {
		return false
	}
	versionsPrefix := strings.TrimRight(stratoVirtVersionsDir, "/") + "/"
	if stratoVirtVersionsDir == "" || !strings.Contains(cmdline, versionsPrefix) {
		return false
	}

	return orchestratorStratoVirtQMPRe.MatchString(cmdline)
}

func anyStratoVirtAlive(ctx context.Context, targets []*process.Process) bool {
	for _, p := range targets {
		alive, err := p.IsRunningWithContext(ctx)
		if err != nil || alive {
			return true
		}
	}

	return false
}
