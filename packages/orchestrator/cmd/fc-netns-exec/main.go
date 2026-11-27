//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

const netnsDir = "/run/netns"

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <netns-name-or-path> <command> [args...]\n", os.Args[0])
		os.Exit(2)
	}

	namespace := os.Args[1]
	command := os.Args[2]
	commandArgs := os.Args[2:]

	runtime.LockOSThread()

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

	if os.Getenv("E2B_FC_START_SCRIPT_DIAG") == "1" {
		fmt.Fprintf(
			os.Stderr,
			"e2b_fc_start_script_marker stage=inside_netns_before_firecracker_exec ns=%d socket=%s namespace=%s\n",
			time.Now().UnixNano(),
			findArgValue(commandArgs, "--api-sock"),
			namespace,
		)
	}

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

func findArgValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}

	return ""
}
