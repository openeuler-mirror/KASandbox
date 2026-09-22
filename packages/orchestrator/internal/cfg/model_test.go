package cfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Run("embedded structs get defaults", func(t *testing.T) { //nolint:paralleltest // siblings set env, which may cause issues
		config, err := Parse()
		require.NoError(t, err)

		assert.Equal(t, "/fc-vm", config.SandboxDir)
	})

	t.Run("embedded structs get overrides", func(t *testing.T) {
		t.Setenv("SANDBOX_DIR", "/fc-vm2")

		config, err := Parse()
		require.NoError(t, err)

		assert.Equal(t, "/fc-vm2", config.SandboxDir)
	})

	t.Run("network config local flag defaults to false", func(t *testing.T) { //nolint:paralleltest // siblings set env, which may cause issues
		config, err := Parse()
		require.NoError(t, err)

		assert.False(t, config.NetworkConfig.UseLocalNamespaceStorage)
	})

	t.Run("network config is parsed correctly", func(t *testing.T) {
		t.Setenv("USE_LOCAL_NAMESPACE_STORAGE", "true")

		config, err := Parse()
		require.NoError(t, err)

		assert.True(t, config.NetworkConfig.UseLocalNamespaceStorage)
	})

	t.Run("multiple services parses correctly", func(t *testing.T) {
		t.Setenv("ORCHESTRATOR_SERVICES", "service1,service2")

		config, err := Parse()
		require.NoError(t, err)

		assert.Equal(t, []string{"service1", "service2"}, config.Services)
	})

	t.Run("env defaults get defaults before expansion", func(t *testing.T) { //nolint:paralleltest // siblings set env, which may cause issues
		config, err := Parse()
		require.NoError(t, err)
		assert.Equal(t, "/orchestrator/build", config.DefaultCacheDir)
	})

	t.Run("env defaults get expanded", func(t *testing.T) {
		t.Setenv("ORCHESTRATOR_BASE_PATH", "/a/b/c")
		config, err := Parse()
		require.NoError(t, err)
		assert.Equal(t, "/a/b/c/build", config.DefaultCacheDir)
		assert.Equal(t, "/a/b/c/sandbox", config.StorageConfig.SandboxCacheDir)
	})

	// Parse must run the network egress-proxy validation (the daemon bypasses
	// network.ParseConfig), otherwise SANDBOX_PROXY_* fail-fast rules and
	// CA_AUTO generation are dead code in the orchestrator daemon.
	t.Run("egress proxy validation rejects invalid values", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_CA_AUTO", "maybe")

		_, err := Parse()
		require.ErrorContains(t, err, "SANDBOX_PROXY_CA_AUTO")
	})

	t.Run("egress proxy validation rejects shared mode", func(t *testing.T) {
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "shared")

		_, err := Parse()
		require.ErrorContains(t, err, "SANDBOX_EGRESS_PROXY_MODE")
	})

	t.Run("egress proxy auto CA generates material", func(t *testing.T) {
		confdir := t.TempDir()
		addon := filepath.Join(t.TempDir(), "addon.py")
		require.NoError(t, os.WriteFile(addon, []byte("# addon"), 0o644))
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_CA_AUTO", "true")
		t.Setenv("SANDBOX_PROXY_CONFDIR", confdir)
		t.Setenv("SANDBOX_PROXY_ADDON", addon)

		_, err := Parse()
		require.NoError(t, err)

		// §7.5.1 代际化布局：两 PEM 落在 current symlink 指向的 gen 目录内。
		current, err := os.Readlink(filepath.Join(confdir, "current"))
		require.NoError(t, err)
		for _, name := range []string{"mitmproxy-ca.pem", "mitmproxy-ca-cert.pem"} {
			assert.FileExists(t, filepath.Join(confdir, current, name))
		}
	})
}
