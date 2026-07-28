package vmm

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

// OsType identifies the guest operating system family a sandbox runs.
type OsType string

const (
	OsLinux   OsType = "linux"
	OsWindows OsType = "windows"
	OsAndroid OsType = "android"
)

func ParseOsType(osType string) (OsType, error) {
	normalized := OsType(strings.ToLower(strings.TrimSpace(osType)))
	switch normalized {
	case "":
		return OsLinux, nil
	case OsLinux, OsWindows, OsAndroid:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported os type %q", osType)
	}
}

func ValidateBackendForOS(osType OsType, backend BackendType) error {
	switch osType {
	case OsLinux, OsWindows, OsAndroid:
	default:
		return fmt.Errorf("unsupported os type %q", osType)
	}

	switch backend {
	case BackendFirecracker, BackendStratoVirt:
	default:
		return fmt.Errorf("unsupported VMM type %q", backend)
	}

	if (osType == OsWindows || osType == OsAndroid) && backend == BackendFirecracker {
		return fmt.Errorf("unsupported OS/VMM combination %q/%q", osType, backend)
	}

	return nil
}

func (o OsType) OrDefault() OsType {
	if o == "" {
		return OsLinux
	}

	return o
}

func (o OsType) SupportsHugePages() bool {
	return o.OrDefault() == OsLinux
}

type VMMConfig struct {
	Type          BackendType
	KernelVersion string
	VMMVersion    string
	OsType        OsType
}

func (c VMMConfig) Backend() BackendType {
	return c.Type.OrDefault()
}

func (c VMMConfig) HostKernelPath(config cfg.BuilderConfig) string {
	return filepath.Join(config.HostKernelsDir, c.KernelVersion, "vmlinux.bin")
}

type ProcessOptions struct {
	// IoEngine is the io engine to use for the rootfs drive.
	IoEngine *string

	// InitScriptPath is the path to the init script that will be executed inside the VM on kernel start.
	InitScriptPath string

	// KernelLogs is a flag to enable kernel logs output to the process stdout.
	KernelLogs bool

	// SystemdToKernelLogs is a flag to enable systemd logs output to the console.
	// It enabled the kernel logs by default too.
	SystemdToKernelLogs bool

	// KvmClock is a flag to enable kvm-clock as the clocksource for the kernel.
	KvmClock bool

	// Stdout is the writer to which the process stdout will be written.
	Stdout io.Writer

	// Stderr is the writer to which the process stderr will be written.
	Stderr io.Writer
}

type KernelArgs map[string]string

func (ka KernelArgs) String() string {
	args := make([]string, 0, len(ka))
	for k, v := range ka {
		if v == "" {
			args = append(args, k)
		} else {
			args = append(args, fmt.Sprintf("%s=%s", k, v))
		}
	}
	sort.Strings(args) // optional: for consistent output

	return strings.Join(args, " ")
}
