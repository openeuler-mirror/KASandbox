package env

import "testing"

func TestValidLogsCollectorAddress(t *testing.T) {
	for _, tt := range []struct {
		address string
		valid   bool
	}{
		{"", false},
		{"localhost:30006", false},
		{"/relative", false},
		{"ftp://collector:30006", false},
		{"http://", false},
		{"http://collector:30006", true},
		{"https://collector/logs", true},
	} {
		t.Run(tt.address, func(t *testing.T) {
			t.Setenv("LOGS_COLLECTOR_ADDRESS", tt.address)
			address, valid := ValidLogsCollectorAddress()
			if valid != tt.valid || (valid && address != tt.address) || (!valid && address != "") {
				t.Fatalf("ValidLogsCollectorAddress() = %q, %v", address, valid)
			}
		})
	}
}
