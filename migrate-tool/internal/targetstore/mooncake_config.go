package targetstore

import (
	"fmt"
	"os"
	"strconv"
)

// An import is a short-lived client, never a storage node. GLOBAL_SEGMENT_SIZE
// and MOUNT_SEGMENT_SIZE are deliberately not configurable here, even when the
// shell inherited a long-running server's environment.
type mooncakeConfig struct {
	localHostname, metadataServer, protocol, deviceName, masterAddr string
	localBufferSize                                                 uint64
}

func readMooncakeConfig() (mooncakeConfig, error) {
	// Match the runtime's default transfer buffer in
	// packages/shared/pkg/storage/storage_mooncake.go (128 MiB).
	// This buffer does not contribute storage capacity; the environment may override it.
	buffer, err := strconv.ParseUint(mooncakeEnv("MOONCAKE_LOCAL_BUFFER_SIZE", "134217728"), 10, 64)
	if err != nil || buffer == 0 {
		return mooncakeConfig{}, fmt.Errorf("MOONCAKE_LOCAL_BUFFER_SIZE must be a positive byte count")
	}
	return mooncakeConfig{
		localHostname:   mooncakeEnv("MOONCAKE_LOCAL_HOSTNAME", "localhost"),
		metadataServer:  mooncakeEnv("MOONCAKE_METADATA_SERVER", "http://localhost:8080/metadata"),
		protocol:        mooncakeEnv("MOONCAKE_PROTOCOL", "tcp"),
		deviceName:      mooncakeEnv("MOONCAKE_DEVICE_NAME", ""),
		masterAddr:      mooncakeEnv("MOONCAKE_MASTER_ADDR", "localhost:50051"),
		localBufferSize: buffer,
	}, nil
}

func mooncakeEnv(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}
