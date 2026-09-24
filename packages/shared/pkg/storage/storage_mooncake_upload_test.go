//go:build mooncake

package storage

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestUploadStorage(t *testing.T) (*mooncakeStorage, *mooncakeUploadConfig) {
	t.Helper()

	cfg := &mooncakeUploadConfig{
		secret:    []byte("test-signing-secret"),
		publicURL: "http://127.0.0.1:5008",
	}

	return &mooncakeStorage{upload: cfg}, cfg
}

func TestMooncakeUploadSignature_Roundtrip(t *testing.T) {
	t.Parallel()

	storageProvider, cfg := newTestUploadStorage(t)
	path := "team-id/files/9f2c1e.tar"
	expires := time.Now().Add(30 * time.Minute).Unix()

	sig := cfg.sign(path, expires)
	require.NotEmpty(t, sig)
	require.NoError(t, storageProvider.VerifySignedUpload(path, expires, sig))

	// Signing is deterministic for the same path/expiry pair.
	assert.Equal(t, sig, cfg.sign(path, expires))
}

func TestMooncakeUploadSignature_RejectsTamperedInputs(t *testing.T) {
	t.Parallel()

	storageProvider, cfg := newTestUploadStorage(t)
	path := "team-id/files/9f2c1e.tar"
	expires := time.Now().Add(30 * time.Minute).Unix()
	sig := cfg.sign(path, expires)

	otherSecret := &mooncakeUploadConfig{secret: []byte("another-secret")}

	testCases := []struct {
		name    string
		path    string
		expires int64
		sig     string
	}{
		{name: "tampered path", path: "team-id/files/other.tar", expires: expires, sig: sig},
		{name: "tampered expires", path: path, expires: expires + 60, sig: sig},
		{name: "signature from other secret", path: path, expires: expires, sig: otherSecret.sign(path, expires)},
		{name: "empty signature", path: path, expires: expires, sig: ""},
		{name: "expired", path: path, expires: time.Now().Add(-time.Minute).Unix(), sig: cfg.sign(path, time.Now().Add(-time.Minute).Unix())},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Error(t, storageProvider.VerifySignedUpload(tc.path, tc.expires, tc.sig))
		})
	}
}

func TestMooncakeUploadSignature_NotConfigured(t *testing.T) {
	t.Parallel()

	storageProvider := &mooncakeStorage{}

	assert.Error(t, storageProvider.VerifySignedUpload("path", time.Now().Add(time.Minute).Unix(), "sig"))
}

func TestNewMooncakeUploadConfig_FromLocalHostnameAndPort(t *testing.T) {
	t.Setenv("MOONCAKE_LOCAL_HOSTNAME", "10.0.0.12")
	t.Setenv("GRPC_PORT", "5008")
	t.Setenv("MOONCAKE_UPLOAD_SIGNING_SECRET", "")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// The endpoint reuses the node address and gRPC port, no separate public
	// endpoint configuration is needed.
	assert.Equal(t, "http://10.0.0.12:5008", cfg.publicURL)
	// Without an explicit secret a random 32 byte process-scoped key is used.
	assert.Len(t, cfg.secret, 32)
}

func TestNewMooncakeUploadConfig_Defaults(t *testing.T) {
	t.Setenv("MOONCAKE_LOCAL_HOSTNAME", "")
	t.Setenv("GRPC_PORT", "")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "http://localhost:5008", cfg.publicURL)
}

func TestNewMooncakeUploadConfig_ExplicitSecret(t *testing.T) {
	t.Setenv("MOONCAKE_LOCAL_HOSTNAME", "10.0.0.12")
	t.Setenv("GRPC_PORT", "6000")
	t.Setenv("MOONCAKE_UPLOAD_SIGNING_SECRET", "shared-secret")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "http://10.0.0.12:6000", cfg.publicURL)
	assert.Equal(t, []byte("shared-secret"), cfg.secret)
}

func TestNewMooncakeUploadConfig_InvalidPort(t *testing.T) {
	t.Setenv("MOONCAKE_LOCAL_HOSTNAME", "10.0.0.12")
	t.Setenv("GRPC_PORT", "not-a-port")

	_, err := newMooncakeUploadConfig()
	assert.Error(t, err)
}

func TestMooncakeUploadSignedURL(t *testing.T) {
	cfg := &mooncakeUploadConfig{
		secret:    []byte("test-signing-secret"),
		publicURL: "http://10.0.0.12:5008",
	}
	provider := &mooncakeStorage{upload: cfg}

	raw, err := provider.UploadSignedURL(context.Background(), "team-id/files/9f2c1e.tar", 30*time.Minute)
	require.NoError(t, err)

	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.12:5008", parsed.Scheme+"://"+parsed.Host)
	assert.Equal(t, SignedUploadPath, parsed.Path)

	query := parsed.Query()
	assert.Equal(t, "team-id/files/9f2c1e.tar", query.Get("path"))

	expires, err := strconv.ParseInt(query.Get("expires"), 10, 64)
	require.NoError(t, err)
	assert.Greater(t, expires, time.Now().Unix())

	require.NoError(t, provider.VerifySignedUpload(query.Get("path"), expires, query.Get("sig")))
}
