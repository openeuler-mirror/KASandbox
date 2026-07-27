package finalize

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	tt "text/template"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/buildcontext"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/sandboxtools"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const configurationTimeout = 5 * time.Minute

//go:embed configure.sh
var configureScriptFile string
var ConfigureScriptTemplate = tt.Must(tt.New("provisioning-finish-script").Parse(configureScriptFile))

//go:embed configure.ps1
var configureScriptFileWindows string
var ConfigureScriptTemplateWindows = tt.Must(tt.New("provisioning-finish-script-windows").Parse(configureScriptFileWindows))

type ConfigurationParams struct {
	EnvID      string
	TemplateID string
	BuildID    string
}

// configurationScript renders the finalize configuration script and the command
// context it must run under for the given guest OS.
//
// Linux runs the POSIX configure.sh as root (creates the default user, writes
// the .e2b marker, fixes permissions). Windows has no root user and ships its
// account/envd in the image, so it runs the PowerShell configure.ps1 (writes
// only the .e2b marker) with an empty user, which makes the orchestrator omit
// the auth header so envd runs as the image's default user.
func configurationScript(osType vmm.OsType, params ConfigurationParams) (string, metadata.Context, error) {
	tmpl := ConfigureScriptTemplate
	cmdCtx := metadata.Context{User: "root"}
	if osType == vmm.OsWindows {
		tmpl = ConfigureScriptTemplateWindows
		cmdCtx = metadata.Context{OsType: string(vmm.OsWindows)}
	}

	var scriptDef bytes.Buffer
	if err := tmpl.Execute(&scriptDef, params); err != nil {
		return "", metadata.Context{}, fmt.Errorf("error executing provision script: %w", err)
	}

	return scriptDef.String(), cmdCtx, nil
}

func runConfiguration(
	ctx context.Context,
	userLogger logger.Logger,
	bc buildcontext.BuildContext,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	osType vmm.OsType,
) error {
	ctx, span := tracer.Start(ctx, "run configuration")
	defer span.End()

	script, cmdCtx, err := configurationScript(osType, ConfigurationParams{
		EnvID:      bc.Config.TemplateID,
		TemplateID: bc.Config.TemplateID,
		BuildID:    bc.Template.BuildID,
	})
	if err != nil {
		return err
	}

	err = sandboxtools.RunCommandWithLogger(
		ctx,
		proxy,
		userLogger,
		zapcore.DebugLevel,
		"config",
		sandboxID,
		script,
		cmdCtx,
	)
	if err != nil {
		return fmt.Errorf("error running configuration script: %w", err)
	}

	return nil
}
