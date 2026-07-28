//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/rs/zerolog"
	"github.com/txn2/txeh"

	"github.com/e2b-dev/infra/packages/envd/internal/execcontext"
	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
	processRpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

const (
	PortForwardPlatformName          = "Windows"
	PortForwardScannerSubscriberName = "windows-port-forwarder"
	DefaultPortForwardBindIP         = "169.254.0.21"
	DefaultPortForwardDisabled       = false
)

func Chown(_ string, _, _ int) error {
	// TODO: Implement Windows ACL ownership updates if envd needs chown-equivalent semantics.
	return nil
}

func ProcessSignal(signal processRpc.Signal) (syscall.Signal, bool) {
	switch signal {
	case processRpc.Signal_SIGNAL_SIGKILL:
		return syscall.SIGKILL, true
	case processRpc.Signal_SIGNAL_SIGTERM:
		return syscall.SIGTERM, true
	default:
		return 0, false
	}
}

func ValidateProcessRequest(req *processRpc.StartRequest) error {
	if req.GetPty() != nil {
		return ErrUnsupportedPTY
	}

	return nil
}

func ConfigureProcessCredentials(
	_ *exec.Cmd,
	_ *user.User,
	_ cgroups.Manager,
	_ cgroups.ProcessType,
	_ *zerolog.Logger,
) error {
	// TODO: Add Windows user impersonation and resource controls when envd needs Linux credential/cgroup parity.
	return nil
}

func ConfigureProcessEnv(
	cmd *exec.Cmd,
	u *user.User,
	process *processRpc.ProcessConfig,
	defaults *execcontext.Defaults,
) {
	var formattedVars []string

	for _, key := range []string{
		"PATH",
		"SystemRoot",
		"WINDIR",
		"ComSpec",
		"PATHEXT",
		"TEMP",
		"TMP",
		"ProgramFiles",
		"ProgramFiles(x86)",
		"ProgramData",
	} {
		if value := os.Getenv(key); value != "" {
			formattedVars = append(formattedVars, key+"="+value)
		}
	}

	formattedVars = append(formattedVars,
		"HOME="+u.HomeDir,
		"USER="+u.Username,
		"LOGNAME="+u.Username,
		"USERPROFILE="+u.HomeDir,
	)

	if defaults.EnvVars != nil {
		defaults.EnvVars.Range(func(key string, value string) bool {
			formattedVars = append(formattedVars, key+"="+value)

			return true
		})
	}

	for key, value := range process.GetEnvs() {
		formattedVars = append(formattedVars, key+"="+value)
	}

	cmd.Env = formattedVars
}

func SendProcessSignal(cmd *exec.Cmd, _ syscall.Signal) error {
	// TODO: Implement Windows signal semantics beyond process termination.
	return cmd.Process.Kill()
}

func UserIDUints(_ *user.User) (uid, gid uint32, err error) {
	return 0, 0, nil
}

func UserIDInts(_ *user.User) (uid, gid int, err error) {
	return 0, 0, nil
}

func SetSystemTime(t time.Time) error {
	utc := t.UTC()
	systemTime := systemtime{
		Year:         uint16(utc.Year()),
		Month:        uint16(utc.Month()),
		DayOfWeek:    uint16(utc.Weekday()),
		Day:          uint16(utc.Day()),
		Hour:         uint16(utc.Hour()),
		Minute:       uint16(utc.Minute()),
		Second:       uint16(utc.Second()),
		Milliseconds: uint16(utc.Nanosecond() / int(time.Millisecond)),
	}

	r1, _, callErr := setSystemTime.Call(uintptr(unsafe.Pointer(&systemTime)))
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("failed to set Windows system time: %w", callErr)
		}

		return errors.New("failed to set Windows system time")
	}

	return nil
}

func SetupNFS(_ context.Context, logger *zerolog.Logger, nfsTarget, path string) {
	// TODO: Implement Windows NFS mount setup when volume mounts are supported.
	logger.Debug().
		Str("nfs_target", nfsTarget).
		Str("path", path).
		Msg("Skipping NFS mount on Windows")
}

func RewriteHostsFile(address, path string) error {
	if path == "/etc/hosts" {
		systemRoot := os.Getenv("SystemRoot")
		if systemRoot == "" {
			systemRoot = `C:\Windows`
		}

		path = filepath.Join(systemRoot, "System32", "drivers", "etc", "hosts")
	}

	hosts, err := txeh.NewHosts(&txeh.HostsConfig{
		ReadFilePath:  path,
		WriteFilePath: path,
	})
	if err != nil {
		return fmt.Errorf("failed to create hosts: %w", err)
	}

	ipFamily, err := getIPFamily(address)
	if err != nil {
		return fmt.Errorf("failed to get ip family: %w", err)
	}

	if ok, current, _ := hosts.HostAddressLookup(eventsHost, ipFamily); ok && current == address {
		return nil
	}

	hosts.AddHost(address, eventsHost)

	return hosts.Save()
}

func DefaultUser(fallback string) string {
	current, err := user.Current()
	if err == nil && current.Username != "" {
		return current.Username
	}

	if username := os.Getenv("USERNAME"); username != "" {
		return username
	}

	if username := os.Getenv("USER"); username != "" {
		return username
	}

	return fallback
}

func LookupUser(username string) (*user.User, error) {
	return user.Lookup(username)
}

func E2BRunDir() string {
	if runDir := os.Getenv("E2B_RUN_DIR"); runDir != "" {
		return runDir
	}

	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "e2b", "run")
	}

	return filepath.Join(os.TempDir(), "e2b", "run")
}

func DiskStats(path string) (DiskSpace, error) {
	root := filepath.VolumeName(os.Getenv("SystemDrive"))
	if root == "" {
		root = filepath.VolumeName(os.TempDir())
	}
	if root == "" {
		root = `C:`
	}

	return diskStatsForPath(rootSubpath(root+`\`, path))
}

func rootSubpath(root, path string) string {
	if path == "" {
		return root
	}

	if filepath.VolumeName(path) != "" {
		return path
	}

	path = filepath.Clean(path)
	path = strings.TrimLeft(path, `\/`)
	if path == "." {
		return root
	}

	return filepath.Join(root, path)
}

func diskStatsForPath(path string) (DiskSpace, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return DiskSpace{}, err
	}

	var freeBytesAvailable uint64
	var totalBytes uint64
	var totalFreeBytes uint64

	r1, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if r1 == 0 {
		return DiskSpace{}, callErr
	}

	return DiskSpace{Total: totalBytes, Available: freeBytesAvailable}, nil
}

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

type systemtime struct {
	Year         uint16
	Month        uint16
	DayOfWeek    uint16
	Day          uint16
	Hour         uint16
	Minute       uint16
	Second       uint16
	Milliseconds uint16
}

var setSystemTime = syscall.NewLazyDLL("kernel32.dll").NewProc("SetSystemTime")

func IsPathOnNetworkMount(_ string) (bool, error) {
	// TODO: Detect Windows UNC paths and mapped network drives.
	return false, nil
}

func FileOwnership(os.FileInfo) (owner, group string) {
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = "default"
	}

	return username, username
}

func GetSubpaths(path string) (subpaths []string) {
	for {
		subpaths = append(subpaths, path)

		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}

	slices.Reverse(subpaths)

	return subpaths
}

func NewDefaultCgroupManager(string, uint64) (cgroups.Manager, error) {
	// TODO: Implement Windows process resource management.
	return nil, errors.New("cgroups are not supported on Windows")
}
