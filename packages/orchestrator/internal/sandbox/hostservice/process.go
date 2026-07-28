package hostservice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapio"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type procEntry struct {
	service Service
	cmd     *exec.Cmd
	done    chan struct{}
	waitMu  sync.Mutex
	waitErr error
}

func startService(ctx context.Context, svc Service) (*procEntry, error) {
	cmd := exec.CommandContext(ctx, svc.Binary, svc.Args...)
	cmd.Env = append(os.Environ(), svc.Env...)
	cmd.ExtraFiles = svc.ExtraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	svcLogger := logger.L().Detach(ctx).With(zap.String("service", svc.Name))
	stdoutW := &zapio.Writer{Log: svcLogger, Level: zap.InfoLevel}
	stderrW := &zapio.Writer{Log: svcLogger, Level: zap.ErrorLevel}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start service %s: %w", svc.Name, err)
	}

	entry := &procEntry{
		service: svc,
		cmd:     cmd,
		done:    make(chan struct{}),
	}

	go func() {
		defer stdoutW.Close()
		defer stderrW.Close()
		waitErr := cmd.Wait()
		entry.waitMu.Lock()
		entry.waitErr = waitErr
		entry.waitMu.Unlock()
		close(entry.done)
	}()

	logger.L().Info(ctx, "host service started",
		zap.String("service", svc.Name),
		zap.Int("pid", cmd.Process.Pid),
	)
	return entry, nil
}

func (entry *procEntry) getWaitErr() error {
	entry.waitMu.Lock()
	defer entry.waitMu.Unlock()
	return entry.waitErr
}

func stopProc(ctx context.Context, entry *procEntry, gracePeriodSec int) error {
	if entry == nil || entry.cmd == nil || entry.cmd.Process == nil {
		return nil
	}

	pid := entry.cmd.Process.Pid

	_ = syscall.Kill(-pid, syscall.SIGCONT)

	_ = entry.cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-entry.done:
		return nil
	case <-time.After(time.Duration(gracePeriodSec) * time.Second):
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("SIGKILL service: %w", err)
	}

	<-entry.done
	return nil
}

func suspendProc(entry *procEntry) error {
	if entry == nil || entry.cmd == nil || entry.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-entry.cmd.Process.Pid, syscall.SIGSTOP)
}

func resumeProc(entry *procEntry) error {
	if entry == nil || entry.cmd == nil || entry.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-entry.cmd.Process.Pid, syscall.SIGCONT)
}

const (
	maxRestarts          = 5
	restartBackoffStart  = 1 * time.Second
	restartBackoffMax    = 16 * time.Second
	successResetInterval = 30 * time.Second
)

func monitorAndRestart(ctx context.Context, entry *procEntry, restart func(context.Context, *procEntry) (*procEntry, error)) {
	restartCount := 0
	lastStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-entry.done:
			waitErr := entry.getWaitErr()
			if ctx.Err() != nil {
				return
			}

			if waitErr != nil {
				logger.L().Warn(ctx, "host service exited",
					zap.String("service", entry.service.Name),
					zap.Error(waitErr),
				)
			} else {
				logger.L().Info(ctx, "host service exited cleanly",
					zap.String("service", entry.service.Name),
				)
				return
			}

			if entry.service.RestartPolicy != RestartOnCrash {
				return
			}

			if time.Since(lastStart) > successResetInterval {
				restartCount = 0
			}
			restartCount++

			if restartCount > maxRestarts {
				logger.L().Error(ctx, "host service exceeded max restarts",
					zap.String("service", entry.service.Name),
					zap.Int("max_restarts", maxRestarts),
				)
				return
			}

			backoff := restartBackoffStart << (restartCount - 1)
			if backoff > restartBackoffMax {
				backoff = restartBackoffMax
			}

			logger.L().Info(ctx, "restarting host service",
				zap.String("service", entry.service.Name),
				zap.Int("attempt", restartCount),
				zap.Duration("backoff", backoff),
			)

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			newEntry, err := restart(ctx, entry)
			if err != nil {
				logger.L().Error(ctx, "failed to restart host service",
					zap.String("service", entry.service.Name),
					zap.Error(err),
				)
				continue
			}
			entry = newEntry
			lastStart = time.Now()
		}
	}
}
