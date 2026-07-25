//go:build windows

package platform

import (
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/envd/internal/execcontext"
	processRpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
	"github.com/e2b-dev/infra/packages/envd/internal/utils"
)

func TestRootSubpath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "empty",
			path: "",
			want: `C:\`,
		},
		{
			name: "root",
			path: "/",
			want: `C:\`,
		},
		{
			name: "slash child",
			path: "/tmp/data",
			want: `C:\tmp\data`,
		},
		{
			name: "backslash child",
			path: `\tmp\data`,
			want: `C:\tmp\data`,
		},
		{
			name: "relative child",
			path: `tmp\data`,
			want: `C:\tmp\data`,
		},
		{
			name: "absolute windows path",
			path: `D:\envd`,
			want: `D:\envd`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rootSubpath(`C:\`, tt.path); got != tt.want {
				t.Fatalf("rootSubpath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigureProcessEnvDoesNotInheritParentEnvironment(t *testing.T) {
	t.Setenv("PATH", `C:\Windows\System32`)
	t.Setenv("SystemRoot", `C:\Windows`)
	t.Setenv("ComSpec", `C:\Windows\System32\cmd.exe`)
	t.Setenv("E2B_SECRET_TOKEN", "secret")

	defaultEnv := utils.NewMap[string, string]()
	defaultEnv.Store("DEFAULT_ENV", "default")
	defaultEnv.Store("OVERRIDDEN_ENV", "default")

	cmd := exec.Command("cmd.exe")
	ConfigureProcessEnv(
		cmd,
		&user.User{
			Username: "sandbox",
			HomeDir:  `C:\Users\sandbox`,
		},
		&processRpc.ProcessConfig{
			Envs: map[string]string{
				"PROCESS_ENV":    "process",
				"OVERRIDDEN_ENV": "process",
			},
		},
		&execcontext.Defaults{
			EnvVars: defaultEnv,
		},
	)

	env := envSliceToMap(cmd.Env)
	require.Equal(t, `C:\Windows\System32`, env["PATH"])
	require.Equal(t, `C:\Windows`, env["SystemRoot"])
	require.Equal(t, `C:\Windows\System32\cmd.exe`, env["ComSpec"])
	require.Equal(t, `C:\Users\sandbox`, env["HOME"])
	require.Equal(t, "sandbox", env["USER"])
	require.Equal(t, "sandbox", env["LOGNAME"])
	require.Equal(t, `C:\Users\sandbox`, env["USERPROFILE"])
	require.Equal(t, "default", env["DEFAULT_ENV"])
	require.Equal(t, "process", env["PROCESS_ENV"])
	require.Equal(t, "process", env["OVERRIDDEN_ENV"])
	require.NotContains(t, env, "E2B_SECRET_TOKEN")
}

func TestProcessSignalUsesSignalConstants(t *testing.T) {
	signal, ok := ProcessSignal(processRpc.Signal_SIGNAL_SIGKILL)
	require.True(t, ok)
	require.Equal(t, syscall.SIGKILL, signal)

	signal, ok = ProcessSignal(processRpc.Signal_SIGNAL_SIGTERM)
	require.True(t, ok)
	require.Equal(t, syscall.SIGTERM, signal)

	_, ok = ProcessSignal(processRpc.Signal_SIGNAL_UNSPECIFIED)
	require.False(t, ok)
}

func envSliceToMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}

		result[key] = value
	}

	return result
}
