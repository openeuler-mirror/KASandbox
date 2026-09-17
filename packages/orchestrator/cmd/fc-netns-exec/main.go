//go:build linux

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const netnsDir = "/run/netns"

// netnsFDEnv 外层启动链通过该环境变量把已打开的 netns fd 传给本 helper：
// 拉起沙箱时 netns 由编排器在宿主视图打开并随 exec 传入（此时当前 mount ns
// 里 /run/netns 可能不可见，不能再按路径打开）；setns 到相同 netns 是 no-op。
const netnsFDEnv = "E2B_FC_NETNS_FD"

// prepareVMArgs 对应原 V2 startScript 中 bash 链执行的全部准备步骤：
//
//	mount --make-rprivate /
//	mount -t tmpfs tmpfs <SandboxDir> -o X-mount.mkdir
//	ln -s <HostRootfsPath> <SandboxDir>/<rootfsName>
//	mkdir -p <SandboxDir>/<kernelDir>
//	ln -s <HostKernelPath> <SandboxDir>/<kernelDir>/<kernelName>
//
// 全部改为直接系统调用，省掉 bash、mount、ln、mkdir 五个工具进程的
// fork/exec 与动态链接开销。步骤顺序与脚本保持一致（kernel 目录的创建
// 必须发生在 tmpfs 挂载之后）。
type prepareVMArgs struct {
	sandboxDir string
	rootfsSrc  string
	rootfsName string
	kernelSrc  string
	kernelDir  string
	kernelName string
}

func main() {
	runtime.LockOSThread()

	if len(os.Args) > 1 && os.Args[1] == "--prepare-vm" {
		prepareVMMain(os.Args[2:])

		return
	}

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s [--prepare-vm <flags>] <netns-name-or-path> <command> [args...]\n", os.Args[0])
		os.Exit(2)
	}

	namespace := os.Args[1]
	command := os.Args[2]
	commandArgs := os.Args[2:]

	joinNetns(namespace)

	if os.Getenv("E2B_FC_START_SCRIPT_DIAG") == "1" {
		printDiagMarker(namespace, commandArgs)
	}

	execCommand(command, commandArgs)
}

// prepareVMMain 处理 `--prepare-vm` 子模式。`--probe` 只回显能力并退出，
// 供编排器启动时探测 helper 版本（旧版本 helper 不认识该参数会非零退出）。
func prepareVMMain(args []string) {
	fs := flag.NewFlagSet("prepare-vm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var prep prepareVMArgs
	var probe bool
	fs.StringVar(&prep.sandboxDir, "sandbox-dir", "", "sandbox directory to mount tmpfs on")
	fs.StringVar(&prep.rootfsSrc, "rootfs-src", "", "host rootfs link source")
	fs.StringVar(&prep.rootfsName, "rootfs-name", "", "rootfs file name inside sandbox dir")
	fs.StringVar(&prep.kernelSrc, "kernel-src", "", "host kernel path")
	fs.StringVar(&prep.kernelDir, "kernel-dir", "", "kernel directory inside sandbox dir")
	fs.StringVar(&prep.kernelName, "kernel-name", "", "kernel file name inside kernel dir")
	fs.BoolVar(&probe, "probe", false, "probe prepare-vm capability and exit")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: prepare-vm: %v\n", err)
		os.Exit(2)
	}

	if probe {
		fmt.Println("prepare-vm ok")
		os.Exit(0)
	}

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s --prepare-vm <flags> <netns-name-or-path> <command> [args...]\n", os.Args[0])
		os.Exit(2)
	}

	if err := prep.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: prepare-vm: %v\n", err)
		os.Exit(2)
	}

	if err := prep.run(); err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: prepare-vm: %v\n", err)
		os.Exit(1)
	}

	namespace := rest[0]
	command := rest[1]
	commandArgs := rest[1:]

	joinNetns(namespace)

	if os.Getenv("E2B_FC_START_SCRIPT_DIAG") == "1" {
		printDiagMarker(namespace, commandArgs)
	}

	execCommand(command, commandArgs)
}

func (p *prepareVMArgs) validate() error {
	for name, value := range map[string]string{
		"sandbox-dir": p.sandboxDir,
		"rootfs-src":  p.rootfsSrc,
		"rootfs-name": p.rootfsName,
		"kernel-src":  p.kernelSrc,
		"kernel-dir":  p.kernelDir,
		"kernel-name": p.kernelName,
	} {
		if value == "" {
			return fmt.Errorf("missing required flag -%s", name)
		}
	}

	return nil
}

// run 按 V2 startScript 的顺序执行准备步骤，每步失败都带步骤名返回，
// 与原 bash `&&` 链失败时能在 stderr 看到具体命令的行为对齐。
func (p *prepareVMArgs) run() error {
	// mount --make-rprivate /
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount --make-rprivate /: %w", err)
	}

	// -o X-mount.mkdir：挂载点不存在时创建
	if err := os.MkdirAll(p.sandboxDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", p.sandboxDir, err)
	}

	// mount -t tmpfs tmpfs <SandboxDir>
	if err := unix.Mount("tmpfs", p.sandboxDir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", p.sandboxDir, err)
	}

	// ln -s <HostRootfsPath> <SandboxDir>/<rootfsName>
	rootfsDst := filepath.Join(p.sandboxDir, p.rootfsName)
	if err := os.Symlink(p.rootfsSrc, rootfsDst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", rootfsDst, p.rootfsSrc, err)
	}

	// mkdir -p <SandboxDir>/<kernelDir>（在 tmpfs 内）
	kernelDir := filepath.Join(p.sandboxDir, p.kernelDir)
	if err := os.MkdirAll(kernelDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", kernelDir, err)
	}

	// ln -s <HostKernelPath> <kernelDir>/<kernelName>
	kernelDst := filepath.Join(kernelDir, p.kernelName)
	if err := os.Symlink(p.kernelSrc, kernelDst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", kernelDst, p.kernelSrc, err)
	}

	return nil
}

func joinNetns(namespace string) {
	// fd 传递模式：编排器已在外层进入过 netns，这里 setns 到相同 netns（no-op）
	if fdEnv := os.Getenv(netnsFDEnv); fdEnv != "" {
		fd, err := strconv.Atoi(fdEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fc-netns-exec: invalid %s %q: %v\n", netnsFDEnv, fdEnv, err)
			os.Exit(1)
		}
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			fmt.Fprintf(os.Stderr, "fc-netns-exec: setns(fd %d, CLONE_NEWNET): %v\n", fd, err)
			os.Exit(1)
		}
		// 进入后即关闭，避免 fd 随 exec 泄漏进 firecracker
		unix.Close(fd)
		os.Unsetenv(netnsFDEnv)

		return
	}

	nsPath := namespacePath(namespace)
	fd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: open network namespace %q: %v\n", nsPath, err)
		os.Exit(1)
	}
	defer unix.Close(fd)

	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: setns(%q, CLONE_NEWNET): %v\n", nsPath, err)
		os.Exit(1)
	}
}

func execCommand(command string, commandArgs []string) {
	if err := unix.Exec(command, commandArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "fc-netns-exec: exec %q: %v\n", command, err)
		os.Exit(1)
	}
}

func namespacePath(namespace string) string {
	if filepath.IsAbs(namespace) {
		return filepath.Clean(namespace)
	}

	return filepath.Join(netnsDir, namespace)
}

func printDiagMarker(namespace string, commandArgs []string) {
	fmt.Fprintf(
		os.Stderr,
		"e2b_fc_start_script_marker stage=inside_netns_before_firecracker_exec ns=%d socket=%s namespace=%s\n",
		time.Now().UnixNano(),
		findArgValue(commandArgs, "--api-sock"),
		namespace,
	)
}

func findArgValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}

	return ""
}
