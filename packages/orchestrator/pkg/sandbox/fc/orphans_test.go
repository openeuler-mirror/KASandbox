package fc

import "testing"

func TestIsOrchestratorFirecrackerCmdline(t *testing.T) {
	t.Parallel()

	const versionsDir = "/fc-versions"

	tests := []struct {
		name     string
		cmdline  string
		versions string
		want     bool
	}{
		{
			name:     "orchestrator path with sock",
			cmdline:  "/fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-abc-xyz.sock",
			versions: versionsDir,
			want:     true,
		},
		{
			name:     "ip netns exec wrapper",
			cmdline:  "ip netns exec ns-1 /fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-sbx-rand.sock",
			versions: versionsDir,
			want:     true,
		},
		{
			name:     "fc-netns-exec wrapper",
			cmdline:  "/opt/e2b-infra/bin/fc-netns-exec ns-1 /fc-versions/1.4.0/firecracker --api-sock /tmp/fc-test-sandbox-static-id.sock",
			versions: versionsDir,
			want:     true,
		},
		{
			name:     "system firecracker",
			cmdline:  "/usr/bin/firecracker --api-sock /tmp/fc-abc-xyz.sock",
			versions: versionsDir,
			want:     false,
		},
		{
			name:     "version only no api sock pattern",
			cmdline:  "/usr/bin/firecracker --version",
			versions: versionsDir,
			want:     false,
		},
		{
			name:     "our binary but foreign sock name",
			cmdline:  "/fc-versions/v1.13.1/firecracker --api-sock /tmp/other.sock",
			versions: versionsDir,
			want:     false,
		},
		{
			name:     "our binary but incomplete sock name",
			cmdline:  "/fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-onlyone.sock",
			versions: versionsDir,
			want:     false,
		},
		{
			name:     "empty versions dir rejects all",
			cmdline:  "/fc-versions/v1.13.1/firecracker --api-sock /tmp/fc-abc-xyz.sock",
			versions: "",
			want:     false,
		},
		{
			name:     "grep noise",
			cmdline:  "grep --color=auto fir",
			versions: versionsDir,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isOrchestratorFirecrackerCmdline(tt.cmdline, tt.versions); got != tt.want {
				t.Fatalf("isOrchestratorFirecrackerCmdline(%q, %q) = %v, want %v", tt.cmdline, tt.versions, got, tt.want)
			}
		})
	}
}
