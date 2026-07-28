//go:build windows

package port

import "testing"

func TestForwardDisabled(t *testing.T) {
	t.Setenv(forwardDisabledEnv, "true")

	if !forwardDisabled() {
		t.Fatal("forwardDisabled() = false, want true")
	}
}

func TestForwardBindIP(t *testing.T) {
	t.Setenv(forwardBindIPEnv, "192.0.2.10")

	if got := forwardBindIP(); got != "192.0.2.10" {
		t.Fatalf("forwardBindIP() = %q, want %q", got, "192.0.2.10")
	}
}

func TestForwardBindIPDefault(t *testing.T) {
	t.Setenv(forwardBindIPEnv, "")

	if got := forwardBindIP(); got != defaultForwardBindIP {
		t.Fatalf("forwardBindIP() = %q, want %q", got, defaultForwardBindIP)
	}
}

func TestForwardIgnoredPorts(t *testing.T) {
	t.Setenv(forwardIgnorePortsEnv, "49983, 3000, invalid, 70000")

	ignored := forwardIgnoredPorts()
	if _, ok := ignored[49983]; !ok {
		t.Fatal("expected 49983 to be ignored")
	}
	if _, ok := ignored[3000]; !ok {
		t.Fatal("expected 3000 to be ignored")
	}
	if _, ok := ignored[70000]; ok {
		t.Fatal("did not expect invalid port 70000 to be ignored")
	}
}

func TestForwardIgnoredPortsDefaultIsEmpty(t *testing.T) {
	t.Setenv(forwardIgnorePortsEnv, "")

	if got := forwardIgnoredPorts(); len(got) != 0 {
		t.Fatalf("forwardIgnoredPorts() len = %d, want 0", len(got))
	}
}
