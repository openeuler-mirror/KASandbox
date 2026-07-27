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

func GetWindowsEnvdVersion(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
) (string, error) {
	var stdout strings.Builder
	var stderr strings.Builder

	err := RunCommandWithOutput(
		ctx,
		proxy,
		sandboxID,
		windowsEnvdVersionCommand,
		metadata.Context{OsType: string(vmm.OsWindows)},
		func(out, errOut string) {
			stdout.WriteString(out)
			stderr.WriteString(errOut)
		},
	)
	if err != nil {
		return "", fmt.Errorf("error getting Windows envd version: %w", err)
	}

	version := strings.TrimSpace(stdout.String())
	if version == "" {
		version = strings.TrimSpace(stderr.String())
	}
	if version == "" {
		return "", fmt.Errorf("Windows envd version command returned empty output")
	}

	return version, nil
}
