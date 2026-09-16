package targetstore

import "testing"

func TestMooncakeIgnoresInheritedStorageCapacitySettings(t *testing.T) {
	t.Setenv("MOONCAKE_GLOBAL_SEGMENT_SIZE", "1073741824")
	t.Setenv("MOONCAKE_MOUNT_SEGMENT_SIZE", "not-even-a-number")
	t.Setenv("MOONCAKE_LOCAL_BUFFER_SIZE", "134217728")
	t.Setenv("MOONCAKE_MASTER_ADDR", "master:50051")
	t.Setenv("MOONCAKE_PROTOCOL", "ub")
	cfg, err := readMooncakeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.masterAddr != "master:50051" || cfg.protocol != "ub" || cfg.localBufferSize != 134217728 {
		t.Fatalf("connection config = %+v", cfg)
	}
}
