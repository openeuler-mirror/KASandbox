package stratovirt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
)

const stratoVirtNetnsFDEnv = "E2B_STRATOVIRT_NETNS_FD"
const stratoVirtMountnsFDEnv = "E2B_STRATOVIRT_MOUNTNS_FD"

var pinnedMountNS struct {
	once        sync.Once
	file        *os.File
	err         error
	cmd         *exec.Cmd
	keepOpen    *os.File // 管道写端，编排器存活期间保持打开
	prepareVMOK bool     // helper --prepare-android 探测结果
	warnOnce    sync.Once
}

// Mirrors fc.buildStartCommand: Android pinned mount namespace, inherited netns FD,
// native preparation, and a legacy fallback. Template builds retain their launcher.
func buildStartCommand(
	ctx context.Context,
	execCtx context.Context,
	config cfg.BuilderConfig,
	startScript string,
	slot *network.Slot,
	restoredSandbox bool,
	prepArgv []string,
) (*exec.Cmd, func()) {
	fallback := func() (*exec.Cmd, func()) {
		return exec.CommandContext(execCtx,
			"unshare",
			"-m",
			"--",
			"bash",
			"-c",
			startScript,
		), func() {}
	}

	helper := strings.TrimSpace(config.StratoVirtNetnsExecHelper)
	if helper == "" || helper == "disabled" || helper == "ip-netns-exec" || !restoredSandbox || len(prepArgv) == 0 {
		return fallback()
	}

	mountnsFile, err := pinnedMountNSFile(ctx, helper)
	if err != nil {
		pinnedMountNS.warnOnce.Do(func() {
			zap.L().Warn("Android pinned mount namespace unavailable, falling back to plain unshare for all sandboxes", zap.Error(err))
		})

		return fallback()
	}

	if !pinnedMountNS.prepareVMOK {
		return fallback()
	}

	netnsFile, err := os.Open(netnsHostPath(slot))
	if err != nil {
		zap.L().Error("open sandbox netns failed, falling back to plain unshare", zap.String("netns", slot.NamespaceID()), zap.Error(err))

		return fallback()
	}

	inner := append([]string{"unshare", "-m", "--"}, prepArgv...)
	cmd := exec.CommandContext(execCtx,
		"nsenter", append([]string{"--mount=/proc/self/fd/4"}, inner...)...,
	)
	// Pin the namespace itself, so holder exit or PID reuse cannot redirect nsenter.
	cmd.ExtraFiles = []*os.File{netnsFile, mountnsFile}
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", stratoVirtNetnsFDEnv, 3), fmt.Sprintf("%s=%d", stratoVirtMountnsFDEnv, 4))

	return cmd, func() { netnsFile.Close() }
}

func netnsHostPath(slot *network.Slot) string {
	if slot.ExternalNetNS && slot.NetNSPath != "" {
		return slot.NetNSPath
	}

	return filepath.Join("/run/netns", slot.NamespaceID())
}

// WarmupPinnedMountNS must run before the network pool starts filling.
func WarmupPinnedMountNS(ctx context.Context, builderConfig cfg.BuilderConfig) {
	helper := strings.TrimSpace(builderConfig.StratoVirtNetnsExecHelper)
	if helper == "" || helper == "disabled" || helper == "ip-netns-exec" {
		return
	}

	if _, err := pinnedMountNSFile(ctx, helper); err != nil {
		zap.L().Error("warmup Android pinned mount namespace failed", zap.Error(err))
	}
}

func pinnedMountNSFile(ctx context.Context, helper string) (*os.File, error) {
	pinnedMountNS.once.Do(func() {
		pinnedMountNS.file, pinnedMountNS.err = setupPinnedMountNS(ctx, helper)
	})

	return pinnedMountNS.file, pinnedMountNS.err
}

// The holder exits on pipe EOF when orchestrator exits, including SIGKILL.
// Keep mount namespace operations in subprocesses, as in the FC launcher.
func setupPinnedMountNS(ctx context.Context, helper string) (*os.File, error) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create holder lifetime pipe: %w", err)
	}

	cmd := exec.CommandContext(context.Background(),
		"unshare", "-m", "--propagation", "private", "--",
		"sh", "-c", "umount -l /run/netns 2>/dev/null; read _ || true",
	)
	cmd.Stdin = pipeR

	if err := cmd.Start(); err != nil {
		pipeR.Close()
		pipeW.Close()

		return nil, fmt.Errorf("start mount ns holder: %w", err)
	}
	pipeR.Close()

	teardown := func() {
		pipeW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", cmd.Process.Pid)

	selfNS, err := os.Stat("/proc/self/ns/mnt")
	if err != nil {
		teardown()

		return nil, fmt.Errorf("stat self mount ns: %w", err)
	}
	selfStat := selfNS.Sys().(*syscall.Stat_t)

	deadline := time.Now().Add(5 * time.Second)
	unshared := false
	for time.Now().Before(deadline) {
		if childNS, err := os.Stat(nsPath); err == nil {
			childStat := childNS.Sys().(*syscall.Stat_t)
			if childStat.Ino != selfStat.Ino || childStat.Dev != selfStat.Dev {
				unshared = true

				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !unshared {
		teardown()

		return nil, fmt.Errorf("wait for mount ns holder unshare: timeout")
	}

	nsFile, err := os.Open(nsPath)
	if err != nil {
		teardown()
		return nil, fmt.Errorf("open pinned mount ns: %w", err)
	}
	keepNamespace := false
	defer func() {
		if !keepNamespace {
			_ = nsFile.Close()
		}
	}()

	enterProbe := exec.CommandContext(ctx, "nsenter", "--mount=/proc/self/fd/3", "/bin/true")
	enterProbe.ExtraFiles = []*os.File{nsFile}
	if out, err := enterProbe.CombinedOutput(); err != nil {
		teardown()

		return nil, fmt.Errorf("nsenter pinned mount ns failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	selfNetNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		teardown()

		return nil, fmt.Errorf("open self netns for helper probe: %w", err)
	}
	probe := exec.CommandContext(ctx, helper, "self", "/bin/true")
	probe.ExtraFiles = []*os.File{selfNetNS}
	probe.Env = append(os.Environ(), fmt.Sprintf("%s=%d", stratoVirtNetnsFDEnv, 3))
	out, probeErr := probe.CombinedOutput()
	selfNetNS.Close()
	if probeErr != nil {
		teardown()

		return nil, fmt.Errorf("helper %s does not support %s mode: %w (%s)", helper, stratoVirtNetnsFDEnv, probeErr, strings.TrimSpace(string(out)))
	}

	prepProbe := exec.CommandContext(ctx, helper, "--prepare-android", "--probe")
	if out, err := prepProbe.CombinedOutput(); err != nil {
		zap.L().Warn("helper does not support --prepare-android, falling back to bash start script", zap.String("helper", helper), zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
	} else {
		pinnedMountNS.prepareVMOK = true
		zap.L().Info("Android native launcher --prepare-android probe passed", zap.String("helper", helper))
	}

	pinnedMountNS.cmd = cmd
	pinnedMountNS.keepOpen = pipeW

	go func() {
		err := cmd.Wait()
		zap.L().Warn("Android mount namespace holder exited; pinned namespace remains available", zap.Int("pid", cmd.Process.Pid), zap.Error(err))
	}()

	zap.L().Sugar().Infof("pinned minimal mount namespace via holder pid %d (%s)", cmd.Process.Pid, nsPath)

	keepNamespace = true
	return nsFile, nil
}
