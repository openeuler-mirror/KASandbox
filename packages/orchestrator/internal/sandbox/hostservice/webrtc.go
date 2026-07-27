package hostservice

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

func BuildWebRTCOperatorService(config cfg.BuilderConfig, cuttlefishConfigPath string) (Service, error) {
	binaryPath := filepath.Join(config.CvdHostPackageDir, "bin", "webrtc_operator")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("webrtc_operator binary not found at %s: %w", binaryPath, err)
	}
	assetsDir := filepath.Join(config.CvdHostPackageDir, "usr", "share", "webrtc", "assets")
	certsDir := filepath.Join(config.CvdHostPackageDir, "usr", "share", "webrtc", "certs")
	return Service{
		Name:   "webrtc_operator:host-shared",
		Binary: binaryPath,
		Args: []string{
			fmt.Sprintf("-assets_dir=%s", assetsDir),
			fmt.Sprintf("-certs_dir=%s", certsDir),
			fmt.Sprintf("-http_server_port=%d", WebRTCOperatorPort),
			"-use_secure_http=true",
		},
		Env: []string{
			fmt.Sprintf("HOME=%s", config.CvdHostPackageDir),
			fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
		},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    &TCPPortReady{Address: fmt.Sprintf("127.0.0.1:%d", WebRTCOperatorPort)},
	}, nil
}

// BuildWebRTCService wires the exact Android 14 webRTC FD contract. The
// sockets are per-sandbox Unix listeners, so multiple streamers can coexist;
// the StratoVirt display/input bridge can connect to these paths independently.
func BuildWebRTCService(
	config cfg.BuilderConfig,
	sandboxID string,
	sandboxDir string,
	cuttlefishConfigPath string,
	_ int,
) (Service, error) {
	binaryPath := filepath.Join(config.CvdHostPackageDir, "bin", "webRTC")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("webRTC binary not found at %s: %w", binaryPath, err)
	}
	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("webRTC requires a runtime cuttlefish config")
	}

	workDir := filepath.Join(sandboxDir, "webrtc")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return Service{}, fmt.Errorf("create WebRTC work directory %s: %w", workDir, err)
	}

	type inheritedFD struct {
		flag string
		file *os.File
	}
	var inherited []inheritedFD
	var parentFiles []*os.File
	var socketPaths []string
	cleanup := func() {
		for _, item := range inherited {
			if item.file != nil {
				_ = item.file.Close()
			}
		}
		for _, file := range parentFiles {
			if file != nil {
				_ = file.Close()
			}
		}
		for _, path := range socketPaths {
			_ = os.Remove(path)
		}
	}

	addUnixListener := func(flag, name string, socketType int) error {
		path := filepath.Join(workDir, name)
		_ = os.Remove(path)
		fd, err := unix.Socket(unix.AF_UNIX, socketType|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("create %s listener: %w", flag, err)
		}
		if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("bind %s listener %s: %w", flag, path, err)
		}
		if err := unix.Listen(fd, 8); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("listen on %s: %w", path, err)
		}
		inherited = append(inherited, inheritedFD{flag: flag, file: os.NewFile(uintptr(fd), path)})
		socketPaths = append(socketPaths, path)
		return nil
	}

	addSocketPair := func(flag string) error {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("create %s socketpair: %w", flag, err)
		}
		parentFiles = append(parentFiles, os.NewFile(uintptr(fds[0]), flag+":parent"))
		inherited = append(inherited, inheritedFD{flag: flag, file: os.NewFile(uintptr(fds[1]), flag+":child")})
		return nil
	}

	addPipe := func(flag string) error {
		readFile, writeFile, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("create %s pipe: %w", flag, err)
		}
		parentFiles = append(parentFiles, writeFile)
		inherited = append(inherited, inheritedFD{flag: flag, file: readFile})
		return nil
	}

	addFIFO := func(flag, name string) error {
		path := filepath.Join(workDir, name)
		_ = os.Remove(path)
		if err := unix.Mkfifo(path, 0o600); err != nil {
			return fmt.Errorf("create %s FIFO: %w", flag, err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open %s FIFO: %w", flag, err)
		}
		inherited = append(inherited, inheritedFD{flag: flag, file: file})
		socketPaths = append(socketPaths, path)
		return nil
	}

	steps := []func() error{
		func() error { return addUnixListener("touch_fds", "touch.sock", unix.SOCK_STREAM) },
		func() error { return addUnixListener("keyboard_fd", "keyboard.sock", unix.SOCK_STREAM) },
		func() error { return addUnixListener("frame_server_fd", "frames.sock", unix.SOCK_STREAM) },
		func() error { return addUnixListener("audio_server_fd", "audio.sock", unix.SOCK_SEQPACKET) },
		func() error { return addFIFO("confui_in_fd", "confui_vm.in") },
		func() error { return addFIFO("confui_out_fd", "confui_vm.out") },
		func() error { return addSocketPair("command_fd") },
		func() error { return addPipe("kernel_log_events_fd") },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			cleanup()
			return Service{}, err
		}
	}

	extraFiles := make([]*os.File, 0, len(inherited))
	args := []string{
		"-write_virtio_input",
		fmt.Sprintf("-client_dir=%s", filepath.Join(config.CvdHostPackageDir, "usr", "share", "webrtc", "assets")),
	}
	for i, item := range inherited {
		extraFiles = append(extraFiles, item.file)
		args = append(args, fmt.Sprintf("-%s=%d", item.flag, 3+i))
	}

	env := []string{
		fmt.Sprintf("HOME=%s", config.CvdHostPackageDir),
		fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
		"http_proxy=",
		"https_proxy=",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
	}

	return Service{
		Name:        fmt.Sprintf("webRTC:%s", sandboxID),
		Binary:      binaryPath,
		Args:        args,
		Env:         env,
		ExtraFiles:  extraFiles,
		ParentFiles: parentFiles,
		Cleanup: func() {
			for _, path := range socketPaths {
				_ = os.Remove(path)
			}
		},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    NoReadyCheck{},
	}, nil
}
