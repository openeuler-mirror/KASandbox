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

// netnsFDEnv carries a network namespace fd the outer start chain already
// opened: the orchestrator opens the slot netns in the host mount namespace and
// passes it on exec, because /run/netns may not be visible in the mount
// namespace this helper runs in. Joining the same netns again is a no-op.
const netnsFDEnv = "E2B_FC_NETNS_FD"

// prepareVMArgs covers every preparation step the V2 start script runs in bash:
//
//	mount --make-rprivate /
//	mount -t tmpfs tmpfs <SandboxDir> -o X-mount.mkdir
//	ln -s <HostRootfsPath> <SandboxDir>/<rootfsName>
//	mkdir -p <SandboxDir>/<kernelDir>
//	ln -s <HostKernelPath> <SandboxDir>/<kernelDir>/<kernelName>
//
// Done as direct system calls, this saves the fork/exec and dynamic linking of
// bash, mount, ln and mkdir. The order is the script's (the kernel directory
// must be created after the tmpfs mount).
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

// prepareVMMain handles the --prepare-vm mode. --probe only reports the
// capability and exits, so the orchestrator can probe the helper version at
// startup (an older helper does not know the flag and exits non-zero).
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

// run performs the preparation steps in the order of the V2 start script. Each
// failure names its step, as the bash && chain shows the failing command on
// stderr.
func (p *prepareVMArgs) run() error {
	// mount --make-rprivate /
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount --make-rprivate /: %w", err)
	}

	// -o X-mount.mkdir: create the mount point if it does not exist
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

	// mkdir -p <SandboxDir>/<kernelDir> (inside the tmpfs)
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
	// fd mode: the outer chain passed the netns fd; joining it again is a no-op
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
		// Close it once joined, so the fd does not leak into firecracker on exec
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
