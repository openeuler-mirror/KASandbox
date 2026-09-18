package hostservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
)

const (
	// egressProxyServiceNamePrefix prefixes the service name so proxy logs
	// piped into the orchestrator log are greppable as
	// service=egress-proxy-<sandboxID>.
	egressProxyServiceNamePrefix = "egress-proxy-"

	// egressProxyUpstreamOff is the egress-upstream annotation value that
	// disables the upstream cascade for one sandbox (direct egress after
	// local interception): SQUID_UPSTREAM_PROXY is then not injected at all.
	egressProxyUpstreamOff = "off"

	// egressProxyDefaultProfile is the addon's default SPOTBOX_PROFILE. Only the
	// internal distribution is supported (cri-multiplex rejects any other
	// annotation value), so the default must be internal as well.
	egressProxyDefaultProfile = "internal"

	// noMITMPlaceholderHost is a never-matching placeholder written into
	// header_whitelist.hosts of the derived no-MITM config. The whitelist must
	// never be emptied: hosts and domains both empty means "match every host"
	// (addon.py _is_header_whitelisted), i.e. full injection + full MITM —
	// the exact opposite of the intended disable.
	noMITMPlaceholderHost = "mitm-disabled.invalid"

	// noMITMConfigFilename is the derived per-sandbox MITMPROXY_CONFIG used
	// when egress-mitm=false.
	noMITMConfigFilename = "mitmproxy-config-nomitm.json"

	// defaultMitmproxyConfigPath mirrors addon.py's MITMPROXY_CONFIG default,
	// used to locate the node-shared config when the orchestrator process env
	// does not set it.
	defaultMitmproxyConfigPath = "/opt/opensandbox/bin/mitmproxy.json"

	// strategyCacheFilename and strategyTriggerFilename are the per-sandbox
	// paths of the addon's on-disk strategy cache and reload sentinel. Their
	// defaults are node-wide fixed paths, which would let concurrent proxy
	// instances overwrite each other's cache (and cross-warm identities) or
	// steal each other's reload sentinel — hence the mandatory per-sandbox
	// injection.
	strategyCacheFilename   = "strategy_cache.json"
	strategyTriggerFilename = "strategy-reload"
)

// EgressProxyConfig carries the node-level SANDBOX_PROXY_* settings needed to
// build one sandbox's egress proxy service.
type EgressProxyConfig struct {
	Binary   string // SANDBOX_PROXY_BINARY
	Addon    string // SANDBOX_PROXY_ADDON
	ConfDir  string // SANDBOX_PROXY_CONFDIR (mitmproxy CA confdir)
	Port     uint16 // SANDBOX_PROXY_LISTEN_PORT
	Restart  string // SANDBOX_PROXY_RESTART ("on-crash" / "never")
	LogDir   string // SANDBOX_PROXY_LOG_DIR (empty = zapio into orchestrator log)
	Upstream  string // SANDBOX_PROXY_UPSTREAM (node default upstream proxy URL)
	ExtraArgs string // SANDBOX_PROXY_EXTRA_ARGS (space-separated extra mitmdump args)

	// SandboxDir is the per-sandbox host directory; the strategy cache,
	// reload sentinel and the derived no-MITM config live under it and are
	// removed with the sandbox.
	SandboxDir string
}

// TCPNetNSReady is a ReadyCheck that TCP-dials an address from inside a
// network namespace (the proxy listens on 0.0.0.0 in the slot netns, so the
// probe must enter the netns to observe readiness).
type TCPNetNSReady struct {
	Addr      string
	NetNSPath string
}

func (r *TCPNetNSReady) Check(ctx context.Context) error {
	n, err := ns.GetNS(r.NetNSPath)
	if err != nil {
		return fmt.Errorf("open network namespace %s: %w", r.NetNSPath, err)
	}
	defer n.Close()

	return n.Do(func(_ ns.NetNS) error {
		dialer := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := dialer.DialContext(ctx, "tcp", r.Addr)
		if err != nil {
			return fmt.Errorf("dial %s in netns %s: %w", r.Addr, r.NetNSPath, err)
		}

		return conn.Close()
	})
}

func (r *TCPNetNSReady) String() string { return "tcp-netns:" + r.NetNSPath + ":" + r.Addr }

// BuildEgressProxyService builds the per-sandbox mitmproxy+addon proxy
// service running inside the sandbox's (named) slot netns:
//
//	mitmdump --mode transparent --listen-host 0.0.0.0 --listen-port <port> \
//	         -s <addon> --set confdir=<caDir>
//
// --listen-host 0.0.0.0 is required: the REDIRECT target address is the tap0
// netns-side address (169.254.0.22), not loopback.
//
// The identity and cascade configuration is injected via process env per
// sandbox (SANDBOX_MIS / SANDBOX_ID / SPOTBOX_PROFILE / SQUID_UPSTREAM_PROXY
// / the multi-instance isolation files / the derived no-MITM
// MITMPROXY_CONFIG); node-level shared config (MITMPROXY_CONFIG,
// SPOTBOX_STRATEGY_BASE_URL, SPOTBOX_SKILLS_FILE, ...) is inherited from the
// orchestrator process env by mergeProcessEnv, same as the sidecar model.
func BuildEgressProxyService(cfg EgressProxyConfig, slotNetNSName, sandboxID string, identity map[string]string) (Service, error) {
	if cfg.Binary == "" {
		return Service{}, fmt.Errorf("egress proxy requires a binary path")
	}
	if cfg.Addon == "" {
		return Service{}, fmt.Errorf("egress proxy requires an addon script path")
	}
	if cfg.Port == 0 {
		return Service{}, fmt.Errorf("egress proxy requires a listen port in 1-65535")
	}
	if slotNetNSName == "" {
		return Service{}, fmt.Errorf("egress proxy requires the slot netns name")
	}
	if cfg.SandboxDir == "" {
		return Service{}, fmt.Errorf("egress proxy requires the sandbox host directory")
	}

	// SandboxHostDir is created on demand by its first writer — the derived
	// no-MITM config below and the addon's strategy cache/reload sentinel all
	// live under it, so the directory must exist before the proxy starts.
	if err := os.MkdirAll(cfg.SandboxDir, 0o755); err != nil {
		return Service{}, fmt.Errorf("create sandbox host dir %s: %w", cfg.SandboxDir, err)
	}

	env := []string{
		"SANDBOX_MIS=" + identity[network.MetadataKeySandboxMIS],
		"SANDBOX_ID=" + sandboxID,
	}
	profile := identity[network.MetadataKeyEgressProfile]
	if profile == "" {
		profile = egressProxyDefaultProfile
	}
	env = append(env, "SPOTBOX_PROFILE="+profile)

	// Upstream cascade: the per-sandbox annotation overrides the node default;
	// "off" disables the cascade for this sandbox (env not injected at all);
	// neither configured = direct egress after local interception.
	if upstream, ok := identity[network.MetadataKeyEgressUpstream]; ok {
		if upstream != egressProxyUpstreamOff {
			env = append(env, "SQUID_UPSTREAM_PROXY="+upstream)
		}
	} else if cfg.Upstream != "" {
		env = append(env, "SQUID_UPSTREAM_PROXY="+cfg.Upstream)
	}

	// Multi-instance isolation: the addon's strategy cache and reload sentinel
	// default to node-wide fixed paths, so they must be per-sandbox.
	env = append(env,
		"SPOTBOX_STRATEGY_CACHE_FILE="+filepath.Join(cfg.SandboxDir, strategyCacheFilename),
		"SPOTBOX_STRATEGY_TRIGGER_FILE="+filepath.Join(cfg.SandboxDir, strategyTriggerFilename),
	)

	// MITM gating: enabled by default (the template is expected to carry the
	// proxy CA in the guest trust store) and leaves the node-shared
	// MITMPROXY_CONFIG inherited. Only an explicit egress-mitm=false derives a
	// per-sandbox config with a never-matching header whitelist placeholder to
	// disable header-triggered MITM.
	if identity[network.MetadataKeyEgressMitm] == "false" {
		derivedPath, err := deriveNoMITMConfig(cfg.SandboxDir)
		if err != nil {
			return Service{}, fmt.Errorf("derive no-MITM mitmproxy config: %w", err)
		}
		env = append(env, "MITMPROXY_CONFIG="+derivedPath)
	}

	args := []string{
		"--mode", "transparent",
		"--listen-host", "0.0.0.0",
		"--listen-port", strconv.Itoa(int(cfg.Port)),
		"-s", cfg.Addon,
		"--set", "confdir=" + cfg.ConfDir,
		// transparent 模式默认 connection_strategy=eager：客户端连接一建立
		// mitmproxy 就抢先直连 original dst，addon 的 next_layer 钩子再设
		// server.via 会抛 "Cannot change server.via on open connection"，
		// 导致 passthrough 层安装被中断（退化为 MITM）且级联上游永不生效。
		// lazy 把上游连接推迟到路由决策之后，via 才可正常设置。
		"--set", "connection_strategy=lazy",
	}
	// Deployment-specific extra mitmdump args (e.g. upstream verification of a
	// private-CA upstream/mock via "--set ssl_verify_upstream_trusted_ca=<pem>";
	// both upstream trust options REPLACE certifi, never append).
	if cfg.ExtraArgs != "" {
		args = append(args, strings.Fields(cfg.ExtraArgs)...)
	}

	svc := Service{
		Name:      egressProxyServiceNamePrefix + sandboxID,
		Binary:    cfg.Binary,
		Args:      args,
		NetNSName: slotNetNSName,
		Env:       env,
		ReadyCheck: &TCPNetNSReady{
			Addr:      net.JoinHostPort("127.0.0.1", strconv.Itoa(int(cfg.Port))),
			NetNSPath: network.NamedNetNSPath(slotNetNSName),
		},
	}

	switch cfg.Restart {
	case "", network.EgressProxyRestartOnCrash:
		svc.RestartPolicy = RestartOnCrash
	case network.EgressProxyRestartNever:
		svc.RestartPolicy = RestartNever
	default:
		return Service{}, fmt.Errorf("unknown egress proxy restart policy %q", cfg.Restart)
	}

	// Per-sandbox log file instead of zapio piping when SANDBOX_PROXY_LOG_DIR
	// is set. The file is caller-owned: closed via Cleanup (CloseParentResources
	// at StopAll / startup rollback), not per process (re)start.
	if cfg.LogDir != "" {
		if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
			return Service{}, fmt.Errorf("create egress proxy log dir %s: %w", cfg.LogDir, err)
		}
		logFile, err := os.OpenFile(filepath.Join(cfg.LogDir, sandboxID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return Service{}, fmt.Errorf("open egress proxy log file: %w", err)
		}
		svc.LogWriter = logFile
		svc.Cleanup = func() { _ = logFile.Close() }
	}

	return svc, nil
}

// deriveNoMITMConfig writes the per-sandbox no-MITM MITMPROXY_CONFIG: read the
// node-shared config (orchestrator env MITMPROXY_CONFIG, falling back to the
// addon's default path; a missing/unreadable file is treated as an empty
// config, mirroring the addon's own behavior), set header_whitelist hosts and
// domains to the never-matching placeholder (the addon treats either list
// matching as whitelisted), and atomically write it (tmp + rename)
// under the sandbox directory. Returns the derived file path.
func deriveNoMITMConfig(sandboxDir string) (string, error) {
	nodeConfigPath := os.Getenv("MITMPROXY_CONFIG")
	if nodeConfigPath == "" {
		nodeConfigPath = defaultMitmproxyConfigPath
	}

	config := map[string]any{}
	if data, err := os.ReadFile(nodeConfigPath); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return "", fmt.Errorf("parse node MITMPROXY_CONFIG %s: %w", nodeConfigPath, err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read node MITMPROXY_CONFIG %s: %w", nodeConfigPath, err)
	}

	whitelist, ok := config["header_whitelist"].(map[string]any)
	if !ok {
		whitelist = map[string]any{}
	}
	// addon's _is_header_whitelisted is OR semantics across hosts and domains
	// (addon.py:415-434), so both must become the never-matching placeholder —
	// preserving node domains would keep header-triggered MITM enabled.
	whitelist["hosts"] = []string{noMITMPlaceholderHost}
	whitelist["domains"] = []string{noMITMPlaceholderHost}
	config["header_whitelist"] = whitelist

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal derived no-MITM config: %w", err)
	}

	derivedPath := filepath.Join(sandboxDir, noMITMConfigFilename)
	tmpPath := derivedPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write derived no-MITM config: %w", err)
	}
	if err := os.Rename(tmpPath, derivedPath); err != nil {
		return "", fmt.Errorf("rename derived no-MITM config: %w", err)
	}

	return derivedPath, nil
}
