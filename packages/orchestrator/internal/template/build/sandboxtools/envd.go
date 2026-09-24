package sandboxtools

import (
	"context"
	"fmt"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
)

const windowsEnvdVersionCommand = `$ErrorActionPreference = 'Stop'
& 'C:\e2b\envd\envd.exe' --version
exit $LASTEXITCODE`

const androidEnvdVersionCommand = "envd --version"

func GetGuestEnvdVersion(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	osType vmm.OsType,
) (string, error) {
	var command string
	switch osType {
	case vmm.OsWindows:
		command = windowsEnvdVersionCommand
	case vmm.OsAndroid:
		command = androidEnvdVersionCommand
	default:
		return "", fmt.Errorf("unsupported guest OS for envd version: %s", osType)
	}

	var stdout strings.Builder
	var stderr strings.Builder

	err := RunCommandWithOutput(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata.Context{OsType: string(osType)},
		func(out, errOut string) {
			stdout.WriteString(out)
			stderr.WriteString(errOut)
		},
	)
	if err != nil {
		return "", fmt.Errorf("error getting %s envd version: %w", osType, err)
	}

	version := strings.TrimSpace(stdout.String())
	if version == "" {
		version = strings.TrimSpace(stderr.String())
	}
	if version == "" {
		return "", fmt.Errorf("%s envd version command returned empty output", osType)
	}

	return version, nil
}
