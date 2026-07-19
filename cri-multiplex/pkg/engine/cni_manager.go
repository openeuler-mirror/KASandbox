package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/containernetworking/cni/libcni"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netns"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type CNIManager struct {
	confDir  string
	binDir   string
	ifName   string
	netNSDir string
	prefix   string

	cniConfig *libcni.CNIConfig
	netConfig *libcni.NetworkConfigList
}

type CNIRecord struct {
	SandboxID  string
	Network    string
	IfName     string
	NetNSName  string
	NetNSPath  string
	PodIP      string
	Gateway    string
	DNS        []string
	ResultJSON []byte
}

func NewCNIManager(cfg CNIConfig) (*CNIManager, error) {
	if cfg.ConfDir == "" {
		cfg.ConfDir = "/etc/cni/net.d"
	}
	if cfg.BinDir == "" {
		cfg.BinDir = "/opt/cni/bin"
	}
	if cfg.IfName == "" {
		cfg.IfName = "eth0"
	}
	if cfg.NetNSDir == "" {
		cfg.NetNSDir = "/var/run/netns"
	}
	if cfg.NetNSPrefix == "" {
		cfg.NetNSPrefix = "e2b-"
	}

	netConfig, err := loadDefaultNetworkConf(cfg.ConfDir)
	if err != nil {
		return nil, fmt.Errorf("load cni config from %s: %w", cfg.ConfDir, err)
	}

	return &CNIManager{
		confDir:   cfg.ConfDir,
		binDir:    cfg.BinDir,
		ifName:    cfg.IfName,
		netNSDir:  cfg.NetNSDir,
		prefix:    cfg.NetNSPrefix,
		cniConfig: libcni.NewCNIConfig([]string{cfg.BinDir}, nil),
		netConfig: netConfig,
	}, nil
}

func loadDefaultNetworkConf(confDir string) (*libcni.NetworkConfigList, error) {
	files, err := libcni.ConfFiles(confDir, []string{".conflist"})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		files, err = libcni.ConfFiles(confDir, []string{".conf", ".json"})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, libcni.NoConfigsFoundError{Dir: confDir}
	}
	sort.Strings(files)
	if filepath.Ext(files[0]) == ".conflist" {
		return libcni.ConfListFromFile(files[0])
	}
	conf, err := libcni.ConfFromFile(files[0])
	if err != nil {
		return nil, err
	}
	return libcni.ConfListFromConf(conf)
}

func (m *CNIManager) Add(ctx context.Context, sandboxID string, podCfg *runtime.PodSandboxConfig) (*CNIRecord, error) {
	if podCfg == nil || podCfg.GetMetadata() == nil {
		return nil, fmt.Errorf("pod sandbox metadata is required for cni add")
	}

	netnsName := m.prefix + shortID(sandboxID)
	netnsPath := filepath.Join(m.netNSDir, netnsName)

	goruntime.LockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		goruntime.UnlockOSThread()
		return nil, fmt.Errorf("get host netns: %w", err)
	}
	netNS, err := netns.NewNamed(netnsName)
	if err != nil {
		_ = hostNS.Close()
		goruntime.UnlockOSThread()
		return nil, fmt.Errorf("create cni netns %s: %w", netnsPath, err)
	}
	if err = netns.Set(hostNS); err != nil {
		_ = netNS.Close()
		_ = hostNS.Close()
		goruntime.UnlockOSThread()
		_ = netns.DeleteNamed(netnsName)
		return nil, fmt.Errorf("restore host netns after creating %s: %w", netnsPath, err)
	}
	_ = hostNS.Close()
	goruntime.UnlockOSThread()
	defer netNS.Close()

	if err := ensureNetNSLoopbackUp(netnsName); err != nil {
		_ = netns.DeleteNamed(netnsName)
		return nil, fmt.Errorf("enable loopback for %s: %w", netnsPath, err)
	}

	rt := m.runtimeConf(sandboxID, netnsPath, podCfg)
	res, err := m.cniConfig.AddNetworkList(ctx, m.netConfig, rt)
	if err != nil {
		_ = netNS.Close()
		_ = netns.DeleteNamed(netnsName)
		return nil, fmt.Errorf("cni add %s: %w", sandboxID, err)
	}

	result, err := current.GetResult(res)
	if err != nil {
		_ = m.cniConfig.DelNetworkList(ctx, m.netConfig, rt)
		_ = netns.DeleteNamed(netnsName)
		return nil, fmt.Errorf("convert cni result %s: %w", sandboxID, err)
	}

	podIP, gateway := firstIPv4(result)
	if podIP == "" {
		_ = m.cniConfig.DelNetworkList(ctx, m.netConfig, rt)
		_ = netns.DeleteNamed(netnsName)
		return nil, fmt.Errorf("cni add %s did not return an IPv4 address", sandboxID)
	}

	resultJSON, _ := json.Marshal(result)
	return &CNIRecord{
		SandboxID:  sandboxID,
		Network:    m.netConfig.Name,
		IfName:     m.ifName,
		NetNSName:  netnsName,
		NetNSPath:  netnsPath,
		PodIP:      podIP,
		Gateway:    gateway,
		DNS:        append([]string(nil), result.DNS.Nameservers...),
		ResultJSON: resultJSON,
	}, nil
}

func (m *CNIManager) Del(ctx context.Context, rec *CNIRecord, podCfg *runtime.PodSandboxConfig) error {
	if rec == nil {
		return nil
	}

	rt := m.runtimeConf(rec.SandboxID, rec.NetNSPath, podCfg)
	err := m.cniConfig.DelNetworkList(ctx, m.netConfig, rt)
	unmountErr := netns.DeleteNamed(filepath.Base(rec.NetNSPath))
	if unmountErr != nil && !os.IsNotExist(unmountErr) {
		if err != nil {
			return fmt.Errorf("cni del %s: %w; unmount netns: %v", rec.SandboxID, err, unmountErr)
		}
		return fmt.Errorf("unmount netns %s: %w", rec.NetNSPath, unmountErr)
	}
	if err != nil {
		return fmt.Errorf("cni del %s: %w", rec.SandboxID, err)
	}
	return nil
}

func (m *CNIManager) runtimeConf(sandboxID string, netnsPath string, podCfg *runtime.PodSandboxConfig) *libcni.RuntimeConf {
	args := [][2]string{{"IgnoreUnknown", "1"}}
	if podCfg != nil && podCfg.GetMetadata() != nil {
		meta := podCfg.GetMetadata()
		args = append(args,
			[2]string{"K8S_POD_NAMESPACE", meta.GetNamespace()},
			[2]string{"K8S_POD_NAME", meta.GetName()},
			[2]string{"K8S_POD_INFRA_CONTAINER_ID", sandboxID},
			[2]string{"K8S_POD_UID", meta.GetUid()},
		)
	}

	return &libcni.RuntimeConf{
		ContainerID: sandboxID,
		NetNS:       netnsPath,
		IfName:      m.ifName,
		Args:        args,
	}
}

func firstIPv4(result *current.Result) (string, string) {
	for _, ipCfg := range result.IPs {
		ip := ipCfg.Address.IP.To4()
		if ip == nil {
			continue
		}
		gateway := ""
		if ipCfg.Gateway != nil {
			gateway = ipCfg.Gateway.String()
		}
		return ip.String(), gateway
	}
	return "", ""
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func ensureNetNSLoopbackUp(netnsName string) error {
	cmd := exec.Command("ip", "netns", "exec", netnsName, "ip", "link", "set", "lo", "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
