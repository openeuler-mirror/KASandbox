package vmm

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
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

// AndroidVersion identifies the Android guest version a sandbox runs. It is
// only meaningful when OsType is OsAndroid, in which case it must be one of
// 14/15/16 and may not be empty — there is no default. An empty value is
// rejected by ValidateAndroidVersion.
type AndroidVersion string

const (
	AndroidVersion14 AndroidVersion = "14"
	AndroidVersion15 AndroidVersion = "15"
	AndroidVersion16 AndroidVersion = "16"
)

// ParseAndroidVersion normalizes an Android version string. An empty input is
// returned as-is (unspecified); non-Android callers may leave it empty, but
// Android callers must pass a valid version, which ValidateAndroidVersion
// enforces.
func ParseAndroidVersion(v string) (AndroidVersion, error) {
	normalized := AndroidVersion(strings.TrimSpace(v))
	switch normalized {
	case "":
		return "", nil
	case AndroidVersion14, AndroidVersion15, AndroidVersion16:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported android version %q", v)
	}
}

// RequiresSecureEnv reports whether the guest exposes a secure_env client
// that needs the host-side secure_env daemon and its virtio-serial FIFOs.
// Android 14 has no such client; Android 15 and 16 do.
func (v AndroidVersion) RequiresSecureEnv() bool {
	switch v {
	case AndroidVersion15, AndroidVersion16:
		return true
	}
	return false
}

// RequiresConfigServer reports whether the guest needs the host-side
// config_server daemon. Android 14 does (runtime config over vsock 6800);
// 15+ use a different channel and omit the binary.
func (v AndroidVersion) RequiresConfigServer() bool {
	switch v {
	case AndroidVersion14:
		return true
	}
	return false
}

// SupportsJCardSim reports whether this Android version's secure_env binary
// supports the jcardsim flags (--enable_jcard_simulator, --jcardsim_fd_in/out).
// Added in Android 16; Android 15's secure_env rejects them and exits, so
// callers must only pass these flags when this returns true.
func (v AndroidVersion) SupportsJCardSim() bool {
	switch v {
	case AndroidVersion16:
		return true
	}
	return false
}

// ValidateAndroidVersion checks that the android version is valid for the
// given OS type. For non-Android OS types the version is ignored (no error).
// For Android the version must be one of 14/15/16; an empty or unsupported
// value is rejected and a reminder is logged.
func ValidateAndroidVersion(osType OsType, version AndroidVersion) error {
	if osType.OrDefault() != OsAndroid {
		return nil
	}
	switch version {
	case AndroidVersion14, AndroidVersion15, AndroidVersion16:
		return nil
	}
	logger.L().Warn(context.Background(),
		"android version is missing or unsupported; pass one of 14, 15, or 16",
		zap.String("os_type", string(osType)),
		zap.String("android_version", string(version)),
	)
	return fmt.Errorf("unsupported or missing android version %q (pass one of 14, 15, 16)", version)
}

type VMMConfig struct {
	Type           BackendType
	KernelVersion  string
	VMMVersion     string
	OsType         OsType
	AndroidVersion AndroidVersion
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
	// It enables the kernel logs by default too.
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
	sort.Strings(args) // for consistent output

	return strings.Join(args, " ")
}
