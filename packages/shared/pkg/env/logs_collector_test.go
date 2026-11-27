package env

import "testing"

func TestValidLogsCollectorAddress(t *testing.T) {
	for _, tt := range []struct {
		value   string
		address string
		valid   bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"localhost:30006", "", false},
		{"/relative", "", false},
		{"ftp://collector:30006", "", false},
		{"http://", "", false},
		{"http://collector:30006", "http://collector:30006", true},
		{"https://collector/logs", "https://collector/logs", true},
		{" http://collector:30006\n", "http://collector:30006", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("LOGS_COLLECTOR_ADDRESS", tt.value)
			address, valid := ValidLogsCollectorAddress()
			if valid != tt.valid || address != tt.address {
				t.Fatalf("ValidLogsCollectorAddress() = %q, %v, want %q, %v", address, valid, tt.address, tt.valid)
			}
		})
	}
}
