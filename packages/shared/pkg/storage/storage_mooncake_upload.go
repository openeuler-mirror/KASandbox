//go:build mooncake

package storage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
)

// mooncakeUploadConfig holds the configuration of the self-signed HTTP upload
// endpoint used by Mooncake storage.
type mooncakeUploadConfig struct {
	secret    []byte
	publicURL string
}

// newMooncakeUploadConfig reads the upload endpoint configuration. When
// MOONCAKE_UPLOAD_PUBLIC_ENDPOINT is not set it returns (nil, nil), which keeps
// UploadSignedURL returning an error.
func newMooncakeUploadConfig() (*mooncakeUploadConfig, error) {
	publicURL := env.GetEnv("MOONCAKE_UPLOAD_PUBLIC_ENDPOINT", "")
	if publicURL == "" {
		return nil, nil
	}

	secret := []byte(env.GetEnv("MOONCAKE_UPLOAD_SIGNING_SECRET", ""))
	if len(secret) == 0 {
		// Process-scoped random secret: InitLayerFileUpload is routed by nodeID, so
		// signing and verification happen in the same process and no external
		// configuration is needed. A process restart invalidates old URLs, which is
		// compatible with the 30min TTL semantics.
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("failed to generate upload signing secret: %w", err)
		}
	}

	return &mooncakeUploadConfig{
		secret:    secret,
		publicURL: strings.TrimSuffix(publicURL, "/"),
	}, nil
}

func (c *mooncakeUploadConfig) sign(path string, expires int64) string {
	mac := hmac.New(sha256.New, c.secret)
	fmt.Fprintf(mac, "PUT\n%s\n%d", path, expires)

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySignedUpload implements the SignedUploadVerifier interface.
func (s *mooncakeStorage) VerifySignedUpload(path string, expires int64, sig string) error {
	if s.upload == nil {
		return fmt.Errorf("mooncake signed upload is not configured")
	}
	if time.Now().Unix() > expires {
		return fmt.Errorf("signed URL expired")
	}
	if !hmac.Equal([]byte(s.upload.sign(path, expires)), []byte(sig)) {
		return fmt.Errorf("invalid signature")
	}

	return nil
}
