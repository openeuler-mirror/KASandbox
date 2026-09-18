package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestParseConfigEgressProxyDefaultsDisabled(t *testing.T) {
	// Not parallel: mutates process env.

	cfg, err := ParseConfig()
	require.NoError(t, err)

	assert.Empty(t, cfg.SandboxEgressProxyMode)
	assert.Equal(t, uint16(15001), cfg.SandboxProxyListenPort)
	assert.Equal(t, "/opt/mitmproxy/mitmdump", cfg.SandboxProxyBinary)
	assert.Equal(t, "/opt/opensandbox-egress/addon.py", cfg.SandboxProxyAddon)
	assert.Equal(t, "/var/lib/cri-multiplex/egress-ca", cfg.SandboxProxyConfDir)
	assert.Equal(t, EgressProxyApplyReady, cfg.SandboxProxyApply)
	assert.Equal(t, EgressProxyRestartOnCrash, cfg.SandboxProxyRestart)
	assert.Empty(t, cfg.SandboxProxyExemptCIDRs)
	assert.Empty(t, cfg.SandboxProxyLogDir)
	assert.Empty(t, cfg.SandboxProxyUpstream)
}

// setupPerSandboxAddon points SANDBOX_PROXY_ADDON at a real temp file so the
// per-sandbox mode existence check passes.
func setupPerSandboxAddon(t *testing.T) {
	t.Helper()
	addon := filepath.Join(t.TempDir(), "addon.py")
	require.NoError(t, os.WriteFile(addon, []byte("# test addon"), 0o644))
	t.Setenv("SANDBOX_PROXY_ADDON", addon)
}

func TestParseConfigEgressProxyModes(t *testing.T) {
	// Not parallel: mutates process env.

	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{name: "empty mode disabled", mode: "", wantErr: false},
		{name: "per-sandbox accepted", mode: "per-sandbox", wantErr: false},
		{name: "shared not implemented", mode: "shared", wantErr: true},
		{name: "unknown mode rejected", mode: "gateway", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SANDBOX_EGRESS_PROXY_MODE", tt.mode)
			setupPerSandboxAddon(t)

			cfg, err := ParseConfig()
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.mode, cfg.SandboxEgressProxyMode)
		})
	}
}

func TestParseConfigEgressProxyPerSandboxChecks(t *testing.T) {
	// Not parallel: mutates process env.

	t.Run("missing addon fails", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_ADDON", filepath.Join(t.TempDir(), "does-not-exist.py"))

		_, err := ParseConfig()
		require.Error(t, err)
	})

	t.Run("zero listen port fails", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_LISTEN_PORT", "0")
		setupPerSandboxAddon(t)

		_, err := ParseConfig()
		require.Error(t, err)
	})

	t.Run("missing CA in confdir only warns", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_CONFDIR", filepath.Join(t.TempDir(), "no-such-dir"))
		setupPerSandboxAddon(t)

		_, err := ParseConfig()
		require.NoError(t, err, "missing CA material must not block startup")
	})

	t.Run("confdir with CA accepted", func(t *testing.T) {
		confdir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(confdir, "mitmproxy-ca.pem"), []byte("ca"), 0o644))
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_CONFDIR", confdir)
		setupPerSandboxAddon(t)

		_, err := ParseConfig()
		require.NoError(t, err)
	})
}

func TestParseConfigEgressProxyApplyAndRestart(t *testing.T) {
	// Not parallel: mutates process env.

	tests := []struct {
		name    string
		apply   string
		restart string
		wantErr bool
	}{
		{name: "apply immediate", apply: "immediate", wantErr: false},
		{name: "apply ready", apply: "ready", wantErr: false},
		{name: "apply bogus", apply: "bogus", wantErr: true},
		{name: "restart never", restart: "never", wantErr: false},
		{name: "restart on-crash", restart: "on-crash", wantErr: false},
		{name: "restart bogus", restart: "always", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.apply != "" {
				t.Setenv("SANDBOX_PROXY_APPLY", tt.apply)
			}
			if tt.restart != "" {
				t.Setenv("SANDBOX_PROXY_RESTART", tt.restart)
			}

			_, err := ParseConfig()
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestParseConfigEgressProxyExemptCIDRs(t *testing.T) {
	// Not parallel: mutates process env.

	t.Run("valid list", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_EXEMPT_CIDRS", "10.0.0.0/8, 172.16.0.0/12")

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8", " 172.16.0.0/12"}, cfg.SandboxProxyExemptCIDRs)
	})

	t.Run("invalid entry rejected", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_EXEMPT_CIDRS", "10.0.0.0/8,bogus")

		_, err := ParseConfig()
		require.Error(t, err)
	})
}

func TestParseConfigEgressProxyUpstream(t *testing.T) {
	// Not parallel: mutates process env.

	tests := []struct {
		name     string
		upstream string
		wantErr  bool
	}{
		{name: "empty allowed", upstream: "", wantErr: false},
		{name: "http with auth", upstream: "http://user:pass@10.233.0.1:8080", wantErr: false},
		{name: "https host", upstream: "https://proxy.example.com:3128", wantErr: false},
		{name: "bad scheme", upstream: "socks5://10.233.0.1:8080", wantErr: true},
		{name: "missing host", upstream: "http://:8080", wantErr: true},
		{name: "missing port", upstream: "http://10.233.0.1", wantErr: true},
		{name: "port zero", upstream: "http://10.233.0.1:0", wantErr: true},
		{name: "port out of range", upstream: "http://10.233.0.1:70000", wantErr: true},
		{name: "not a url", upstream: "10.233.0.1:8080", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SANDBOX_PROXY_UPSTREAM", tt.upstream)

			cfg, err := ParseConfig()
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.upstream, cfg.SandboxProxyUpstream)
		})
	}
}

func TestParseConfigEgressProxyPoolSize(t *testing.T) {
	// Not parallel: mutates process env.

	t.Run("mode off ignores the pool size", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_POOL_SIZE", "7")

		cfg, err := ParseConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg.SandboxProxyPoolSize)
		assert.Equal(t, 7, *cfg.SandboxProxyPoolSize)
		assert.Equal(t, 0, cfg.ProxySlotsPoolSize(), "mode off must keep the proxy sub-pool empty")
	})

	t.Run("per-sandbox defaults to 200 regardless of plain pool size", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("NETWORK_POOL_NEW_SLOTS_SIZE", "0")
		setupPerSandboxAddon(t)

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Nil(t, cfg.SandboxProxyPoolSize)
		assert.Equal(t, 200, cfg.ProxySlotsPoolSize())
	})

	t.Run("explicit override wins", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_POOL_SIZE", "5")
		setupPerSandboxAddon(t)

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Equal(t, 5, cfg.ProxySlotsPoolSize())
	})

	t.Run("explicit zero disables pre-warming", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_POOL_SIZE", "0")
		setupPerSandboxAddon(t)

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Equal(t, 0, cfg.ProxySlotsPoolSize())
	})

	t.Run("negative rejected", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_POOL_SIZE", "-1")

		_, err := ParseConfig()
		require.Error(t, err)
	})

	t.Run("non-integer rejected", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_POOL_SIZE", "abc")

		_, err := ParseConfig()
		require.Error(t, err)
	})

	t.Run("reused pool size parsed and negative rejected", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_REUSED_POOL_SIZE", "7")

		cfg, err := ParseConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg.SandboxProxyReusedPoolSize)
		assert.Equal(t, 7, *cfg.SandboxProxyReusedPoolSize)

		t.Setenv("SANDBOX_PROXY_REUSED_POOL_SIZE", "-1")

		_, err = ParseConfig()
		require.Error(t, err)
	})
}

func TestProxySlotsPoolSize(t *testing.T) {
	t.Parallel()

	five := 5

	assert.Equal(t, 0, Config{}.ProxySlotsPoolSize(), "mode off -> no proxy sub-pool")
	assert.Equal(t, 200, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, NewSlotsPoolSize: 0}.ProxySlotsPoolSize(), "unset falls back to the default, decoupled from the plain pool")
	assert.Equal(t, 200, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, NewSlotsPoolSize: 300}.ProxySlotsPoolSize())
	assert.Equal(t, 5, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, SandboxProxyPoolSize: &five}.ProxySlotsPoolSize())
	assert.Equal(t, 0, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, SandboxProxyPoolSize: new(int)}.ProxySlotsPoolSize())
}

func TestProxyReusedSlotsPoolSize(t *testing.T) {
	t.Parallel()

	five := 5

	assert.Equal(t, 0, Config{}.ProxyReusedSlotsPoolSize(), "mode off -> no proxy reuse pool")
	assert.Equal(t, 200, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, ReusedSlotsPoolSize: 1}.ProxyReusedSlotsPoolSize(), "unset falls back to the default, decoupled from the plain reuse pool")
	assert.Equal(t, 5, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, SandboxProxyReusedPoolSize: &five}.ProxyReusedSlotsPoolSize())
	assert.Equal(t, 0, Config{SandboxEgressProxyMode: EgressProxyModePerSandbox, SandboxProxyReusedPoolSize: new(int)}.ProxyReusedSlotsPoolSize())
}

func newProxyShapeSlot(t *testing.T, idx int) *Slot {
	t.Helper()

	slot, err := NewSlot("proxy", idx, Config{})
	require.NoError(t, err)
	slot.egressProxy = true

	return slot
}

func newProxyTestPool(t *testing.T, proxySize int) *Pool {
	t.Helper()

	storage, err := NewStorageMemory(16, Config{})
	require.NoError(t, err)

	cfg := Config{
		SandboxEgressProxyMode: EgressProxyModePerSandbox,
		SandboxProxyPoolSize:   &proxySize,
	}

	return NewPool(2, 2, storage, cfg)
}

func TestPoolProxyDisabledWhenModeOff(t *testing.T) {
	t.Parallel()

	storage, err := NewStorageMemory(16, Config{})
	require.NoError(t, err)

	pool := NewPool(2, 2, storage, Config{})
	assert.Equal(t, 0, pool.proxyNewSlotsSize, "mode off must keep the proxy sub-pool empty")
	assert.Equal(t, 0, cap(pool.proxyNewSlots))
	assert.Equal(t, 0, len(pool.proxyNewSlots))
	assert.Equal(t, 0, len(pool.proxyReusedSlots))
}

func TestPoolGetEgressProxySlotPrefersReused(t *testing.T) {
	t.Parallel()

	pool := newProxyTestPool(t, 2)

	reused := newProxyShapeSlot(t, 1)
	fresh := newProxyShapeSlot(t, 2)
	pool.proxyReusedSlots <- reused
	pool.proxyNewSlots <- fresh

	got, err := pool.GetEgressProxySlot(t.Context(), &orchestrator.SandboxNetworkConfig{})
	require.NoError(t, err)
	assert.Same(t, reused, got, "reused proxy slots must be drained before new ones")

	got, err = pool.GetEgressProxySlot(t.Context(), &orchestrator.SandboxNetworkConfig{})
	require.NoError(t, err)
	assert.Same(t, fresh, got)
}

func TestPoolReturnEgressProxySlotEntersProxyReusedPool(t *testing.T) {
	// Not parallel: stubs the package-level rule remover.

	orig := removeSlotEgressProxyRules
	removeSlotEgressProxyRules = func(context.Context, *Slot) error { return nil }
	t.Cleanup(func() { removeSlotEgressProxyRules = orig })

	pool := newProxyTestPool(t, 2)
	slot := newProxyShapeSlot(t, 3)

	require.NoError(t, pool.Return(t.Context(), slot))

	select {
	case got := <-pool.proxyReusedSlots:
		assert.Same(t, slot, got)
	default:
		t.Fatal("proxy slot was not returned to proxyReusedSlots")
	}
	assert.Equal(t, 0, len(pool.reusedSlots), "proxy slots must not enter the plain reuse pool")
}

func TestPoolReturnEgressProxySlotCleanupFailureDiscards(t *testing.T) {
	// Not parallel: stubs the package-level rule remover.

	orig := removeSlotEgressProxyRules
	removeSlotEgressProxyRules = func(context.Context, *Slot) error { return errors.New("boom") }
	t.Cleanup(func() { removeSlotEgressProxyRules = orig })

	storage, err := NewStorageMemory(16, Config{})
	require.NoError(t, err)

	proxySize := 2
	cfg := Config{
		SandboxEgressProxyMode: EgressProxyModePerSandbox,
		SandboxProxyPoolSize:   &proxySize,
	}
	pool := NewPool(2, 2, storage, cfg)

	slot, err := storage.Acquire(t.Context())
	require.NoError(t, err)
	slot.egressProxy = true

	err = pool.Return(t.Context(), slot)
	require.Error(t, err, "rule removal failure must not pool the slot")
	assert.Equal(t, 0, len(pool.proxyReusedSlots))

	// The slot must have been discarded: its storage index is free again.
	reacquired, err := storage.Acquire(t.Context())
	require.NoError(t, err)
	assert.Equal(t, slot.Idx, reacquired.Idx)
}

func TestPoolReturnPlainSlotUnaffectedByProxyPool(t *testing.T) {
	t.Parallel()

	pool := newProxyTestPool(t, 2)
	slot, err := NewSlot("plain", 4, Config{})
	require.NoError(t, err)

	require.NoError(t, pool.Return(t.Context(), slot))

	select {
	case got := <-pool.reusedSlots:
		assert.Same(t, slot, got)
	default:
		t.Fatal("plain slot was not returned to reusedSlots")
	}
	assert.Equal(t, 0, len(pool.proxyReusedSlots))
}
