//go:build mooncake

package storage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
)

// defaultMooncakeUploadPort matches the default orchestrator gRPC port, on which
// the signed-upload HTTP route is multiplexed through cmux.
const defaultMooncakeUploadPort = 5008

// mooncakeUploadConfig holds the configuration of the self-signed HTTP upload
// endpoint used by Mooncake storage.
type mooncakeUploadConfig struct {
	secret    []byte
	publicURL string
}

// newMooncakeUploadConfig builds the upload endpoint from the node host used by
// Mooncake (MOONCAKE_LOCAL_HOSTNAME) and the orchestrator gRPC port (GRPC_PORT),
// which also serves the signed-upload HTTP route through cmux. Reusing the node
// address means the URL is already reachable by clients without a dedicated
// public endpoint configuration.
func newMooncakeUploadConfig() (*mooncakeUploadConfig, error) {
	host := env.GetEnv("MOONCAKE_LOCAL_HOSTNAME", "localhost")
	port, err := env.GetEnvAsInt("GRPC_PORT", defaultMooncakeUploadPort)
	if err != nil {
		return nil, fmt.Errorf("invalid GRPC_PORT: %w", err)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("GRPC_PORT must be between 1 and 65535, got %d", port)
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
		publicURL: "http://" + net.JoinHostPort(host, strconv.Itoa(port)),
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
