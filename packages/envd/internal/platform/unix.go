//go:build linux

package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/txn2/txeh"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/envd/internal/execcontext"
	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
	processRpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

const defaultCgroupManagerMegabyte = 1024 * 1024

// Filesystem magic numbers from Linux kernel (include/uapi/linux/magic.h).
const (
	nfsSuperMagic   = 0x6969
	cifsMagic       = 0xFF534D42
	smbSuperMagic   = 0x517B
	smb2MagicNumber = 0xFE534D42
	fuseSuperMagic  = 0x65735546
)

func Chown(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
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

func ValidateProcessRequest(*processRpc.StartRequest) error {
	return nil
}

func DefaultUser(fallback string) string {
	return fallback
}

func ConfigureProcessCredentials(
	cmd *exec.Cmd,
	u *user.User,
	cgroupManager cgroups.Manager,
	procType cgroups.ProcessType,
	logger *zerolog.Logger,
) error {
	uid, gid, err := UserIDUints(u)
	if err != nil {
		return err
	}

	groups := []uint32{gid}
	if gids, err := u.GroupIds(); err != nil {
		logger.Warn().Err(err).Str("user", u.Username).Msg("failed to get supplementary groups")
	} else {
		for _, g := range gids {
			if parsed, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(parsed))
			}
		}
	}

	cgroupFD, ok := cgroupManager.GetFileDescriptor(procType)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: ok,
		CgroupFD:    cgroupFD,
		Credential: &syscall.Credential{
			Uid:    uid,
			Gid:    gid,
			Groups: groups,
		},
	}

	return nil
}

func ConfigureProcessEnv(cmd *exec.Cmd, u *user.User, process *processRpc.ProcessConfig, defaults *execcontext.Defaults) {
	var formattedVars []string

	formattedVars = append(formattedVars, "PATH="+os.Getenv("PATH"))
	formattedVars = append(formattedVars, "HOME="+u.HomeDir)
	formattedVars = append(formattedVars, "USER="+u.Username)
	formattedVars = append(formattedVars, "LOGNAME="+u.Username)

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

func SendProcessSignal(cmd *exec.Cmd, signal syscall.Signal) error {
	return cmd.Process.Signal(signal)
}

func UserIDUints(u *user.User) (uid, gid uint32, err error) {
	newUID, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing uid '%s': %w", u.Uid, err)
	}

	newGID, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing gid '%s': %w", u.Gid, err)
	}

	return uint32(newUID), uint32(newGID), nil
}

func UserIDInts(u *user.User) (uid, gid int, err error) {
	newUID, err := strconv.ParseInt(u.Uid, 10, strconv.IntSize)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing uid '%s': %w", u.Uid, err)
	}

	newGID, err := strconv.ParseInt(u.Gid, 10, strconv.IntSize)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing gid '%s': %w", u.Gid, err)
	}

	return int(newUID), int(newGID), nil
}

func SetSystemTime(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())

	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

func SetupNFS(ctx context.Context, logger *zerolog.Logger, nfsTarget, path string) {
	commands := [][]string{
		{"mkdir", "-p", path},
		{"mount", "-v", "-t", "nfs", "-o", "mountproto=tcp,mountport=2049,proto=tcp,port=2049,nfsvers=3,noacl", nfsTarget, path},
	}

	for _, command := range commands {
		data, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()

		event := logger.
			With().
			Strs("command", command).
			Str("output", string(data)).
			Logger()

		if err != nil {
			event.Error().Err(err).Msg("Mount NFS")

			return
		}

		event.Debug().Msg("Mount NFS")
	}
}

func RewriteHostsFile(address, path string) error {
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

func DiskStats(path string) (DiskSpace, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskSpace{}, err
	}

	block := uint64(st.Bsize)

	return DiskSpace{
		Total:     st.Blocks * block,
		Available: st.Bavail * block,
	}, nil
}

func IsPathOnNetworkMount(path string) (bool, error) {
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(path, &statfs); err != nil {
		return false, fmt.Errorf("failed to statfs %s: %w", path, err)
	}

	switch statfs.Type {
	case nfsSuperMagic, cifsMagic, smbSuperMagic, smb2MagicNumber, fuseSuperMagic:
		return true, nil
	default:
		return false, nil
	}
}

func FileOwnership(fileInfo os.FileInfo) (owner, group string) {
	sys, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}

	owner = fmt.Sprintf("%d", sys.Uid)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}

	group = fmt.Sprintf("%d", sys.Gid)
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}

	return owner, group
}

func GetSubpaths(path string) (subpaths []string) {
	for {
		subpaths = append(subpaths, path)

		path = filepath.Dir(path)
		if path == "/" {
			break
		}
	}

	slices.Reverse(subpaths)

	return subpaths
}

func NewDefaultCgroupManager(root string, memTotal uint64) (cgroups.Manager, error) {
	// Try to keep 1/8 of the memory free, but no more than 128 MB.
	maxMemoryReserved := uint64(float64(memTotal) * .125)
	maxMemoryReserved = min(maxMemoryReserved, uint64(128)*defaultCgroupManagerMegabyte)

	opts := []cgroups.Cgroup2ManagerOption{
		cgroups.WithCgroup2ProcessType(cgroups.ProcessTypePTY, "ptys", map[string]string{
			"cpu.weight": "200", // Gets much preferred cpu access, to help keep these real time.
		}),
		cgroups.WithCgroup2ProcessType(cgroups.ProcessTypeSocat, "socats", map[string]string{
			"cpu.weight": "150", // Gets slightly preferred cpu access.
			"memory.min": fmt.Sprintf("%d", 5*defaultCgroupManagerMegabyte),
			"memory.low": fmt.Sprintf("%d", 8*defaultCgroupManagerMegabyte),
		}),
		cgroups.WithCgroup2ProcessType(cgroups.ProcessTypeUser, "user", map[string]string{
			"memory.high": fmt.Sprintf("%d", memTotal-maxMemoryReserved),
			"cpu.weight":  "50", // Less than envd, and less than core processes that default to 100.
		}),
	}
	if root != "" {
		opts = append(opts, cgroups.WithCgroup2RootSysFSPath(root))
	}

	return cgroups.NewCgroup2Manager(opts...)
}
