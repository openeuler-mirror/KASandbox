package network

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseProxyCACert 从 confdir 的纯 cert 文件解析出 X509 证书。
func parseProxyCACert(t *testing.T, confdir string) *x509.Certificate {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block, "cert file must be PEM")
	require.Equal(t, "CERTIFICATE", block.Type)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	return cert
}

func TestEnsureProxyCAGeneratesOnEmptyConfdir(t *testing.T) {
	t.Parallel()

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))

	certPath := filepath.Join(confdir, proxyCACertFile)
	combinedPath := filepath.Join(confdir, proxyCACombinedFile)

	cert := parseProxyCACert(t, confdir)
	assert.Equal(t, proxyCACN, cert.Subject.CommonName)
	assert.True(t, cert.IsCA, "CA:TRUE")
	assert.True(t, cert.BasicConstraintsValid)
	assert.Equal(t, x509.KeyUsageCertSign|x509.KeyUsageCRLSign, cert.KeyUsage)
	assert.True(t, cert.NotAfter.After(time.Now().AddDate(proxyCAValidityYears-1, 0, 0)),
		"validity must be ~%d years", proxyCAValidityYears)
	assert.True(t, cert.NotAfter.Before(time.Now().AddDate(proxyCAValidityYears+1, 0, 0)))
	assert.Positive(t, cert.SerialNumber.Sign(), "random non-zero serial")

	// 自签：cert 可由自身公钥验证。
	require.NoError(t, cert.CheckSignatureFrom(cert))

	// 合并文件 = cert + key（mitmproxy confdir 约定），权限 0600；纯 cert 0644。
	combined, err := os.ReadFile(combinedPath)
	require.NoError(t, err)
	assert.True(t, bytes.Contains(combined, []byte("PRIVATE KEY")), "combined file must carry the key")
	assert.True(t, proxyCACombinedValid(combined))
	certData, err := os.ReadFile(certPath)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(combined, certData), "combined must start with the cert block")

	certStat, err := os.Stat(certPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), certStat.Mode().Perm())
	combinedStat, err := os.Stat(combinedPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), combinedStat.Mode().Perm())

	// openssl 可解析性等价校验：PEM 证书可被标准库完整解析且无尾随垃圾。
	block, rest := pem.Decode(certData)
	require.NotNil(t, block)
	assert.Empty(t, bytes.TrimSpace(rest), "cert file must contain exactly one PEM block")

	// SKID 必须是 RFC 5280 method 1（SHA-1 over PKCS#1 DER），与
	// openssl/mitmproxy 从 issuer 公钥推导 AKID 的口径一致；Go ≥1.25 默认的
	// SHA-256 截断 SKID 会导致下游 AKID 与 CA SKID 不匹配（openssl3 客户端
	// 严格校验报 "authority and subject key identifier mismatch"）。
	rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
	require.True(t, ok, "CA public key must be RSA")
	wantSKID := sha1.Sum(x509.MarshalPKCS1PublicKey(rsaPub))
	assert.Equal(t, wantSKID[:], cert.SubjectKeyId, "SKID must be RFC 5280 method 1")
}

func TestEnsureProxyCAIdempotentSkips(t *testing.T) {
	t.Parallel()

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))

	certBefore, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	combinedBefore, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)

	require.NoError(t, ensureProxyCA(confdir))

	certAfter, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	combinedAfter, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)

	assert.Equal(t, certBefore, certAfter, "second run must not regenerate the cert")
	assert.Equal(t, combinedBefore, combinedAfter, "second run must not regenerate the key")
}

func TestEnsureProxyCAHalfStateCombinedOnly(t *testing.T) {
	t.Parallel()

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))

	combined, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(confdir, proxyCACertFile)))

	// 半文件（只有 cert+key 合并 PEM）：cert 从合并 PEM 重导出，不重生成 key。
	require.NoError(t, ensureProxyCA(confdir))

	certData, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	assert.Equal(t, proxyCACertBlock(combined), certData, "cert must be re-exported from the combined PEM")

	combinedAfter, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)
	assert.Equal(t, combined, combinedAfter, "combined file (key) must be untouched")
}

func TestEnsureProxyCAHalfStateCertOnlyRegenerates(t *testing.T) {
	t.Parallel()

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	oldCert, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)

	// 只剩纯 cert：私钥不可恢复，整套重生成（cert 与 key 必须同源）。
	require.NoError(t, os.Remove(filepath.Join(confdir, proxyCACombinedFile)))
	require.NoError(t, ensureProxyCA(confdir))

	combined, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)
	assert.True(t, proxyCACombinedValid(combined))
	newCert, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(combined, newCert), "cert file and combined file must be the same CA")
	assert.NotEqual(t, oldCert, newCert, "cert-only half state cannot recover the key and must regenerate")
}

func TestEnsureProxyCACorruptRegenerates(t *testing.T) {
	t.Parallel()

	confdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(confdir, proxyCACertFile), []byte("garbage"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(confdir, proxyCACombinedFile), []byte("garbage"), 0o600))

	require.NoError(t, ensureProxyCA(confdir))
	cert := parseProxyCACert(t, confdir)
	assert.Equal(t, proxyCACN, cert.Subject.CommonName)
}

func TestEnsureProxyCAUnwritableConfdirFails(t *testing.T) {
	t.Parallel()

	t.Run("confdir path occupied by a file", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		confdir := filepath.Join(base, "confdir")
		require.NoError(t, os.WriteFile(confdir, []byte("not a dir"), 0o644))

		require.Error(t, ensureProxyCA(confdir))
	})

	t.Run("read-only confdir", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root 不受目录权限位约束，跳过只读 confdir 用例")
		}

		confdir := t.TempDir()
		require.NoError(t, os.Chmod(confdir, 0o555))

		require.Error(t, ensureProxyCA(confdir))
	})
}

func TestParseConfigCAAutoValidation(t *testing.T) {
	// Not parallel: mutates process env.

	t.Run("default off leaves confdir untouched", func(t *testing.T) {
		confdir := t.TempDir()
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_CONFDIR", confdir)
		setupPerSandboxAddon(t)

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Equal(t, "false", cfg.SandboxProxyCAAuto)
		assert.False(t, cfg.ProxyCAAutoEnabled())

		entries, err := os.ReadDir(confdir)
		require.NoError(t, err)
		assert.Empty(t, entries, "开关关闭时 confdir 必须保持原样（现状逐字节不变）")
	})

	t.Run("invalid value fails fast", func(t *testing.T) {
		t.Setenv("SANDBOX_PROXY_CA_AUTO", "yes")

		_, err := ParseConfig()
		require.Error(t, err)
	})

	t.Run("enabled generates CA into confdir", func(t *testing.T) {
		confdir := t.TempDir()
		t.Setenv("SANDBOX_EGRESS_PROXY_MODE", "per-sandbox")
		t.Setenv("SANDBOX_PROXY_CONFDIR", confdir)
		t.Setenv("SANDBOX_PROXY_CA_AUTO", "true")
		setupPerSandboxAddon(t)

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.True(t, cfg.ProxyCAAutoEnabled())

		cert := parseProxyCACert(t, confdir)
		assert.Equal(t, proxyCACN, cert.Subject.CommonName)
	})

	t.Run("enabled without per-sandbox mode does not touch confdir", func(t *testing.T) {
		confdir := t.TempDir()
		t.Setenv("SANDBOX_PROXY_CONFDIR", confdir)
		t.Setenv("SANDBOX_PROXY_CA_AUTO", "true")

		_, err := ParseConfig()
		require.NoError(t, err)

		entries, err := os.ReadDir(confdir)
		require.NoError(t, err)
		assert.Empty(t, entries, "非 per-sandbox 模式下不生成 CA")
	})
}
