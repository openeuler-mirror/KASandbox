package hostservice

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	hostServiceOrphanTermTimeout = 1 * time.Second
	hostServiceOrphanKillWait    = 2 * time.Second
)

var androidHostServiceNames = map[string]struct{}{
	"config_server":      {},
	"modem_simulator":    {},
	"secure_env":         {},
	"socket_vsock_proxy": {},
}

// KillOrphanedProcesses terminates Android host services left by a previous
// orchestrator process. Only known binaries below CvdHostPackagesRoot are
// selected, so similarly named system processes are not touched.
func KillOrphanedProcesses(ctx context.Context, cvdHostPackagesRoot string) int {
	root := filepath.Clean(cvdHostPackagesRoot)
	if root == "." || root == string(filepath.Separator) {
		logger.L().Warn(ctx, "refusing to scan an unsafe CVD host packages root", zap.String("root", cvdHostPackagesRoot))
		return 0
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		logger.L().Warn(ctx, "failed to list processes while killing orphaned Android host services", zap.Error(err))
		return 0
	}

	var targets []*process.Process
	for _, p := range procs {
		name, nameErr := p.NameWithContext(ctx)
		executable, exeErr := p.ExeWithContext(ctx)
		if nameErr != nil || exeErr != nil || !isAndroidHostServiceProcess(name, executable, root) {
			continue
		}
		targets = append(targets, p)
	}
	if len(targets) == 0 {
		return 0
	}

	logger.L().Warn(ctx, "killing orphaned Android host services", zap.Int("count", len(targets)))
	for _, p := range targets {
		_ = p.SendSignalWithContext(ctx, syscall.SIGTERM)
	}
	waitForHostServiceOrphans(ctx, targets, hostServiceOrphanTermTimeout)

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
	waitForHostServiceOrphans(ctx, targets, hostServiceOrphanKillWait)

	return killed
}

func isAndroidHostServiceProcess(processName, executable, root string) bool {
	binaryName := filepath.Base(executable)
	if _, ok := androidHostServiceNames[binaryName]; !ok {
		return false
	}
	// Linux comm names are limited to 15 bytes, so socket_vsock_proxy may be
	// reported as socket_vsock_pr.
	expectedProcessName := binaryName
	if len(expectedProcessName) > 15 {
		expectedProcessName = expectedProcessName[:15]
	}
	if processName != binaryName && processName != expectedProcessName {
		return false
	}

	rel, err := filepath.Rel(root, filepath.Clean(executable))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 && strings.HasPrefix(parts[0], "android-") && parts[1] == "bin" && parts[2] == binaryName
}

func waitForHostServiceOrphans(ctx context.Context, targets []*process.Process, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range targets {
			running, err := p.IsRunningWithContext(ctx)
			if err != nil || running {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
