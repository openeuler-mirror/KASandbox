package sandboxtools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const commandHardTimeout = 1 * time.Hour

func RunCommandWithOutput(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
	processOutput func(stdout, stderr string),
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
		processOutput,
	)
}

func RunCommand(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
		func(_, _ string) {},
	)
}

func RunCommandWithLogger(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	logger logger.Logger,
	lvl zapcore.Level,
	id string,
	sandboxID string,
	command string,
	metadata metadata.Context,
) error {
	return RunCommandWithConfirmation(
		ctx,
		proxy,
		logger,
		lvl,
		id,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
	)
}

func RunCommandWithConfirmation(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	logger logger.Logger,
	lvl zapcore.Level,
	id string,
	sandboxID string,
	command string,
	metadata metadata.Context,
	confirmCh chan<- struct{},
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		confirmCh,
		func(stdout, stderr string) {
			logStream(ctx, logger, lvl, id, "stdout", stdout)
			logStream(ctx, logger, lvl, id, "stderr", stderr)
		},
	)
}

func runCommandWithAllOptions(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
	confirmCh chan<- struct{},
	processOutput func(stdout, stderr string),
) (e error) {
	ctx, span := tracer.Start(ctx, "run command", trace.WithAttributes(attribute.String("command", command), telemetry.WithSandboxID(sandboxID)))
	defer span.End()
	defer func() {
		if e != nil {
			span.RecordError(e)
			span.SetStatus(codes.Error, e.Error())
		}
	}()

	envs := buildCommandEnvs(metadata)

	shellCmd, shellArgs := guestShell(metadata.OsType, command)

	runCmdReq := connect.NewRequest(&process.StartRequest{
		Process: &process.ProcessConfig{
			Cmd:  shellCmd,
			Cwd:  metadata.WorkDir,
			Args: shellArgs,
			Envs: envs,
		},
	})

	hc := http.Client{
		Timeout:   commandHardTimeout,
		Transport: sandbox.SandboxHttpTransport,
	}

	proxyHost := fmt.Sprintf("http://localhost%s", proxy.GetAddr())
	processC := processconnect.NewProcessClient(&hc, proxyHost)
	err := grpc.SetSandboxHeader(runCmdReq.Header(), proxyHost, sandboxID)
	if err != nil {
		return fmt.Errorf("failed to set sandbox header: %w", err)
	}
	// An empty user means "let envd pick the default user". We omit the auth
	// header entirely (mirroring the SDK) instead of sending an empty username,
	// which envd would reject via user.Lookup(""). This is how Windows commands
	// run as the image's baked-in default user (there is no Linux "root").
	if metadata.User != "" {
		grpc.SetUserHeader(runCmdReq.Header(), metadata.User)
	}

	processCtx, processCancel := context.WithCancel(ctx)
	defer processCancel()
	commandStream, err := processC.Start(processCtx, runCmdReq)
	// Confirm the command has executed before proceeding
	close(confirmCh)
	if err != nil {
		return fmt.Errorf("error starting process: %w", err)
	}
	defer func() {
		processCancel()
		commandStream.Close()
	}()

	msgCh, msgErrCh := grpc.StreamToChannel(ctx, commandStream)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context: %w", ctx.Err())
		case err := <-msgErrCh:
			return fmt.Errorf("command failed: %w", err)
		case msg, ok := <-msgCh:
			if !ok {
				return nil
			}
			e := msg.GetEvent()
			if e == nil {
				logger.L().Error(ctx, "received nil command event")

				return nil
			}

			switch {
			case e.GetData() != nil:
				data := e.GetData()
				processOutput(string(data.GetStdout()), string(data.GetStderr()))

			case e.GetEnd() != nil:
				end := e.GetEnd()
				success := end.GetExitCode() == 0

				if !success {
					return errors.New(end.GetStatus())
				}
			}
		}
	}
}

// guestShell resolves the shell command and args used to run a command string
// inside the guest, mirroring the SDK's per-OS selection (see python-sdk
// e2b/sandbox/commands/main.py::_get_command_shell). envd executes the command
// verbatim, so the OS-appropriate shell must be chosen here. The linux branch
// (including the empty default) is byte-for-byte identical to the previous
// hard-coded behavior.
func guestShell(osType string, command string) (string, []string) {
	if osType == string(vmm.OsWindows) {
		return "powershell.exe", []string{"-NoLogo", "-NonInteractive", "-Command", command}
	}

	return "/bin/bash", []string{"-l", "-c", command}
}

// buildCommandEnvs clones the command environment and, on linux, appends the
// standard directories to PATH so utilities are always findable even if the
// user sets PATH to something broken. The unix path list and ':' separator are
// meaningless on Windows, so they are skipped there.
func buildCommandEnvs(meta metadata.Context) map[string]string {
	envs := maps.Clone(meta.EnvVars)

	if meta.OsType == string(vmm.OsWindows) {
		return envs
	}

	if _, ok := envs["PATH"]; ok {
		envs["PATH"] += ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	return envs
}

func logStream(ctx context.Context, logger logger.Logger, lvl zapcore.Level, id string, name string, content string) {
	if logger == nil {
		return
	}

	if content == "" {
		return
	}
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msg := fmt.Sprintf("[%s] [%s]: %s", id, name, line)
		logger.Log(ctx, lvl, msg)
	}
}

// windowsSyncCommand flushes every mounted fixed volume's write cache to disk
// via the Storage module's Write-VolumeCache. $ErrorActionPreference = 'Stop'
// turns a missing cmdlet or a permission failure into a non-zero exit so the
// caller treats it as an error, matching the Linux "sync" failure semantics.
const windowsSyncCommand = `$ErrorActionPreference = 'Stop'; Get-Volume | Where-Object DriveLetter | ForEach-Object { Write-VolumeCache $_.DriveLetter }`

// syncCommand returns the guest command and command context used to flush the
// in-guest page cache to disk for the given OS family. Linux runs busybox sync
// as root over bash; Windows runs Write-VolumeCache over powershell as the
// image's default (admin) user. The empty/Linux branch is byte-for-byte
// identical to the previous hard-coded behavior.
func syncCommand(osType vmm.OsType) (string, metadata.Context) {
	if osType.OrDefault() == vmm.OsWindows {
		return windowsSyncCommand, metadata.Context{OsType: string(vmm.OsWindows)}
	}

	return rootfs.SandboxBusyBoxPath + " sync", metadata.Context{User: "root"}
}

// SyncChangesToDisk synchronizes filesystem changes to disk so the sandbox can
// be re-created without resuming from memory. It is OS-aware: Linux runs busybox
// sync as root, Windows runs Write-VolumeCache over powershell (see syncCommand).
func SyncChangesToDisk(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	osType vmm.OsType,
) error {
	command, meta := syncCommand(osType)

	return RunCommand(
		ctx,
		proxy,
		sandboxID,
		command,
		meta,
	)
}
