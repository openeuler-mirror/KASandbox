package hostservice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
)

func egressProxyTestConfig(t *testing.T) EgressProxyConfig {
	t.Helper()

	return EgressProxyConfig{
		Binary:     "/opt/mitmproxy/mitmdump",
		Addon:      "/opt/opensandbox-egress/addon.py",
		ConfDir:    "/var/lib/cri-multiplex/egress-ca",
		Port:       15001,
		Restart:    network.EgressProxyRestartOnCrash,
		SandboxDir: t.TempDir(),
	}
}

func egressProxyTestIdentity() map[string]string {
	return map[string]string{
		network.MetadataKeySandboxMIS:    "mis-1",
		network.MetadataKeyEgressProfile: "internal",
		network.MetadataKeyEgressMitm:    "true",
	}
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range env {
		if len(item) > len(prefix) && item[:len(prefix)] == prefix {
			return item[len(prefix):], true
		}
	}

	return "", false
}

func TestBuildEgressProxyServiceShape(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.NoError(t, err)

	assert.Equal(t, "egress-proxy-sbx-1", svc.Name)
	assert.Equal(t, cfg.Binary, svc.Binary)
	assert.Equal(t, []string{
		"--mode", "transparent",
		"--listen-host", "0.0.0.0",
		"--listen-port", "15001",
		"-s", cfg.Addon,
		"--set", "confdir=" + cfg.ConfDir,
		// transparent 模式必须 lazy，否则 mitmproxy 抢先直连上游，
		// addon 设 server.via 抛 "Cannot change server.via on open connection"
		"--set", "connection_strategy=lazy",
	}, svc.Args)
}

func TestBuildEgressProxyServiceExtraArgs(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	cfg.ExtraArgs = "--set ssl_verify_upstream_trusted_ca=/etc/egress-ca/mitmproxy-ca-cert.pem"
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.NoError(t, err)

	assert.Equal(t, []string{
		"--mode", "transparent",
		"--listen-host", "0.0.0.0",
		"--listen-port", "15001",
		"-s", cfg.Addon,
		"--set", "confdir=" + cfg.ConfDir,
		"--set", "connection_strategy=lazy",
		"--set", "ssl_verify_upstream_trusted_ca=/etc/egress-ca/mitmproxy-ca-cert.pem",
	}, svc.Args)

	// NetNSName wrapping: startService execs `ip netns exec <name> <binary>`.
	assert.Equal(t, "ns-7", svc.NetNSName)

	ready, ok := svc.ReadyCheck.(*TCPNetNSReady)
	require.True(t, ok, "ReadyCheck must be a netns TCP probe, got %T", svc.ReadyCheck)
	assert.Equal(t, "127.0.0.1:15001", ready.Addr)
	assert.Equal(t, network.NamedNetNSPath("ns-7"), ready.NetNSPath)

	assert.Equal(t, RestartOnCrash, svc.RestartPolicy)
	assert.Nil(t, svc.LogWriter)
}

func TestBuildEgressProxyServiceIdentityEnv(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.NoError(t, err)

	mis, ok := envValue(svc.Env, "SANDBOX_MIS")
	require.True(t, ok)
	assert.Equal(t, "mis-1", mis)

	id, ok := envValue(svc.Env, "SANDBOX_ID")
	require.True(t, ok)
	assert.Equal(t, "sbx-1", id)

	profile, ok := envValue(svc.Env, "SPOTBOX_PROFILE")
	require.True(t, ok)
	assert.Equal(t, "internal", profile)

	// Multi-instance isolation: strategy cache and reload sentinel must be
	// per-sandbox paths under the sandbox dir.
	cacheFile, ok := envValue(svc.Env, "SPOTBOX_STRATEGY_CACHE_FILE")
	require.True(t, ok)
	assert.Equal(t, filepath.Join(cfg.SandboxDir, "strategy_cache.json"), cacheFile)

	triggerFile, ok := envValue(svc.Env, "SPOTBOX_STRATEGY_TRIGGER_FILE")
	require.True(t, ok)
	assert.Equal(t, filepath.Join(cfg.SandboxDir, "strategy-reload"), triggerFile)
}

func TestBuildEgressProxyServiceProfileDefault(t *testing.T) {
	t.Parallel()

	identity := egressProxyTestIdentity()
	delete(identity, network.MetadataKeyEgressProfile)

	svc, err := BuildEgressProxyService(egressProxyTestConfig(t), "ns-7", "sbx-1", identity)
	require.NoError(t, err)

	profile, ok := envValue(svc.Env, "SPOTBOX_PROFILE")
	require.True(t, ok)
	assert.Equal(t, "internal", profile)
}

func TestBuildEgressProxyServiceUpstreamMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		nodeUpstream  string
		annotation    string
		annotationSet bool
		wantUpstream  string
		wantInjected  bool
	}{
		{name: "node default injected", nodeUpstream: "http://10.233.0.1:8080", wantUpstream: "http://10.233.0.1:8080", wantInjected: true},
		{name: "annotation overrides node default", nodeUpstream: "http://10.233.0.1:8080", annotation: "http://user:pass@10.233.0.2:3128", annotationSet: true, wantUpstream: "http://user:pass@10.233.0.2:3128", wantInjected: true},
		{name: "annotation off disables injection", nodeUpstream: "http://10.233.0.1:8080", annotation: "off", annotationSet: true, wantInjected: false},
		{name: "nothing configured stays direct", wantInjected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := egressProxyTestConfig(t)
			cfg.Upstream = tt.nodeUpstream
			identity := egressProxyTestIdentity()
			if tt.annotationSet {
				identity[network.MetadataKeyEgressUpstream] = tt.annotation
			}

			svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", identity)
			require.NoError(t, err)

			upstream, ok := envValue(svc.Env, "SQUID_UPSTREAM_PROXY")
			assert.Equal(t, tt.wantInjected, ok)
			if tt.wantInjected {
				assert.Equal(t, tt.wantUpstream, upstream)
			}
		})
	}
}

func TestBuildEgressProxyServiceNoMITMDerivedConfig(t *testing.T) {
	// Not parallel: mutates process env (MITMPROXY_CONFIG).

	nodeConfig := `{
  "internal": {"hosts": ["internal.example.com"], "domains": [".corp"], "nets": ["10.0.0.0/8"]},
  "header_whitelist": {"hosts": ["api.example.com"], "domains": [".example.com"]},
  "header_blacklist": {"hosts": ["blocked.example.com"]}
}`
	nodeConfigPath := filepath.Join(t.TempDir(), "mitmproxy.json")
	require.NoError(t, os.WriteFile(nodeConfigPath, []byte(nodeConfig), 0o644))
	t.Setenv("MITMPROXY_CONFIG", nodeConfigPath)

	identity := egressProxyTestIdentity()
	identity[network.MetadataKeyEgressMitm] = "false"

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", identity)
	require.NoError(t, err)

	configPath, ok := envValue(svc.Env, "MITMPROXY_CONFIG")
	require.True(t, ok, "mitm=false must inject the derived MITMPROXY_CONFIG")
	assert.Equal(t, filepath.Join(cfg.SandboxDir, "mitmproxy-config-nomitm.json"), configPath)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var derived map[string]any
	require.NoError(t, json.Unmarshal(data, &derived))

	// The whitelist hosts and domains both become the never-matching
	// placeholder — the addon treats either list matching as whitelisted, so
	// preserving domains would keep MITM enabled. Never emptied, because an
	// empty hosts+domains pair means "match every host" (full injection +
	// full MITM) in addon.py.
	whitelist, ok := derived["header_whitelist"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"mitm-disabled.invalid"}, whitelist["hosts"])
	assert.Equal(t, []any{"mitm-disabled.invalid"}, whitelist["domains"], "whitelist domains must also be replaced (OR semantics)")

	// Untouched sections of the node config must survive the derivation.
	internal, ok := derived["internal"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"internal.example.com"}, internal["hosts"])
	blacklist, ok := derived["header_blacklist"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"blocked.example.com"}, blacklist["hosts"])

	// The derivation must be atomic: no leftover tmp file.
	_, err = os.Stat(configPath + ".tmp")
	assert.True(t, os.IsNotExist(err))
}

func TestBuildEgressProxyServiceNoMITMWithoutNodeConfig(t *testing.T) {
	// Not parallel: mutates process env (MITMPROXY_CONFIG).

	t.Setenv("MITMPROXY_CONFIG", filepath.Join(t.TempDir(), "missing.json"))

	identity := egressProxyTestIdentity()
	identity[network.MetadataKeyEgressMitm] = "false"

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", identity)
	require.NoError(t, err)

	configPath, ok := envValue(svc.Env, "MITMPROXY_CONFIG")
	require.True(t, ok)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var derived map[string]any
	require.NoError(t, json.Unmarshal(data, &derived))
	whitelist, ok := derived["header_whitelist"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"mitm-disabled.invalid"}, whitelist["hosts"])
}

func TestBuildEgressProxyServiceMITMTrueKeepsNodeConfig(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.NoError(t, err)

	_, ok := envValue(svc.Env, "MITMPROXY_CONFIG")
	assert.False(t, ok, "mitm=true must not override MITMPROXY_CONFIG (node-shared config is inherited)")

	_, err = os.Stat(filepath.Join(cfg.SandboxDir, "mitmproxy-config-nomitm.json"))
	assert.True(t, os.IsNotExist(err), "mitm=true must not write a derived config")
}

func TestBuildEgressProxyServiceMITMDefaultKeepsNodeConfig(t *testing.T) {
	t.Parallel()

	identity := egressProxyTestIdentity()
	delete(identity, network.MetadataKeyEgressMitm)

	cfg := egressProxyTestConfig(t)
	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", identity)
	require.NoError(t, err)

	_, ok := envValue(svc.Env, "MITMPROXY_CONFIG")
	assert.False(t, ok, "mitm unset defaults to true: MITMPROXY_CONFIG must be inherited, not overridden")

	_, err = os.Stat(filepath.Join(cfg.SandboxDir, "mitmproxy-config-nomitm.json"))
	assert.True(t, os.IsNotExist(err), "mitm unset defaults to true: no derived config")
}

func TestBuildEgressProxyServiceRestartPolicyMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		restart string
		want    RestartPolicy
		wantErr bool
	}{
		{restart: "on-crash", want: RestartOnCrash},
		{restart: "", want: RestartOnCrash},
		{restart: "never", want: RestartNever},
		{restart: "always", wantErr: true},
	}

	for _, tt := range tests {
		t.Run("restart="+tt.restart, func(t *testing.T) {
			t.Parallel()

			cfg := egressProxyTestConfig(t)
			cfg.Restart = tt.restart

			svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, svc.RestartPolicy)
		})
	}
}

func TestBuildEgressProxyServiceLogDir(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	cfg.LogDir = filepath.Join(t.TempDir(), "proxy-logs")

	svc, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.NoError(t, err)

	require.NotNil(t, svc.LogWriter, "LogDir set must replace zapio with a file writer")
	require.NotNil(t, svc.Cleanup)

	logPath := filepath.Join(cfg.LogDir, "sbx-1.log")
	require.FileExists(t, logPath)

	svc.CloseParentResources()

	_, err = svc.LogWriter.(*os.File).Write([]byte("x"))
	assert.Error(t, err, "Cleanup must close the log file")
}

func TestBuildEgressProxyServiceRequiresBasics(t *testing.T) {
	t.Parallel()

	cfg := egressProxyTestConfig(t)
	cfg.Binary = ""
	_, err := BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.Error(t, err)

	cfg = egressProxyTestConfig(t)
	cfg.Port = 0
	_, err = BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.Error(t, err)

	cfg = egressProxyTestConfig(t)
	_, err = BuildEgressProxyService(cfg, "", "sbx-1", egressProxyTestIdentity())
	require.Error(t, err)

	cfg = egressProxyTestConfig(t)
	cfg.SandboxDir = ""
	_, err = BuildEgressProxyService(cfg, "ns-7", "sbx-1", egressProxyTestIdentity())
	require.Error(t, err)
}
