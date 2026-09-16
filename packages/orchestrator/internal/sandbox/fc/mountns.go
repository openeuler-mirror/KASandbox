package fc

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

// 沙箱拉起时 startScript 需要独立的 mount namespace（会在沙箱目录挂载专属
// tmpfs），此前通过 `unshare -m -- bash -c <script>` 实现：每个沙箱的
// unshare(CLONE_NEWNS) 都会完整复制宿主挂载表。宿主挂载数随 netns 池规模
// （每槽位一个 nsfs 挂载）持续增长，且复制过程被内核命名空间锁串行化，
// 高并发下拉起耗时随挂载数线性恶化。
//
// 这里的实现：
//  1. 编排器启动时创建「固定最小 mount namespace」：独立 holder 子进程 unshare
//     并设为 private，先在自己视图里卸掉 /run/netns（不引用任何 netns），
//     之后挂载表冻结、不再随宿主增长。
//  2. 拉起沙箱改为 `nsenter -m <holder ns> unshare -m -- bash -c <script>`：
//     先进入 holder 的命名空间，再 unshare 复制出沙箱私有 mount ns——复制对象
//     从数千项的宿主表变成冻结的小表，单次成本由随挂载数增长变为恒定。
//  3. 目标 netns 由编排器在宿主视图打开后经 ExtraFiles 传入（子进程内为 fd 3，
//     环境变量 E2B_FC_NETNS_FD 告知脚本里的 fc-netns-exec 直接按 fd setns），
//     不依赖 /run/netns 在新 mount ns 中的可见性。
//
// holder 的生命周期由管道维系：其 stdin 是编排器持有的管道读端，编排器退出
// （包括被 SIGKILL）后写端关闭，holder 读到 EOF 自行退出，不需要 pid 文件，
// 也不会误杀其他进程。不用 Pdeathsig：Go 的 Pdeathsig 绑定 fork 线程，
// 线程退出会误杀 holder。
//
// 不用 Go 直接 setns(CLONE_NEWNS)：内核禁止多线程进程进入 mount namespace。
// 任何一步失败都降级为原 `unshare -m` 拉起路径，仅记录一次日志。

// fcNetnsFDEnv 与 cmd/fc-netns-exec 的 netnsFDEnv 保持一致
const fcNetnsFDEnv = "E2B_FC_NETNS_FD"

// prepareVMEnv 控制是否用 fc-netns-exec --prepare-vm 合并 bash 准备链
// （mount/mkdir/ln 五个工具进程 → helper 直接系统调用）。默认开启，置
// 0/false 关闭；helper 探测不支持时也会自动回退原脚本路径。
const prepareVMEnv = "E2B_FC_PREPARE_VM"

var pinnedMountNS struct {
	once        sync.Once
	path        string
	err         error
	cmd         *exec.Cmd
	keepOpen    *os.File // 管道写端，编排器存活期间保持打开
	prepareVMOK bool     // helper --prepare-vm 探测结果
	warnOnce    sync.Once
}

func prepareVMEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(prepareVMEnv))) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

// buildStartCommand 构造 FC 启动命令；返回的 closeFn 在 cmd 启动后调用（关闭
// 传入的 netns fd 的父进程副本）。
//
// helper 可用且为恢复快照的沙箱（非模板构建 VM）时走固定最小 mount ns 链路：
// prepArgv 非空（V2 布局）且 --prepare-vm 探测通过时，bash 准备链整体并入
// helper，启动链收敛为 nsenter → unshare → helper → firecracker；否则保持
// `nsenter → unshare → bash -c <script>`。helper 不可用或非恢复沙箱时回退为
// `unshare -m -- bash -c <script>`（直接复制宿主挂载表）。
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

	// holder 意外退出后 /proc/<pid>/ns/mnt 随之消失，此时回退而不是让拉起失败
	if _, err := os.Stat(pinPath); err != nil {
		pinnedMountNS.warnOnce.Do(func() {
			zap.L().Warn("pinned mount namespace holder is gone, falling back to plain unshare for all sandboxes", zap.String("path", pinPath), zap.Error(err))
		})

		return fallback()
	}

	// 在宿主 mount ns 视图里打开目标 netns（/run/netns 在沙箱新 mount ns 中不可见），
	// 经 ExtraFiles 传入启动链，子进程内为 fd 3
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

// netnsHostPath 返回槽位 netns 在宿主视图下的路径。外部（CNI）netns 直接用
// 其真实路径，不假定位于 /run/netns 下。
func netnsHostPath(slot *network.Slot) string {
	if slot.ExternalNetNS && slot.NetNSPath != "" {
		return slot.NetNSPath
	}

	return filepath.Join("/run/netns", slot.NamespaceID())
}

// WarmupPinnedMountNS 在编排器启动早期创建固定最小 mount namespace。
// 应在 /run/netns 隔离、遗留 netns 清理之后、网络池填充之前调用，使 holder
// 的挂载表冻结在最小规模且不引用任何 netns。不可用时仅记录日志，拉起路径
// 会回退为直接 unshare -m。
func WarmupPinnedMountNS(ctx context.Context, builderConfig cfg.BuilderConfig) {
	helper := strings.TrimSpace(builderConfig.FirecrackerNetnsExecHelper)
	if helper == "" || helper == "disabled" || helper == "ip-netns-exec" {
		return
	}

	if _, err := pinnedMountNSPath(ctx, helper); err != nil {
		zap.L().Error("warmup pinned mount namespace failed", zap.Error(err))
	}
}

// pinnedMountNSPath 返回固定最小 mount namespace 的引用路径；不可用时返回错误。
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

	// holder：private 传播保证挂载表不随宿主增长；先卸掉 /run/netns 使其不引用
	// 任何 netns（拉起链按 fd 进入 netns，不需要这个目录）；然后阻塞读 stdin，
	// 编排器退出时读到 EOF 自行结束
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
	// 读端已复制进子进程，父进程副本关闭；写端保持打开直到编排器退出
	pipeR.Close()

	teardown := func() {
		pipeW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", cmd.Process.Pid)

	// 等子进程真正完成 unshare（其 mnt ns 的 inode 与宿主不同）
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

	// 探测 1：nsenter 能否进入钉住的 mount ns（用绝对路径，unix.Exec 系不做 PATH 搜索）
	if out, err := exec.CommandContext(ctx, "nsenter", "--mount="+nsPath, "/bin/true").CombinedOutput(); err != nil {
		teardown()

		return "", fmt.Errorf("nsenter pinned mount ns failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// 探测 2：helper 是否支持 E2B_FC_NETNS_FD 模式（helper 与编排器可能
	// 不是同批部署的版本，不支持则整体回退，避免拉起链路引用不存在的能力）
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

	// 探测 3：helper 是否支持 --prepare-vm 合并准备链。不支持不算致命，
	// 拉起路径回退为 bash 脚本（行为与未合并前完全一致）
	prepProbe := exec.CommandContext(ctx, helper, "--prepare-vm", "--probe")
	if out, err := prepProbe.CombinedOutput(); err != nil {
		zap.L().Warn("helper does not support --prepare-vm, falling back to bash start script", zap.String("helper", helper), zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
	} else {
		pinnedMountNS.prepareVMOK = true
		zap.L().Info("helper --prepare-vm probe passed, bash start script chain merged into helper", zap.String("helper", helper))
	}

	pinnedMountNS.cmd = cmd
	pinnedMountNS.keepOpen = pipeW

	// 回收 holder；正常情况下它随编排器一起退出，提前退出说明被外部杀掉
	go func() {
		err := cmd.Wait()
		zap.L().Warn("pinned mount namespace holder exited, sandbox start falls back to plain unshare", zap.Int("pid", cmd.Process.Pid), zap.Error(err))
	}()

	zap.L().Sugar().Infof("pinned minimal mount namespace via holder pid %d (%s)", cmd.Process.Pid, nsPath)

	return nsPath, nil
}
