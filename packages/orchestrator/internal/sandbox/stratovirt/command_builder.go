package stratovirt

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type CommandBuilder struct {
	config cfg.BuilderConfig
}

func NewCommandBuilder(config cfg.BuilderConfig) *CommandBuilder {
	return &CommandBuilder{config: config}
}

type CommandResult struct {
	Value      string
	RootfsPath string
	KernelPath string
	QmpSocket  string
}

func (b *CommandBuilder) Build(
	versions Config,
	files *storage.SandboxFiles,
	slot *network.Slot,
	kernelArgs string,
	memoryMB int64,
	vcpuCount int64,
	_ bool,
	incomingURI string,
	vsockGuestCID int64,
) *CommandResult {
	kernelPath := filepath.Join(b.config.SandboxDir, versions.SandboxKernelDir(), SandboxKernelFile)
	rootfsPath := filepath.Join(b.config.SandboxDir, SandboxRootfsFile)
	hostKernelPath := versions.HostKernelPath(b.config)
	hostRootfsLinkPath := files.SandboxCacheRootfsLinkPath(b.config.StorageConfig)
	qmpSocket := files.SandboxFirecrackerSocketPath()
	serialLogPath := files.SandboxSerialLogPath()

	memArgs := fmt.Sprintf("-smp %d -m %d", vcpuCount, memoryMB)

	incomingArg := ""
	if incomingURI != "" {
		incomingArg = fmt.Sprintf("-incoming \"%s\" ", incomingURI)
	}

	machineType := "virt"
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "386" {
		machineType = "q35"
	}

	if versions.OsType.OrDefault() == vmm.OsWindows {
		return b.buildWindowsCommand(
			versions,
			rootfsPath,
			hostRootfsLinkPath,
			qmpSocket,
			slot,
			machineType,
			memArgs,
			incomingArg,
		)
	}
	if versions.OsType.OrDefault() == vmm.OsAndroid {
		return b.buildAndroidCommand(versions, files, slot, qmpSocket, memoryMB, vcpuCount, incomingArg, vsockGuestCID)
	}

	vsockArg := ""
	if vsockGuestCID > 0 {
		vsockArg = fmt.Sprintf("-device vhost-vsock-device,id=vsock0,guest-cid=%d ", vsockGuestCID)
	}

	cmd := fmt.Sprintf(
		"mount --make-rprivate / &&\n"+
			"mount -t tmpfs tmpfs %s -o X-mount.mkdir &&\n\n"+
			"ln -s %s %s &&\n"+
			"mkdir -p %s &&\n"+
			"ln -s %s %s &&\n\n"+
			"ip netns exec %s %s "+
			"-machine type=%s "+
			"-kernel %s "+
			"%s "+
			"-append \"%s\" "+
			"-object rng-random,id=objrng0,filename=/dev/urandom "+
			"-pci virtio-rng-pci,rng=objrng0,max-bytes=%d,period=%d "+
			"-drive file=%s,id=rootfs,readonly=off "+
			"-pci virtio-blk-pci,drive=rootfs,id=blk "+
			"-netdev tap,id=netdev,ifname=%s "+
			"-pci virtio-net-pci,netdev=netdev,id=%s,mac=%s "+
			"-qmp unix:%s,server,nowait "+
			"-serial file,path=%s "+
			"%s"+
			"%s",
		b.config.SandboxDir,
		hostRootfsLinkPath,
		rootfsPath,
		filepath.Join(b.config.SandboxDir, versions.SandboxKernelDir()),
		hostKernelPath,
		kernelPath,
		slot.NamespaceID(),
		versions.StratoVirtPath(b.config),
		machineType,
		kernelPath,
		memArgs,
		kernelArgs,
		entropyBytesSize,
		entropyRefillTime,
		rootfsPath,
		slot.TapName(),
		slot.VpeerName(),
		slot.TapMAC(),
		qmpSocket,
		serialLogPath,
		vsockArg,
		incomingArg,
	)

	return &CommandResult{
		Value:      cmd,
		RootfsPath: rootfsPath,
		KernelPath: kernelPath,
		QmpSocket:  qmpSocket,
	}
}

func (b *CommandBuilder) buildAndroidCommand(versions Config, files *storage.SandboxFiles, slot *network.Slot, qmpSocket string, memoryMB, vcpuCount int64, incomingArg string, vsockGuestCID int64) *CommandResult {
	diskNames := []string{storage.RootfsName, storage.PersistentName, storage.SDCardName}
	guestDiskPaths := make([]string, len(diskNames))
	var preamble strings.Builder
	preamble.WriteString("mount --make-rprivate / &&\n")
	fmt.Fprintf(&preamble, "mount -t tmpfs tmpfs %s -o X-mount.mkdir &&\n", b.config.SandboxDir)
	for i, name := range diskNames {
		guestDiskPaths[i] = filepath.Join(b.config.SandboxDir, fmt.Sprintf("android-disk-%d.raw", i))
		fmt.Fprintf(&preamble, "ln -s %s %s &&\n", files.SandboxCacheDiskLinkPath(b.config.StorageConfig, name), guestDiskPaths[i])
	}
	pipeDir := filepath.Join(b.config.SandboxDir, "android-pipes")
	fmt.Fprintf(&preamble, "mkdir -p %s &&\n", pipeDir)
	pipeNames := []string{"keymaster_fifo_vm", "gatekeeper_fifo_vm", "bt_fifo_vm", "gnsshvc_fifo_vm", "locationhvc_fifo_vm", "uwb_fifo_vm", "oemlock_fifo_vm", "keymint_fifo_vm", "nfc_fifo_vm", "sensors_control_fifo_vm", "sensors_data_fifo_vm"}
	for _, name := range pipeNames {
		path := filepath.Join(pipeDir, name)
		fmt.Fprintf(&preamble, "mkfifo -m 600 %s.in %s.out 2>/dev/null || true; ", path, path)
	}
	preamble.WriteString("\n")

	// Relocate the Android serial and logcat logs from /tmp to /tmp/templates so
	// all build-time logs land alongside the StratoVirt -D log.
	serialLogPath := filepath.Join("/tmp/templates", filepath.Base(files.SandboxSerialLogPath()))
	logcatPath := filepath.Join("/tmp/templates", filepath.Base(files.SandboxAndroidLogcatPath()))

	// Pre-compute the dynamic VMM argument groups so the main command can be
	// assembled via a single fmt.Sprintf template, mirroring buildWindowsCommand.
	diskArgs := buildAndroidDiskArgs(guestDiskPaths)
	virtconsoleArgs := buildAndroidVirtconsoleArgs(pipeDir, serialLogPath, logcatPath)

	vsockArg := ""
	if vsockGuestCID > 0 {
		vsockArg = fmt.Sprintf("-device vhost-vsock-pci,id=vsock0,guest-cid=%d,bus=pcie.0,addr=0x6 ", vsockGuestCID)
	}

	cmd := fmt.Sprintf(
		"ip netns exec %s %s "+
			"-D /tmp/templates/%s-%s.log "+
			"-machine virt,gic-version=3,dump-guest-core=off,mem-share=on "+
			"-accel kvm "+
			"-smp %d,cores=%d,threads=1 "+
			"-m size=%dM,maxmem=%dM "+
			"-uuid 699acfc4-c8c4-11e7-882b-%012x "+
			"-cpu host "+
			"-drive file=%s,if=pflash,unit=0,readonly=true "+
			"%s"+
			"-boot strict=on -msg timestamp=on -rtc base=utc "+
			"-device virtio-serial-pci,id=virtio-serial0,bus=pcie.0,addr=0x11,max-ports=31 "+
			"%s"+
			"%s"+
			"-device nec-usb-xhci,id=xhci,bus=pcie.0,addr=0x7 -device usb-tablet,id=tablet -device usb-kbd,id=kbd "+
			"-netdev tap,id=netdev0,ifname=%s -device virtio-net-pci,netdev=netdev0,id=eth1,bus=pcie.0,addr=0x8,mac=00:1a:11:e0:cf:00 "+
			"-netdev tap,id=netdev1,ifname=%s -device virtio-net-pci,netdev=netdev1,id=%s,bus=pcie.0,addr=0x9,mac=00:1a:11:e1:cf:00 "+
			"-device virtio-gpu-pci,id=gpu0,bus=pcie.0,addr=0x10,xres=720,yres=1280 -vnc 0.0.0.0:544 "+
			"-object rng-random,id=objrng0,filename=/dev/urandom -device virtio-rng-pci,id=rng0,rng=objrng0,bus=pcie.0,addr=0x5,max-bytes=1024,period=2000 "+
			"-qmp unix:%s,server,nowait "+
			"-serial file,path=%s "+
			"%s",
		slot.NamespaceID(),
		versions.StratoVirtPath(b.config),
		files.BuildID,
		files.SandboxID,
		vcpuCount,
		vcpuCount,
		memoryMB,
		memoryMB+4,
		slot.Idx,
		filepath.Join(b.config.FirmwareDir, androidBootloaderFile),
		diskArgs,
		virtconsoleArgs,
		vsockArg,
		slot.ExtraTapName(), // hostnet0 (netdev0) ifname = cvd-mtap
		slot.TapName(),      // hostnet1 (netdev1) ifname = tap0
		slot.VpeerName(),    // hostnet1 (netdev1) id = "eth0" (used by MMDS)
		qmpSocket,
		serialLogPath,
		incomingArg,
	)

	rootfsPath := guestDiskPaths[0]
	return &CommandResult{Value: preamble.String() + cmd, RootfsPath: rootfsPath, QmpSocket: qmpSocket}
}

// buildAndroidDiskArgs renders the three Android data disks (os, persistent,
// sdcard) as virtio-blk-pci devices. The first disk (os) gets bootindex=1 so
// the guest boots from it.
func buildAndroidDiskArgs(guestDiskPaths []string) string {
	var args strings.Builder
	for i, path := range guestDiskPaths {
		bootIndex := ""
		if i == 0 {
			bootIndex = ",bootindex=1"
		}
		fmt.Fprintf(&args, "-drive file=%s,format=raw,if=none,id=drive-virtio-disk%d,aio=threads -device virtio-blk-pci,drive=drive-virtio-disk%d,id=virtio-disk%d,bus=pcie.0,addr=0x%x%s ",
			path, i, i, i, i+2, bootIndex)
	}
	return args.String()
}

// buildAndroidVirtconsoleArgs renders the 31 virtio-serial ports used by the
// Android HALs. Ports 0 and 2 capture the kernel serial log and logcat to
// files; the ports listed in pipeByPort connect to FIFO pairs under pipeDir
// for HAL host↔guest IPC; the rest are left null.
func buildAndroidVirtconsoleArgs(pipeDir string, serialLogPath, logcatPath string) string {
	var args strings.Builder
	pipeByPort := map[int]string{
		3:  "keymaster_fifo_vm",
		4:  "gatekeeper_fifo_vm",
		5:  "bt_fifo_vm",
		6:  "gnsshvc_fifo_vm",
		7:  "locationhvc_fifo_vm",
		9:  "uwb_fifo_vm",
		10: "oemlock_fifo_vm",
		11: "keymint_fifo_vm",
		12: "nfc_fifo_vm",
		18: "sensors_control_fifo_vm",
		19: "sensors_data_fifo_vm",
	}
	for port := 0; port < 31; port++ {
		switch port {
		case 0:
			fmt.Fprintf(&args, "-chardev file,id=hvc0,path=%s -device virtconsole,id=hvc0,chardev=hvc0,nr=0 ", serialLogPath)
		case 2:
			fmt.Fprintf(&args, "-chardev file,id=hvc2,path=%s -device virtconsole,id=hvc2,chardev=hvc2,nr=2 ", logcatPath)
		default:
			if name, ok := pipeByPort[port]; ok {
				fmt.Fprintf(&args, "-chardev pipe,id=hvc%d,path=%s -device virtconsole,id=hvc%d,chardev=hvc%d,nr=%d ", port, filepath.Join(pipeDir, name), port, port, port)
			} else {
				fmt.Fprintf(&args, "-chardev null,id=hvc%d -device virtconsole,id=hvc%d,chardev=hvc%d,nr=%d ", port, port, port, port)
			}
		}
	}
	return args.String()
}

// buildWindowsCommand builds the StratoVirt invocation for a Windows guest.
//
// Unlike the Linux microvm path it boots from a UEFI pflash firmware (D7) rather
// than a kernel, drives a raw disk (D6), and exposes a graphical console through
// virtio-gpu + VNC with USB input. QMP is retained for MMDS injection and
// snapshots. It still launches inside the sandbox network namespace so the proxy
// and VNC listener stay isolated.
func (b *CommandBuilder) buildWindowsCommand(
	versions Config,
	rootfsPath string,
	hostRootfsLinkPath string,
	qmpSocket string,
	slot *network.Slot,
	machineType string,
	memArgs string,
	incomingArg string,
) *CommandResult {
	cmd := fmt.Sprintf(
		"mount --make-rprivate / &&\n"+
			"mount -t tmpfs tmpfs %s -o X-mount.mkdir &&\n\n"+
			"ln -s %s %s &&\n\n"+
			"ip netns exec %s %s "+
			"-machine type=%s "+
			"%s "+
			"-cpu host,pmu=on "+
			"-drive file=%s,if=pflash,unit=0,readonly=true "+
			"-drive file=%s,format=raw,id=disk,readonly=off,direct=off,aio=off "+
			"-device virtio-blk-pci,drive=disk,id=blk,bus=pcie.0,addr=0x2.0x0 "+
			"-device virtio-gpu-pci,id=gpu,bus=pcie.0,addr=0x7,xres=1280,yres=720 "+
			"-vnc 0.0.0.0:2 "+
			"-device nec-usb-xhci,id=xhci,bus=pcie.0,addr=0x9 "+
			"-device usb-kbd,id=kbd "+
			"-device usb-tablet,id=tablet "+
			"-netdev tap,id=netdev,ifname=%s "+
			"-device virtio-net-pci,netdev=netdev,id=%s,bus=pcie.0,addr=0x6,mac=%s "+
			"-qmp unix:%s,server,nowait "+
			"%s",
		b.config.SandboxDir,
		hostRootfsLinkPath,
		rootfsPath,
		slot.NamespaceID(),
		versions.StratoVirtPath(b.config),
		machineType,
		memArgs,
		filepath.Join(b.config.FirmwareDir, windowsUEFIFirmwareFile),
		rootfsPath,
		slot.TapName(),
		slot.VpeerName(), slot.TapMAC(),
		qmpSocket,
		incomingArg,
	)

	return &CommandResult{
		Value:      cmd,
		RootfsPath: rootfsPath,
		QmpSocket:  qmpSocket,
	}
}

func defaultKernelArgs(initScriptPath string, ipv4 string, options vmm.ProcessOptions) vmm.KernelArgs {
	args := vmm.KernelArgs{
		"quiet":    "",
		"loglevel": "1",

		"init": initScriptPath,

		"root":          "/dev/vda",
		"rw":            "",
		"ip":            ipv4,
		"ipv6.disable":  "0",
		"ipv6.autoconf": "1",

		"panic":                               "1",
		"systemd.journald.forward_to_console": "",
		"reboot":                              "k",
	}

	if runtime.GOARCH == "amd64" || runtime.GOARCH == "386" {
		args["pci"] = "off"
		args["i8042.nokbd"] = ""
		args["i8042.noaux"] = ""
		args["random.trust_cpu"] = "on"
		args["clocksource"] = "kvm-clock"
		args["console"] = "ttyS0"
	} else {
		args["console"] = "ttyAMA0"
	}

	if options.KernelLogs || options.SystemdToKernelLogs {
		delete(args, "quiet")
		args["loglevel"] = "5"
	}

	return args
}
