//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

const netnsDir = "/run/netns"

// The orchestrator passes an open netns FD because the pinned mount namespace
// does not contain the network pool's namespace mounts.
const netnsFDEnv = "E2B_STRATOVIRT_NETNS_FD"
const mountnsFDEnv = "E2B_STRATOVIRT_MOUNTNS_FD"

func main() {
	runtime.LockOSThread()

	// nsenter has already joined the mount namespace; do not pass its FD to the VMM.
	if fdEnv := os.Getenv(mountnsFDEnv); fdEnv != "" {
		fd, err := strconv.Atoi(fdEnv)
		if err != nil || fd < 3 {
			fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: invalid %s %q\n", mountnsFDEnv, fdEnv)
			os.Exit(1)
		}
		if err := unix.Close(fd); err != nil {
			fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: close mount namespace fd: %v\n", err)
			os.Exit(1)
		}
		os.Unsetenv(mountnsFDEnv)
	}

	if len(os.Args) > 1 && os.Args[1] == "--prepare-android" {
		prepareAndroidMain(os.Args[2:])
		return
	}

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s [--prepare-android <flags>] <netns-name-or-path> <command> [args...]\n", os.Args[0])
		os.Exit(2)
	}

	joinNetns(os.Args[1])
	execCommand(os.Args[2], os.Args[2:])
}

func joinNetns(namespace string) {
	if fdEnv := os.Getenv(netnsFDEnv); fdEnv != "" {
		fd, err := strconv.Atoi(fdEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: invalid %s %q: %v\n", netnsFDEnv, fdEnv, err)
			os.Exit(1)
		}
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: setns(fd %d, CLONE_NEWNET): %v\n", fd, err)
			os.Exit(1)
		}
		unix.Close(fd)
		os.Unsetenv(netnsFDEnv)
		return
	}

	nsPath := namespacePath(namespace)
	fd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: open network namespace %q: %v\n", nsPath, err)
		os.Exit(1)
	}
	defer unix.Close(fd)

	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: setns(%q, CLONE_NEWNET): %v\n", nsPath, err)
		os.Exit(1)
	}
}

func execCommand(command string, commandArgs []string) {
	if err := unix.Exec(command, commandArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: exec %q: %v\n", command, err)
		os.Exit(1)
	}
}

func namespacePath(namespace string) string {
	if filepath.IsAbs(namespace) {
		return filepath.Clean(namespace)
	}

	return filepath.Join(netnsDir, namespace)
}
