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
	hugePages bool,
	incomingURI string,
) *CommandResult {
	kernelPath := filepath.Join(b.config.SandboxDir, versions.SandboxKernelDir(), SandboxKernelFile)
	rootfsPath := filepath.Join(b.config.SandboxDir, SandboxRootfsFile)
	hostKernelPath := versions.HostKernelPath(b.config)
	hostRootfsLinkPath := files.SandboxCacheRootfsLinkPath(b.config.StorageConfig)
	qmpSocket := files.SandboxFirecrackerSocketPath()
	serialLogPath := files.SandboxSerialLogPath()

	memArgs := fmt.Sprintf("-smp %d -m %d", vcpuCount, memoryMB)
	if hugePages {
		memArgs = fmt.Sprintf("-smp %d -m %d -mem-path /dev/hugepages -mem-prealloc", vcpuCount, memoryMB)
	}

	incomingArg := ""
	if incomingURI != "" {
		incomingArg = fmt.Sprintf("-incoming \"%s\" ", incomingURI)
	}

	machineType := "virt"
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "386" {
		machineType = "q35"
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
			"-pci virtio-net-pci,netdev=netdev,mac=%s "+
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
