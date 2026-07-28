package stratovirt

import (
	"fmt"
	"path/filepath"
	"runtime"

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
		incomingArg,
	)

	return &CommandResult{
		Value:      cmd,
		RootfsPath: rootfsPath,
		KernelPath: kernelPath,
		QmpSocket:  qmpSocket,
	}
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
