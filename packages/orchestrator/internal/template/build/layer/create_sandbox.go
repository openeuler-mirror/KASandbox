package layer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/constants"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// CreateSandbox creates sandboxes for new templates
type CreateSandbox struct {
	config         sandbox.Config
	timeout        time.Duration
	sandboxFactory *sandbox.Factory

	directDiskPaths map[string]string
	ioEngine        *string
}

const (
	minEnvdVersionForKVMClock = "0.2.11"                 // Minimum version of envd that supports KVM clock
	DefaultIoEngine           = models.DriveIoEngineSync // Use the Sync io engine by default to avoid issues with Async.
)

var _ SandboxCreator = (*CreateSandbox)(nil)

type createSandboxOptions struct {
	directDiskPaths map[string]string
	ioEngine        *string
}

type CreateSandboxOption func(*createSandboxOptions)

func WithIoEngine(ioEngine string) CreateSandboxOption {
	return func(opts *createSandboxOptions) {
		opts.ioEngine = &ioEngine
	}
}

func WithRootfsCachePath(rootfsCachePath string) CreateSandboxOption {
	return func(opts *createSandboxOptions) {
		opts.directDiskPaths[storage.RootfsName] = rootfsCachePath
	}
}

// WithDirectDiskPaths configures local writable files used to establish the
// first stored generation of each disk. Disks omitted from the map use an NBD
// copy-on-write overlay.
func WithDirectDiskPaths(paths map[string]string) CreateSandboxOption {
	return func(opts *createSandboxOptions) {
		for name, path := range paths {
			opts.directDiskPaths[name] = path
		}
	}
}

func NewCreateSandbox(config sandbox.Config, sandboxFactory *sandbox.Factory, timeout time.Duration, options ...CreateSandboxOption) *CreateSandbox {
	opts := &createSandboxOptions{
		directDiskPaths: make(map[string]string),
		ioEngine:        utils.ToPtr(DefaultIoEngine),
	}
	for _, option := range options {
		option(opts)
	}

	return &CreateSandbox{
		config:          config,
		timeout:         timeout,
		directDiskPaths: opts.directDiskPaths,
		sandboxFactory:  sandboxFactory,
		ioEngine:        opts.ioEngine,
	}
}

func (cs *CreateSandbox) Sandbox(
	ctx context.Context,
	layerExecutor *LayerExecutor,
	sourceTemplate sbxtemplate.Template,
) (s *sandbox.Sandbox, err error) {
	// Create new memfile with the size of the sandbox RAM, this updates the underlying memfile.
	// This is ok as the sandbox is started from the beginning.
	memfile, err := block.NewEmpty(
		cs.config.RamMB<<constants.ToMBShift,
		config.MemfilePageSize(cs.config.HugePages),
		uuid.MustParse(sourceTemplate.Files().BuildID),
	)
	if err != nil {
		return nil, fmt.Errorf("create memfile: %w", err)
	}

	template := sbxtemplate.NewMaskTemplate(sourceTemplate, sbxtemplate.WithMemfile(memfile))

	kvmClock, err := utils.IsGTEVersion(cs.config.Envd.Version, minEnvdVersionForKVMClock)
	if err != nil {
		return nil, fmt.Errorf("error comparing envd version: %w", err)
	}

	// In case of a new sandbox, base template ID is now used as the potentially exported template base ID.
	sbx, err := cs.sandboxFactory.CreateSandbox(
		ctx,
		cs.config,
		sandbox.RuntimeMetadata{
			TemplateID:  layerExecutor.Config.TemplateID,
			SandboxID:   config.InstanceBuildPrefix + id.Generate(),
			ExecutionID: uuid.NewString(),
		},
		template,
		cs.timeout,
		cs.directDiskPaths,
		vmm.ProcessOptions{
			InitScriptPath:      constants.SystemdInitPath,
			KernelLogs:          env.IsDevelopment(),
			SystemdToKernelLogs: false,
			KvmClock:            kvmClock,
			IoEngine:            cs.ioEngine,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	// Register the sandbox before waiting for envd. Android performs network
	// initialization and outbound requests while envd is starting; the TCP
	// firewall must already be able to associate those connections with this
	// sandbox. BuildLayer keeps the registration for the rest of the build.
	layerExecutor.sandboxes.Insert(sbx)
	defer func() {
		if err != nil {
			layerExecutor.sandboxes.Remove(sbx.Runtime.SandboxID)
			closeErr := layerExecutor.proxy.RemoveFromPool(sbx.LifecycleID)
			if closeErr != nil {
				// Errors here will be from forcefully closing the connections, so we can ignore them—they will at worst timeout on their own.
				layerExecutor.logger.Warn(ctx, "errors when manually closing connections to sandbox", zap.Error(closeErr))
			} else {
				layerExecutor.logger.Debug(
					ctx,
					"removed proxy from pool",
					logger.WithSandboxID(sbx.Runtime.SandboxID),
					logger.WithExecutionID(sbx.Runtime.ExecutionID),
				)
			}

			// Close the sandbox in case of error to avoid leaking resources
			err = errors.Join(err, sbx.Close(ctx))
		}
	}()

	err = sbx.WaitForEnvd(
		ctx,
		waitEnvdTimeout,
	)
	if err != nil {
		return nil, fmt.Errorf("wait for envd: %w", err)
	}

	return sbx, nil
}
