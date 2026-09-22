//go:build mooncake

package storage

import (
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

func TestNewMooncakeUploadConfig_DisabledWithoutEndpoint(t *testing.T) {
	t.Setenv("MOONCAKE_UPLOAD_PUBLIC_ENDPOINT", "")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestNewMooncakeUploadConfig_Defaults(t *testing.T) {
	t.Setenv("MOONCAKE_UPLOAD_PUBLIC_ENDPOINT", "http://10.0.0.12:5008/")
	t.Setenv("MOONCAKE_UPLOAD_SIGNING_SECRET", "")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Trailing slash is trimmed so URLs never contain a double slash.
	assert.Equal(t, "http://10.0.0.12:5008", cfg.publicURL)
	// Without an explicit secret a random 32 byte process-scoped key is used.
	assert.Len(t, cfg.secret, 32)
}

func TestNewMooncakeUploadConfig_ExplicitSecret(t *testing.T) {
	t.Setenv("MOONCAKE_UPLOAD_PUBLIC_ENDPOINT", "http://10.0.0.12:5008")
	t.Setenv("MOONCAKE_UPLOAD_SIGNING_SECRET", "shared-secret")

	cfg, err := newMooncakeUploadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, []byte("shared-secret"), cfg.secret)
}
