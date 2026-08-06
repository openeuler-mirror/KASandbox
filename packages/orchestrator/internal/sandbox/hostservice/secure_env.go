package hostservice

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
)

// SecureEnvFds holds the FIFO fds secure_env inherits, opened by the
// orchestrator. *In opens "<name>.in" (guest reads, host writes), *Out
// opens "<name>.out" (guest writes, host reads). The --*_fd_in/_fd_out
// flags are reversed, hence Out-before-In in ExtraFiles.
type SecureEnvFds struct {
	KeymintIn     *os.File // -> keymint_fifo_vm.in
	KeymintOut    *os.File // -> keymint_fifo_vm.out
	KeymasterIn   *os.File // -> keymaster_fifo_vm.in
	KeymasterOut  *os.File // -> keymaster_fifo_vm.out
	GatekeeperIn  *os.File // -> gatekeeper_fifo_vm.in
	GatekeeperOut *os.File // -> gatekeeper_fifo_vm.out
	OemlockIn     *os.File // -> oemlock_fifo_vm.in
	OemlockOut    *os.File // -> oemlock_fifo_vm.out
	// No virtio-serial peer; O_RDWR only to pass secure_env's fd check.
	JcardsimIn  *os.File // -> jcardsim_fifo_vm.in
	JcardsimOut *os.File // -> jcardsim_fifo_vm.out
	// Inert O_RDWR FIFO: reads block forever (fd is its own writer, no EOF).
	SnapshotControl *os.File // fd -> snapshot_control_fifo (O_RDWR)
	// Real listening AF_UNIX SOCK_STREAM; MainLoop's Accept needs a socket fd.
	// Passing an open fd makes secure_env skip its NetNS-bound rebind branch.
	ConfuiServer *UnixListener // fd 14 -> <sandboxDir>/confui_sign.sock
	// Inert O_RDWR FIFO: StartKernelEventMonitor blocks on ReadEvent.
	KernelEvents *os.File // fd -> kernel_events_fifo (O_RDWR)
}

// secureEnvPipeDir returns the per-sandbox secure_env FIFO directory.
func secureEnvPipeDir(sandboxHostDir string) string {
	return filepath.Join(sandboxHostDir, "android-pipes")
}

// BuildSecureEnvService builds the secure_env daemon service and owns its
// FIFO lifecycle (PrepareSecureEnvFds creates/opens, CloseParentResources
// closes). Required for Android 15+ only. Fds are inherited as fds 3..15
// in the order documented on SecureEnvFds. No ReadyCheck: readiness is only
// provable after the guest client connects post-restore; crashes are handled
// by RestartOnCrash.
func BuildSecureEnvService(
	config cfg.BuilderConfig,
	androidVersion string,
	cuttlefishConfigPath string,
	netNSName string,
	sandboxHostDir string,
) (Service, error) {
	hostPackageDir := config.CvdHostPackageDirForVersion(androidVersion)
	binaryPath := filepath.Join(hostPackageDir, "bin", "secure_env")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("secure_env binary not found at %s: %w", binaryPath, err)
	}

	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("secure_env requires cuttlefish_config.json path")
	}

	// PrepareSecureEnvFds self-cleans on partial failure.
	fds, err := PrepareSecureEnvFds(sandboxHostDir)
	if err != nil {
		return Service{}, fmt.Errorf("prepare secure_env fds: %w", err)
	}

	// Inherited as fds 3..15 in slice order (Out-before-In).
	extraFiles := []*os.File{
		fds.KeymintOut,        // fd 3  -> --keymint_fd_in
		fds.KeymintIn,         // fd 4  -> --keymint_fd_out
		fds.KeymasterOut,      // fd 5  -> --keymaster_fd_in
		fds.KeymasterIn,       // fd 6  -> --keymaster_fd_out
		fds.GatekeeperOut,     // fd 7  -> --gatekeeper_fd_in
		fds.GatekeeperIn,      // fd 8  -> --gatekeeper_fd_out
		fds.OemlockOut,        // fd 9  -> --oemlock_fd_in
		fds.OemlockIn,         // fd 10 -> --oemlock_fd_out
		fds.JcardsimOut,       // fd 11 -> --jcardsim_fd_in  (Android 16+ only)
		fds.JcardsimIn,        // fd 12 -> --jcardsim_fd_out (Android 16+ only)
		fds.SnapshotControl,   // fd 13 -> --snapshot_control_fd
		fds.ConfuiServer.File, // fd 14 -> --confui_server_fd
		fds.KernelEvents,      // fd 15 -> --kernel_events_fd
	}
	for i, f := range extraFiles {
		if f == nil {
			return Service{}, fmt.Errorf("secure_env requires a non-nil fd at ExtraFiles index %d", i)
		}
	}

	// HOME is the writable sandbox dir (TPM state, secure index); the host
	// package is read-only, so ANDROID_ROOT/TZDATA_ROOT point at it.
	sandboxDir := filepath.Dir(cuttlefishConfigPath)
	env := []string{
		fmt.Sprintf("HOME=%s", sandboxDir),
		fmt.Sprintf("ANDROID_ROOT=%s", hostPackageDir),
		fmt.Sprintf("ANDROID_TZDATA_ROOT=%s", hostPackageDir),
		fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
		"CUTTLEFISH_INSTANCE=1",
	}

	args := []string{
		"--keymint_fd_in=3",
		"--keymint_fd_out=4",
		"--keymaster_fd_in=5",
		"--keymaster_fd_out=6",
		"--gatekeeper_fd_in=7",
		"--gatekeeper_fd_out=8",
		"--oemlock_fd_in=9",
		"--oemlock_fd_out=10",
		// fds 11/12 (jcardsim) unused on Android 15 but inherited to keep fd
		// numbering stable; Android 15 never reads them.
		"--snapshot_control_fd=13",
		"--confui_server_fd=14",
		"--kernel_events_fd=15",
		// Software keymint/gatekeeper, in-process TPM oemlock (no hardware).
		"--keymint_impl=software",
		"--gatekeeper_impl=software",
		"--oemlock_impl=tpm",
	}
	if vmm.AndroidVersion(androidVersion).SupportsJCardSim() {
		// Jcardsim has no stratovirt peer; disable the simulator.
		args = append(args,
			"--jcardsim_fd_in=11",
			"--jcardsim_fd_out=12",
			"--enable_jcard_simulator=0",
		)
	}

	// Unlink confui_sign.sock; CloseParentResources only closes the fd.
	confuiPath := fds.ConfuiServer.Path
	return Service{
		Name:       "secure_env",
		Binary:     binaryPath,
		Args:       args,
		NetNSName:  netNSName,
		Env:        env,
		ExtraFiles: extraFiles,
		Cleanup: func() {
			if rerr := os.Remove(confuiPath); rerr != nil && !os.IsNotExist(rerr) {
				_ = rerr // best-effort; sandbox dir cleanup will catch it
			}
		},
		RestartPolicy: RestartOnCrash,
	}, nil
}

// PrepareSecureEnvFds creates and opens the secure_env FIFOs in
// <sandboxHostDir>/android-pipes. Names must match stratovirt's
// buildAndroidVirtconsoleArgs pipeByPort.
func PrepareSecureEnvFds(sandboxHostDir string) (SecureEnvFds, error) {
	pipeDir := secureEnvPipeDir(sandboxHostDir)
	if err := os.MkdirAll(pipeDir, 0o700); err != nil {
		return SecureEnvFds{}, fmt.Errorf("create secure_env pipe dir %s: %w", pipeDir, err)
	}

	openFifo := func(name string, suffix string) (*os.File, error) {
		path := filepath.Join(pipeDir, name+suffix)
		if err := syscall.Mkfifo(path, 0o600); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("mkfifo %s: %w", path, err)
		}
		// O_RDWR returns immediately with no peer; blocking reads still work.
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open fifo %s: %w", path, err)
		}
		return f, nil
	}

	fds := SecureEnvFds{}
	var err error

	// Open .in then .out per pipe. Order must match ExtraFiles fd numbering.
	fifoPairs := []struct {
		name string
		in   **os.File
		out  **os.File
	}{
		{"keymint_fifo_vm", &fds.KeymintIn, &fds.KeymintOut},
		{"keymaster_fifo_vm", &fds.KeymasterIn, &fds.KeymasterOut},
		{"gatekeeper_fifo_vm", &fds.GatekeeperIn, &fds.GatekeeperOut},
		{"oemlock_fifo_vm", &fds.OemlockIn, &fds.OemlockOut},
		{"jcardsim_fifo_vm", &fds.JcardsimIn, &fds.JcardsimOut},
	}
	for _, p := range fifoPairs {
		if *p.in, err = openFifo(p.name, ".in"); err != nil {
			CloseSecureEnvFds(fds)
			return SecureEnvFds{}, err
		}
		if *p.out, err = openFifo(p.name, ".out"); err != nil {
			CloseSecureEnvFds(fds)
			return SecureEnvFds{}, err
		}
	}
	// Inert control FIFOs; ConfuiServer is a real listener (see SecureEnvFds).
	fds.SnapshotControl, err = openInertControlFifo(pipeDir, "snapshot_control_fifo")
	if err != nil {
		CloseSecureEnvFds(fds)
		return SecureEnvFds{}, err
	}
	fds.ConfuiServer, err = NewUnixListener(filepath.Join(sandboxHostDir, "confui_sign.sock"))
	if err != nil {
		CloseSecureEnvFds(fds)
		return SecureEnvFds{}, err
	}
	fds.KernelEvents, err = openInertControlFifo(pipeDir, "kernel_events_fifo")
	if err != nil {
		CloseSecureEnvFds(fds)
		return SecureEnvFds{}, err
	}
	return fds, nil
}

// openInertControlFifo creates a single O_RDWR FIFO used as an inert
// secure_env control fd (snapshot_control_fd, kernel_events_fd).
func openInertControlFifo(pipeDir, name string) (*os.File, error) {
	path := filepath.Join(pipeDir, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("mkfifo %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open inert control fifo %s: %w", path, err)
	}
	return f, nil
}

// CloseSecureEnvFds closes all non-nil fds. Idempotent and safe with a
// partially-populated SecureEnvFds. ConfuiServer.Close also unlinks the UDS.
func CloseSecureEnvFds(fds SecureEnvFds) {
	for _, f := range []*os.File{
		fds.KeymintOut, fds.KeymintIn,
		fds.KeymasterOut, fds.KeymasterIn,
		fds.GatekeeperOut, fds.GatekeeperIn,
		fds.OemlockOut, fds.OemlockIn,
		fds.JcardsimOut, fds.JcardsimIn,
		fds.SnapshotControl,
		fds.KernelEvents,
	} {
		if f != nil {
			_ = f.Close()
		}
	}
	if fds.ConfuiServer != nil {
		_ = fds.ConfuiServer.Close()
	}
}
