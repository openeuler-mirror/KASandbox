package fc

import "testing"

func TestIsFirecrackerCmdline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cmdline string
		want    bool
	}{
		{"/fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-abc.sock", true},
		{"ip netns exec ns-1 /fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-abc.sock", true},
		{"/usr/bin/python3 -s /usr/sbin/firewalld --nofork --nopid", false},
		{"/usr/bin/firecracker --version", false},
		{"grep --color=auto fir", false},
	}

	for _, tt := range tests {
		if got := isFirecrackerCmdline(tt.cmdline); got != tt.want {
			t.Fatalf("isFirecrackerCmdline(%q) = %v, want %v", tt.cmdline, got, tt.want)
		}
	}
}
