package hostservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start service %s: %w", svc.Name, err)
	}

	binary := svc.Binary
	args := svc.Args
	if svc.NetNSName != "" {
		binary = "ip"
		args = append([]string{"netns", "exec", svc.NetNSName, svc.Binary}, svc.Args...)
	}
	cmd := exec.Command(binary, args...)
	mergedEnv, err := mergeProcessEnv(os.Environ(), svc.Env)
	if err != nil {
		return nil, fmt.Errorf("merge service %s environment: %w", svc.Name, err)
	}
	cmd.Env = mergedEnv
	cmd.ExtraFiles = svc.ExtraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	svcLogger := logger.L().Detach(ctx).With(zap.String("service", svc.Name))
	stdoutW := &zapio.Writer{Log: svcLogger, Level: zap.InfoLevel}
	stderrW := &zapio.Writer{Log: svcLogger, Level: zap.ErrorLevel}
	closeWriters := true
	defer func() {
		if closeWriters {
			_ = stdoutW.Close()
			_ = stderrW.Close()
		}
	}()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start service %s: %w", svc.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start service %s: %w", svc.Name, err)
	}
	closeWriters = false

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

	return entry, nil
}

var processEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mergeProcessEnv(parent, service []string) ([]string, error) {
	values := make(map[string]string, len(parent)+len(service))
	order := make([]string, 0, len(parent)+len(service))
	merge := func(items []string, rejectInvalid bool) error {
		for _, item := range items {
			key, value, ok := strings.Cut(item, "=")
			if !ok || !processEnvKeyPattern.MatchString(key) {
				if rejectInvalid {
					return fmt.Errorf("invalid environment entry %q", item)
				}
				continue
			}
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
		return nil
	}
	if err := merge(parent, false); err != nil {
		return nil, err
	}
	if err := merge(service, true); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func (entry *procEntry) getWaitErr() error {
	entry.waitMu.Lock()
	defer entry.waitMu.Unlock()
	return entry.waitErr
}

const killWaitTimeout = 10 * time.Second

func stopProc(_ context.Context, entry *procEntry, gracePeriodSec int) error {
	if entry == nil || entry.cmd == nil || entry.cmd.Process == nil {
		return nil
	}

	pid := entry.cmd.Process.Pid
	var signalErr error

	if err := syscall.Kill(-pid, syscall.SIGCONT); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		signalErr = errors.Join(signalErr, fmt.Errorf("SIGCONT service process group: %w", err))
	}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return signalErr
		}
		return errors.Join(signalErr, fmt.Errorf("SIGTERM service process group: %w", err))
	}

	timer := time.NewTimer(time.Duration(gracePeriodSec) * time.Second)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-entry.done:
		return signalErr
	case <-timer.C:
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return signalErr
		}
		return errors.Join(signalErr, fmt.Errorf("SIGKILL service process group: %w", err))
	}

	killTimer := time.NewTimer(killWaitTimeout)
	defer killTimer.Stop()
	select {
	case <-entry.done:
		return signalErr
	case <-killTimer.C:
		return errors.Join(signalErr, fmt.Errorf("timed out after %s waiting for service process group after SIGKILL", killWaitTimeout))
	}
}

const (
	maxRestarts          = 3
	restartBackoffStart  = 1 * time.Second
	restartBackoffMax    = 16 * time.Second
	successResetInterval = 60 * time.Second
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

			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
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
