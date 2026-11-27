//go:build linux

package network

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// netnsRunDir matches bindMountPath in vishvananda/netns
const netnsRunDir = "/run/netns"

// deleteNamedNamespace deletes a named netns. On systemd hosts /run is a shared
// mount, so a bind mount may propagate into several stacked copies on the same
// path, while netns.DeleteNamed unmounts only once and leaves the lower ones.
// When DeleteNamed fails, unmount repeatedly until the path is no longer a
// mount point (EINVAL), then remove the file.
func deleteNamedNamespace(name string) error {
	err := netns.DeleteNamed(name)
	if err == nil {
		return nil
	}

	path := filepath.Join(netnsRunDir, name)
	for range 8 {
		umountErr := unix.Unmount(path, unix.MNT_DETACH)
		if umountErr == nil {
			continue
		}
		// EINVAL: not a mount point any more (fully unmounted); ENOENT: the
		// leftover was already cleaned up concurrently
		if errors.Is(umountErr, unix.EINVAL) || errors.Is(umountErr, unix.ENOENT) {
			break
		}

		return errors.Join(err, fmt.Errorf("error unmounting duplicated namespace mount %s: %w", path, umountErr))
	}

	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("error removing namespace file %s: %w", path, removeErr))
	}

	return nil
}

// shouldIsolateNetNSDir decides from IsolateNetNSDir and the environment
// whether /run/netns is made private. auto isolates only on a bare host and
// skips it inside a Kubernetes pod (see runningInKubernetes).
func (c Config) shouldIsolateNetNSDir() bool {
	switch strings.ToLower(strings.TrimSpace(c.IsolateNetNSDir)) {
	case "false", "0", "off":
		return false
	case "true", "1", "on":
		return true
	default: // auto and any unknown value
		return !runningInKubernetes()
	}
}

// runningInKubernetes reports whether the process runs in a Kubernetes pod.
// The kubelet sets KUBERNETES_SERVICE_HOST in every pod, hostNetwork included.
func runningInKubernetes() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// isolateNetNSDir makes /run/netns a separate private mount point. On systemd
// hosts /run is a shared mount, so netns bind mounts propagate as stacked
// copies on the same path, while teardown unmounts only once; the leftovers
// pile up and slow down every netns and mount operation. Equivalent to
// mount --bind /run/netns /run/netns && mount --make-rprivate /run/netns, as
// iproute2 does for a shared /run. Idempotent: an existing mount point is not
// bound again, or every restart would stack another layer and bury the netns
// mounts of the previous layer in the mount table (still reachable by their
// old path, but impossible to clean up by name).
func isolateNetNSDir() error {
	if err := os.MkdirAll(netnsRunDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", netnsRunDir, err)
	}

	mounted, err := isMountPoint(netnsRunDir)
	if err != nil {
		return fmt.Errorf("check %s mountpoint: %w", netnsRunDir, err)
	}

	if !mounted {
		if err := unix.Mount(netnsRunDir, netnsRunDir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind mount %s: %w", netnsRunDir, err)
		}
	}

	if err := unix.Mount("", netnsRunDir, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make %s private: %w", netnsRunDir, err)
	}

	return nil
}

// isMountPoint reports whether path is a mount point, by /proc/self/mountinfo.
// Comparing st_dev with the parent does not work: a bind mount inside the same
// file system has the same device number.
func isMountPoint(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}

	// The 5th field of each line is the mount point
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == path {
			return true, nil
		}
	}

	return false, nil
}
