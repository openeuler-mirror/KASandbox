package server

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/builderrors"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/buildlogger"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci/auth"
	templatemetadata "github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/templates"
)

func (s *ServerStore) TemplateCreate(ctx context.Context, templateRequest *templatemanager.TemplateCreateRequest) (*emptypb.Empty, error) {
	ctx, childSpan := tracer.Start(ctx, "template-create")
	defer childSpan.End()

	cfg := templateRequest.GetTemplate()
	osType, vmmType, err := resolveTemplateRuntime(ctx, s.templateStorage, cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid OS/VMM configuration: %w", err)
	}

	hugePages := cfg.GetHugePages()
	if vmmType == vmm.BackendStratoVirt && hugePages {
		s.buildLogger.Warn(ctx, "HugePages are not supported by StratoVirt; disabling HugePages",
			logger.WithTemplateID(cfg.GetTemplateID()),
			logger.WithBuildID(cfg.GetBuildID()),
		)
		hugePages = false
	}

	childSpan.SetAttributes(
		telemetry.WithTemplateID(cfg.GetTemplateID()),
		telemetry.WithBuildID(cfg.GetBuildID()),
		attribute.String("env.kernel.version", cfg.GetKernelVersion()),
		attribute.String("env.firecracker.version", cfg.GetFirecrackerVersion()),
		attribute.String("env.vmm.type", string(vmmType)),
		attribute.String("env.os.type", string(osType)),
		attribute.String("env.start_cmd", cfg.GetStartCommand()),
		attribute.Int64("env.memory_mb", int64(cfg.GetMemoryMB())),
		attribute.Int64("env.vcpu_count", int64(cfg.GetVCpuCount())),
		attribute.Bool("env.huge_pages", hugePages),
	)

	metadata := storage.TemplateFiles{
		BuildID: cfg.GetBuildID(),
	}

	// default to scope by template ID
	cacheScope := cfg.GetTemplateID()
	if templateRequest.CacheScope != nil {
		cacheScope = templateRequest.GetCacheScope()
	}

	// Create the auth provider using the factory
	authProvider := auth.NewAuthProvider(cfg.GetFromImageRegistry())

	// TODO: Remove, temporary handling when version is not sent from the API
	version := templateRequest.GetVersion()
	if version == "" {
		if cfg.GetFromImage() == "" && cfg.GetFromImageRaw() == "" && cfg.GetFromTemplate() == nil {
			version = templates.TemplateV1Version
		} else {
			version = templates.TemplateV2BetaVersion
		}
	}

	template := config.TemplateConfig{
		Version:              version,
		TeamID:               cfg.GetTeamID(),
		TemplateID:           cfg.GetTemplateID(),
		CacheScope:           cacheScope,
		VCpuCount:            int64(cfg.GetVCpuCount()),
		MemoryMB:             int64(cfg.GetMemoryMB()),
		StartCmd:             cfg.GetStartCommand(),
		ReadyCmd:             cfg.GetReadyCommand(),
		DiskSizeMB:           int64(cfg.GetDiskSizeMB()),
		HugePages:            hugePages,
		FromImage:            cfg.GetFromImage(),
		FromImageRaw:         cfg.GetFromImageRaw(),
		FromTemplate:         cfg.GetFromTemplate(),
		RegistryAuthProvider: authProvider,
		Force:                cfg.Force,
		Steps:                cfg.GetSteps(),
		KernelVersion:        cfg.GetKernelVersion(),
		FirecrackerVersion:   cfg.GetFirecrackerVersion(),
		VMMType:              string(vmmType),
		OsType:               osType,
	}

	logs := buildlogger.NewLogEntryLogger()
	buildInfo, err := s.buildCache.Create(template.TeamID, metadata.BuildID, logs)
	if err != nil {
		return nil, fmt.Errorf("error while creating build cache: %w", err)
	}

	// Add new core that will log all messages using logger (zap.Logger) to the logs buffer too
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	bufferCore := zapcore.NewCore(encoder, logs, zapcore.DebugLevel)
	core := zapcore.NewTee(bufferCore, s.buildLogger.Detach(ctx).Core().
		With([]zap.Field{
			{Type: zapcore.StringType, Key: "envID", String: cfg.GetTemplateID()},
			{Type: zapcore.StringType, Key: "buildID", String: metadata.BuildID},
		}),
	)

	s.wg.Add(1)
	s.activeBuilds.Add(1)
	go func(ctx context.Context) {
		defer s.wg.Done()
		defer s.activeBuilds.Add(-1)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctx, buildSpan := tracer.Start(ctx, "template-background-build", trace.WithAttributes(
			telemetry.WithTemplateID(template.TemplateID),
			telemetry.WithBuildID(metadata.BuildID),
			telemetry.WithTeamID(template.TeamID),
		))
		defer buildSpan.End()

		defer func() {
			if r := recover(); r != nil {
				telemetry.ReportCriticalError(ctx, "recovered from panic in template build handler", nil, attribute.String("panic", fmt.Sprintf("%v", r)), telemetry.WithTemplateID(cfg.GetTemplateID()), telemetry.WithBuildID(cfg.GetBuildID()))
				buildInfo.SetFail(builderrors.UnwrapUserError(nil))
			}
		}()

		// Watch for build cancellation requests
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-buildInfo.Result.Done:
				res, _ := buildInfo.Result.Result()
				if res.Status == templatemanager.TemplateBuildState_Failed {
					cancel()
				}

				return
			}
		}()

		res, err := s.builder.Build(ctx, metadata, template, core)
		_ = core.Sync()
		if err != nil {
			userError := builderrors.UnwrapUserError(err)

			attrs := []attribute.KeyValue{
				telemetry.WithTemplateID(cfg.GetTemplateID()),
				telemetry.WithBuildID(cfg.GetBuildID()),
			}
			if userError.GetMessage() == builderrors.InternalErrorMessage {
				telemetry.ReportCriticalError(ctx, "error while building template", err, attrs...)
			} else {
				telemetry.ReportError(ctx, "error while building template", err, attrs...)
			}

			buildInfo.SetFail(userError)
		} else {
			buildInfo.SetSuccess(&templatemanager.TemplateBuildMetadata{
				RootfsSizeKey:  int32(res.RootfsSizeMB),
				EnvdVersionKey: res.EnvdVersion,
			})
			telemetry.ReportEvent(ctx, "Environment built")
		}
	}(context.WithoutCancel(ctx))

	return nil, nil
}

func resolveTemplateRuntime(
	ctx context.Context,
	templateStorage storage.StorageProvider,
	cfg *templatemanager.TemplateConfig,
) (vmm.OsType, vmm.BackendType, error) {
	fromTemplate := cfg.GetFromTemplate()
	if fromTemplate == nil {
		osType, err := vmm.ParseOsType(cfg.GetOsType())
		if err != nil {
			return "", "", err
		}

		vmmType := vmm.BackendType(cfg.GetVmmType()).OrDefault()
		if err := vmm.ValidateBackendForOS(osType, vmmType); err != nil {
			return "", "", err
		}

		return osType, vmmType, nil
	}

	sourceMetadata, err := templatemetadata.FromBuildID(ctx, templateStorage, fromTemplate.GetBuildID())
	if err != nil {
		return "", "", fmt.Errorf("failed to read source template metadata: %w", err)
	}

	sourceOS, err := vmm.ParseOsType(sourceMetadata.Template.OsType)
	if err != nil {
		return "", "", fmt.Errorf("invalid source template OS: %w", err)
	}
	sourceVMM := vmm.BackendType(sourceMetadata.Template.VMMType).OrDefault()
	if err := vmm.ValidateBackendForOS(sourceOS, sourceVMM); err != nil {
		return "", "", fmt.Errorf("invalid source template runtime: %w", err)
	}

	if cfg.GetOsType() != "" {
		requestedOS, err := vmm.ParseOsType(cfg.GetOsType())
		if err != nil {
			return "", "", err
		}
		if requestedOS != sourceOS {
			return "", "", fmt.Errorf("requested OS %q does not match source template OS %q", requestedOS, sourceOS)
		}
	}

	if cfg.GetVmmType() != "" {
		requestedVMM := vmm.BackendType(cfg.GetVmmType())
		if requestedVMM != sourceVMM {
			return "", "", fmt.Errorf("requested VMM %q does not match source template VMM %q", requestedVMM, sourceVMM)
		}
	}

	return sourceOS, sourceVMM, nil
}
