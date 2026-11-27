package fc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
)

// The start script needs its own mount namespace (it mounts a private tmpfs on
// the sandbox directory). Starting it with `unshare -m -- bash -c <script>`
// copies the whole host mount table for every sandbox. The host table grows
// with the netns pool (one nsfs mount per slot), the copy is serialized by the
// kernel's namespace lock, and every copy keeps references to the netns mounts
// that existed at that moment, so start latency gets worse with the number of
// mounts under high concurrency.
//
// Instead:
//  1. At startup the orchestrator creates a pinned minimal mount namespace: a
//     holder child unshares with private propagation and first unmounts
//     /run/netns in its own view (so it references no netns). Its mount table
//     is frozen from then on and does not grow with the host.
//  2. A sandbox starts with `nsenter -m <holder ns> unshare -m -- bash -c
//     <script>`: it enters the holder's namespace and unshares the sandbox's
//     private mount namespace from there, copying the small frozen table
//     instead of thousands of host entries, at a constant cost.
//  3. The orchestrator opens the target netns in the host view and passes it
//     through ExtraFiles (fd 3 in the child; E2B_FC_NETNS_FD tells
//     fc-netns-exec in the script to setns by fd), so the start does not rely
//     on /run/netns being visible in the new mount namespace.
//
// The holder lives as long as a pipe: its stdin is the read end of a pipe whose
// write end the orchestrator keeps open. When the orchestrator exits (SIGKILL
// included) the write end closes, the holder reads EOF and exits, with no pid
// file and no risk of killing an unrelated process. Pdeathsig is not used: in
// Go it is tied to the forking thread, whose exit would kill the holder.
//
// Go cannot setns(CLONE_NEWNS) itself: the kernel refuses mount namespace
// changes in a multi-threaded process. Any failure falls back to the plain
// `unshare -m` start and is logged once.

// fcNetnsFDEnv matches netnsFDEnv in cmd/fc-netns-exec
const fcNetnsFDEnv = "E2B_FC_NETNS_FD"

// prepareVMEnv controls whether fc-netns-exec --prepare-vm replaces the bash
// preparation chain (mount, mkdir and ln processes become direct system calls
// in the helper). On by default, 0/false turns it off; a helper that fails the
// probe also falls back to the script.
const prepareVMEnv = "E2B_FC_PREPARE_VM"

var pinnedMountNS struct {
	once        sync.Once
	path        string
	err         error
	cmd         *exec.Cmd
	keepOpen    *os.File // write end of the pipe, open as long as the orchestrator runs
	prepareVMOK bool     // result of the helper --prepare-vm probe
	warnOnce    sync.Once
	// holderGone is set as soon as the holder has exited and been reaped.
	// From then on its pid may belong to an unrelated process, so
	// /proc/<pid>/ns/mnt must not be used even if the path exists.
	holderGone atomic.Bool
}

func prepareVMEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(prepareVMEnv))) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

// buildStartCommand builds the Firecracker start command; the returned closeFn
// is called after the command started (it closes the parent's copy of the
// netns fd passed in).
//
// With a usable helper and a sandbox restored from a snapshot (not a template
// build VM) it uses the pinned minimal mount namespace: with prepArgv (V2
// layout) and a passed --prepare-vm probe the whole bash preparation chain
// moves into the helper and the chain is nsenter -> unshare -> helper ->
// firecracker; otherwise it is nsenter -> unshare -> bash -c <script>. Without
// a usable helper, or for other VMs, it falls back to
// `unshare -m -- bash -c <script>` (copying the host mount table).
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

	helper := strings.TrimSpace(config.FirecrackerNetnsExecHelper)
	if helper == "" || helper == "disabled" || helper == "ip-netns-exec" || !restoredSandbox {
		return fallback()
	}

	pinPath, err := pinnedMountNSPath(ctx, helper)
	if err != nil {
		pinnedMountNS.warnOnce.Do(func() {
			zap.L().Warn("pinned mount namespace unavailable, falling back to plain unshare for all sandboxes", zap.Error(err))
		})

		return fallback()
	}

	// If the holder died, fall back instead of failing the start. Checking only
	// that /proc/<pid>/ns/mnt exists is not enough: once the holder is reaped
	// its pid can be reused by an unrelated process.
	if pinnedMountNS.holderGone.Load() {
		pinnedMountNS.warnOnce.Do(func() {
			zap.L().Warn("pinned mount namespace holder has exited, falling back to plain unshare for all sandboxes", zap.String("path", pinPath))
		})

		return fallback()
	}
	if _, err := os.Stat(pinPath); err != nil {
		pinnedMountNS.warnOnce.Do(func() {
			zap.L().Warn("pinned mount namespace holder is gone, falling back to plain unshare for all sandboxes", zap.String("path", pinPath), zap.Error(err))
		})

		return fallback()
	}

	// Open the target netns in the host mount namespace view (/run/netns is not
	// visible in the sandbox's new mount namespace) and pass it to the start
	// chain through ExtraFiles, as fd 3 in the child
	netnsFile, err := os.Open(netnsHostPath(slot))
	if err != nil {
		zap.L().Error("open sandbox netns failed, falling back to plain unshare", zap.String("netns", slot.NamespaceID()), zap.Error(err))

		return fallback()
	}

	inner := []string{"unshare", "-m", "--", "bash", "-c", startScript}
	if len(prepArgv) > 0 && pinnedMountNS.prepareVMOK && prepareVMEnabled() {
		inner = append([]string{"unshare", "-m", "--"}, prepArgv...)
	}

	cmd := exec.CommandContext(execCtx,
		"nsenter", append([]string{"--mount=" + pinPath}, inner...)...,
	)
	cmd.ExtraFiles = []*os.File{netnsFile}
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", fcNetnsFDEnv, 3))

	return cmd, func() { netnsFile.Close() }
}

// netnsHostPath returns the path of the slot's netns in the host view.
func netnsHostPath(slot *network.Slot) string {
	return filepath.Join("/run/netns", slot.NamespaceID())
}

// WarmupPinnedMountNS creates the pinned minimal mount namespace early at
// startup. Call it after /run/netns is isolated and leftover netns are cleaned
// up, and before the network pool fills, so the holder's frozen mount table is
// minimal and references no netns. If it is unavailable it only logs, and
// sandbox starts fall back to plain unshare -m.
func WarmupPinnedMountNS(ctx context.Context, builderConfig cfg.BuilderConfig) {
	helper := strings.TrimSpace(builderConfig.FirecrackerNetnsExecHelper)
	if helper == "" || helper == "disabled" || helper == "ip-netns-exec" {
		return
	}

	if _, err := pinnedMountNSPath(ctx, helper); err != nil {
		zap.L().Error("warmup pinned mount namespace failed", zap.Error(err))
	}
}

// pinnedMountNSPath returns the path referencing the pinned minimal mount
// namespace, or an error if it is unavailable.
func pinnedMountNSPath(ctx context.Context, helper string) (string, error) {
	pinnedMountNS.once.Do(func() {
		pinnedMountNS.path, pinnedMountNS.err = setupPinnedMountNS(ctx, helper)
	})

	return pinnedMountNS.path, pinnedMountNS.err
}

func setupPinnedMountNS(ctx context.Context, helper string) (string, error) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("create holder lifetime pipe: %w", err)
	}

	// The holder: private propagation keeps its mount table from growing with
	// the host; it first unmounts /run/netns so it references no netns (the
	// start chain enters the netns by fd and does not need the directory); then
	// it blocks reading stdin and exits on EOF when the orchestrator exits
	cmd := exec.CommandContext(context.Background(),
		"unshare", "-m", "--propagation", "private", "--",
		"sh", "-c", "umount -l /run/netns 2>/dev/null; read _ || true",
	)
	cmd.Stdin = pipeR

	if err := cmd.Start(); err != nil {
		pipeR.Close()
		pipeW.Close()

		return "", fmt.Errorf("start mount ns holder: %w", err)
	}
	// The child has its copy of the read end; close the parent's. The write end
	// stays open until the orchestrator exits
	pipeR.Close()

	teardown := func() {
		pipeW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", cmd.Process.Pid)

	// Wait until the child has really unshared (its mnt ns inode differs from
	// the host's)
	selfNS, err := os.Stat("/proc/self/ns/mnt")
	if err != nil {
		teardown()

		return "", fmt.Errorf("stat self mount ns: %w", err)
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

		return "", fmt.Errorf("wait for mount ns holder unshare: timeout")
	}

	// Probe 1: can nsenter enter the pinned mount namespace (absolute path, the
	// unix.Exec family does no PATH lookup)
	if out, err := exec.CommandContext(ctx, "nsenter", "--mount="+nsPath, "/bin/true").CombinedOutput(); err != nil {
		teardown()

		return "", fmt.Errorf("nsenter pinned mount ns failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Probe 2: does the helper support E2B_FC_NETNS_FD (the helper and the
	// orchestrator may come from different deployments; without support fall
	// back entirely, so the start chain does not rely on a missing capability)
	selfNetNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		teardown()

		return "", fmt.Errorf("open self netns for helper probe: %w", err)
	}
	probe := exec.CommandContext(ctx, helper, "self", "/bin/true")
	probe.ExtraFiles = []*os.File{selfNetNS}
	probe.Env = append(os.Environ(), fmt.Sprintf("%s=%d", fcNetnsFDEnv, 3))
	out, probeErr := probe.CombinedOutput()
	selfNetNS.Close()
	if probeErr != nil {
		teardown()

		return "", fmt.Errorf("helper %s does not support %s mode: %w (%s)", helper, fcNetnsFDEnv, probeErr, strings.TrimSpace(string(out)))
	}

	// Probe 3: does the helper support --prepare-vm. Not fatal: without it the
	// start falls back to the bash script (exactly the behavior before)
	prepProbe := exec.CommandContext(ctx, helper, "--prepare-vm", "--probe")
	if out, err := prepProbe.CombinedOutput(); err != nil {
		zap.L().Warn("helper does not support --prepare-vm, falling back to bash start script", zap.String("helper", helper), zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
	} else {
		pinnedMountNS.prepareVMOK = true
		zap.L().Info("helper --prepare-vm probe passed, bash start script chain merged into helper", zap.String("helper", helper))
	}

	pinnedMountNS.cmd = cmd
	pinnedMountNS.keepOpen = pipeW

	watchHolder(cmd)

	zap.L().Sugar().Infof("pinned minimal mount namespace via holder pid %d (%s)", cmd.Process.Pid, nsPath)

	return nsPath, nil
}

// watchHolder reaps the holder. It normally exits with the orchestrator; an
// early exit means something killed it. holderGone is set right after the
// reap, before its pid can have been handed out again in practice.
func watchHolder(cmd *exec.Cmd) {
	go func() {
		err := cmd.Wait()
		pinnedMountNS.holderGone.Store(true)
		zap.L().Warn("pinned mount namespace holder exited, sandbox start falls back to plain unshare", zap.Int("pid", cmd.Process.Pid), zap.Error(err))
	}()
}
