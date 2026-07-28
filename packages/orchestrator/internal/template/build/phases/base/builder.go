package base

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/buildcontext"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/filesystem"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci"
	coreraw "github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/raw"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/layer"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/phases"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/sandboxtools"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/storage/cache"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	artifactsregistry "github.com/e2b-dev/infra/packages/shared/pkg/artifacts-registry"
	"github.com/e2b-dev/infra/packages/shared/pkg/dockerhub"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	rootfsBuildFileName = "rootfs.filesystem.build"

	baseLayerTimeout = 10 * time.Minute

	defaultUser = "root"

	rawWindowsInitialEnvdVersion = "0.0.0"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/phases/base")

type BaseBuilder struct {
	buildcontext.BuildContext

	logger logger.Logger
	proxy  *proxy.SandboxProxy

	sandboxFactory      *sandbox.Factory
	templateStorage     storage.StorageProvider
	artifactRegistry    artifactsregistry.ArtifactsRegistry
	dockerhubRepository dockerhub.RemoteRepository
	featureFlags        *featureflags.Client
	sandboxes           *sandbox.Map

	layerExecutor *layer.LayerExecutor
	index         cache.Index
	metrics       *metrics.BuildMetrics

	persistentDigest string
}

func (bb *BaseBuilder) androidPersistentPath() string {
	return filepath.Join(bb.BuilderConfig.FirmwareDir, storage.PersistentName)
}

func (bb *BaseBuilder) androidNewfsMsdosPath() string {
	return filepath.Join(bb.BuilderConfig.CvdHostPackageDir, "bin", "newfs_msdos")
}

func (bb *BaseBuilder) androidPersistentDigest(ctx context.Context) (string, error) {
	if bb.persistentDigest != "" {
		return bb.persistentDigest, nil
	}

	digest, err := sha256File(bb.androidPersistentPath())
	if err != nil {
		return "", fmt.Errorf("failed to calculate Android persistent image digest: %w", err)
	}
	bb.persistentDigest = digest
	bb.logger.Info(ctx, "calculated Android base disk inputs",
		zap.String("persistent_digest", digest),
		zap.Int64("sdcard_size_mb", androidSDCardImageSizeMB),
		zap.String("sdcard_generator_version", androidSDCardGeneratorVersion),
	)

	return digest, nil
}

func New(
	buildContext buildcontext.BuildContext,
	featureFlags *featureflags.Client,
	logger logger.Logger,
	proxy *proxy.SandboxProxy,
	templateStorage storage.StorageProvider,
	artifactRegistry artifactsregistry.ArtifactsRegistry,
	dockerhubRepository dockerhub.RemoteRepository,
	layerExecutor *layer.LayerExecutor,
	index cache.Index,
	metrics *metrics.BuildMetrics,
	sandboxFactory *sandbox.Factory,
	sandboxes *sandbox.Map,
) *BaseBuilder {
	return &BaseBuilder{
		BuildContext: buildContext,

		logger: logger,
		proxy:  proxy,

		templateStorage:     templateStorage,
		artifactRegistry:    artifactRegistry,
		dockerhubRepository: dockerhubRepository,
		sandboxFactory:      sandboxFactory,
		featureFlags:        featureFlags,
		sandboxes:           sandboxes,

		layerExecutor: layerExecutor,
		index:         index,
		metrics:       metrics,
	}
}

func (bb *BaseBuilder) Prefix() string {
	return "base"
}

func (bb *BaseBuilder) String(ctx context.Context) (string, error) {
	var baseSource string
	if bb.Config.FromTemplate != nil {
		baseSource = "FROM TEMPLATE " + bb.Config.FromTemplate.GetAlias()
	} else if bb.Config.UsesRawImage() {
		baseSource = "FROM RAW " + bb.Config.FromImageRaw
	} else {
		fromImage := bb.Config.FromImage
		if fromImage == "" {
			tag, err := bb.artifactRegistry.GetTag(ctx, bb.Config.TemplateID, bb.Template.BuildID)
			if err != nil {
				return "", fmt.Errorf("error getting tag for template: %w", err)
			}
			fromImage = tag
		}
		baseSource = "FROM " + fromImage
	}

	return baseSource, nil
}

func (bb *BaseBuilder) Metadata() phases.PhaseMeta {
	return phases.PhaseMeta{
		Phase:    metrics.PhaseBase,
		StepType: "base",
	}
}

func (bb *BaseBuilder) ValidateSourceCapability(ctx context.Context, userLogger logger.Logger) error {
	var err error
	switch {
	case bb.Config.FromTemplate != nil:
		// Derived templates inherit the source template runtime.
		return nil
	case bb.Config.UsesRawImage():
		if bb.Config.GuestOS() != vmm.OsWindows && bb.Config.GuestOS() != vmm.OsAndroid {
			err = fmt.Errorf("the current raw-image build implementation only supports Windows and Android guests, got %q", bb.Config.GuestOS())
		} else if _, parseErr := coreraw.ParseSource(bb.Config.FromImageRaw); parseErr != nil {
			err = parseErr
		} else if bb.Config.IsAndroid() {
			if validateErr := validateRegularNonemptyFile(bb.androidPersistentPath()); validateErr != nil {
				err = fmt.Errorf("invalid Android persistent image: %w", validateErr)
			} else if validateErr := validateExecutableFile(bb.androidNewfsMsdosPath()); validateErr != nil {
				err = fmt.Errorf("invalid Android newfs_msdos tool: %w", validateErr)
			}
		}
	case bb.Config.GuestOS() != vmm.OsLinux:
		err = fmt.Errorf("the current OCI build implementation only supports Linux guests, got %q", bb.Config.GuestOS())
	}

	if err == nil {
		return nil
	}

	userLogger.Error(ctx, err.Error())

	return phases.NewPhaseBuildError(bb.Metadata(), err)
}

func (bb *BaseBuilder) Build(
	ctx context.Context,
	userLogger logger.Logger,
	_ string,
	_ phases.LayerResult,
	currentLayer phases.LayerResult,
) (phases.LayerResult, error) {
	ctx, span := tracer.Start(ctx, "build base", trace.WithAttributes(
		attribute.String("hash", currentLayer.Hash),
	))
	defer span.End()

	build := bb.buildLayerFromOCI
	if bb.Config.UsesRawImage() {
		// Raw images skip OCI conversion; the disk is registered as the rootfs
		// as-is.
		build = bb.buildLayerFromRaw
	}
	baseMetadata, err := build(
		ctx,
		userLogger,
		currentLayer.Metadata,
		currentLayer.Hash,
	)
	if err != nil {
		return phases.LayerResult{}, err
	}

	return phases.LayerResult{
		Metadata: baseMetadata,
		Cached:   false,
		Hash:     currentLayer.Hash,
	}, nil
}

// buildLayerFromRaw builds the base layer from a raw (non-OCI) disk image. The
// guest image already contains envd, so this path skips the OCI provisioning,
// ext4 integrity checks and disk enlargement that the OCI path performs. It
// downloads the raw disk, registers it as the rootfs and snapshots a booted
// sandbox into the base layer.
func (bb *BaseBuilder) buildLayerFromRaw(
	ctx context.Context,
	userLogger logger.Logger,
	baseMetadata metadata.Template,
	hash string,
) (metadata.Template, error) {
	templateBuildDir := filepath.Join(bb.BuilderConfig.TemplatesDir, baseMetadata.Template.BuildID)
	err := os.MkdirAll(templateBuildDir, 0o777)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error creating template build directory: %w", err)
	}
	defer func() {
		err := os.RemoveAll(templateBuildDir)
		if err != nil {
			bb.logger.Error(ctx, "Error while removing template build directory", zap.Error(err))
		}
	}()

	// Created here to be able to pass it to CreateSandbox for populating COW cache
	rootfsPath := filepath.Join(templateBuildDir, rootfsBuildFileName)
	directDiskPaths := map[string]string{storage.RootfsName: rootfsPath}

	var rootfs block.ReadonlyDevice
	var disks []sbxtemplate.Disk
	var memfile block.ReadonlyDevice
	if bb.Config.IsAndroid() {
		persistentDigest, digestErr := bb.androidPersistentDigest(ctx)
		if digestErr != nil {
			err = digestErr
		} else {
			disks, memfile, err = constructLayerFilesFromAndroidRaw(
				ctx,
				userLogger,
				bb.BuildContext,
				baseMetadata.Template.BuildID,
				bb.Config.FromImageRaw,
				bb.androidPersistentPath(),
				persistentDigest,
				bb.androidNewfsMsdosPath(),
				templateBuildDir,
				bb.Config.RegistryAuthProvider,
			)
		}
		if err == nil {
			rootDisk, rootErr := sbxtemplate.RootDisk(disks)
			if rootErr != nil {
				err = rootErr
			} else {
				rootfs = rootDisk.Device
			}
			rootfsPath = filepath.Join(templateBuildDir, storage.RootfsName)
			directDiskPaths = localDiskPaths(templateBuildDir, disks)
		}
	} else {
		rootfs, memfile, _, err = constructLayerFilesFromWindowsRaw(ctx, userLogger, bb.BuildContext, baseMetadata.Template.BuildID, bb.Config.FromImageRaw, rootfsPath, bb.Config.RegistryAuthProvider)
	}
	if err != nil {
		if memfile != nil {
			err = errors.Join(err, memfile.Close())
		}
		for _, disk := range disks {
			if disk.Device != nil {
				err = errors.Join(err, disk.Device.Close())
			}
		}
		return metadata.Template{}, fmt.Errorf("error building environment from raw image: %w", err)
	}

	cacheFiles, err := storage.TemplateFiles{BuildID: baseMetadata.Template.BuildID}.CacheFiles(bb.BuildContext.BuilderConfig.StorageConfig)
	if err != nil {
		err = errors.Join(err, rootfs.Close(), memfile.Close())

		return metadata.Template{}, fmt.Errorf("error creating template files: %w", err)
	}
	var localTemplate sbxtemplate.Template
	if bb.Config.IsAndroid() {
		localTemplate, err = sbxtemplate.NewLocalMultiDiskTemplate(cacheFiles, disks, memfile)
		if err != nil {
			return metadata.Template{}, errors.Join(err, rootfs.Close(), memfile.Close())
		}
	} else {
		localTemplate = sbxtemplate.NewLocalTemplate(cacheFiles, rootfs, memfile)
	}
	defer localTemplate.Close(ctx)

	envdVersion := bb.EnvdVersion
	if bb.Config.IsWindows() || bb.Config.IsAndroid() {
		envdVersion = rawWindowsInitialEnvdVersion
	}

	baseSbxConfig := sandbox.Config{
		Vcpu:      bb.Config.VCpuCount,
		RamMB:     bb.Config.MemoryMB,
		HugePages: bb.Config.HugePages,

		// Allow sandbox internet access during the base build
		Network: &orchestrator.SandboxNetworkConfig{},

		Envd: sandbox.EnvdMetadata{
			Version: envdVersion,
		},

		VMMConfig: vmm.VMMConfig{
			Type:          vmm.BackendType(bb.Config.VMMType).OrDefault(),
			KernelVersion: bb.Config.KernelVersion,
			VMMVersion:    bb.Config.FirecrackerVersion,
			OsType:        bb.Config.GuestOS(),
		},
	}

	// Create sandbox for building the template layer
	sandboxCreator := layer.NewCreateSandbox(
		baseSbxConfig,
		bb.sandboxFactory,
		baseLayerTimeout,
		layer.WithDirectDiskPaths(directDiskPaths),
	)

	// Flush the guest page cache to disk. The base raw layer is later cold-booted
	// by finalize (not resumed from memory), so an un-flushed filesystem can
	// leave the bootloader/filesystem inconsistent on the next boot.
	actionExecutor := layer.NewFunctionAction(func(ctx context.Context, sbx *sandbox.Sandbox, meta metadata.Template) (metadata.Template, error) {
		if bb.Config.IsWindows() {
			guestEnvdVersion, err := sandboxtools.GetWindowsEnvdVersion(
				ctx,
				bb.proxy,
				sbx.Runtime.SandboxID,
			)
			if err != nil {
				return metadata.Template{}, fmt.Errorf("error getting guest envd version: %w", err)
			}

			meta.Template.EnvdVersion = guestEnvdVersion
			sbx.Config.Envd.Version = guestEnvdVersion
		}

		err := sandboxtools.SyncChangesToDisk(
			ctx,
			bb.proxy,
			sbx.Runtime.SandboxID,
			bb.Config.GuestOS(),
		)
		if err != nil {
			return metadata.Template{}, fmt.Errorf("error running sync command: %w", err)
		}

		return meta, nil
	})

	templateProvider := layer.NewDirectSourceTemplateProvider(localTemplate)

	baseLayer, err := bb.layerExecutor.BuildLayer(
		ctx,
		userLogger,
		layer.LayerBuildCommand{
			SourceTemplate: templateProvider,
			CurrentLayer:   baseMetadata,
			Hash:           hash,
			UpdateEnvd:     false,
			SandboxCreator: sandboxCreator,
			ActionExecutor: actionExecutor,
		},
	)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error building base layer from raw image: %w", err)
	}

	return baseLayer, nil
}

func localDiskPaths(dir string, disks []sbxtemplate.Disk) map[string]string {
	paths := make(map[string]string, len(disks))
	for _, disk := range disks {
		paths[disk.Name] = filepath.Join(dir, disk.Name)
	}

	return paths
}

func (bb *BaseBuilder) buildLayerFromOCI(
	ctx context.Context,
	userLogger logger.Logger,
	baseMetadata metadata.Template,
	hash string,
) (metadata.Template, error) {
	templateBuildDir := filepath.Join(bb.BuilderConfig.TemplatesDir, baseMetadata.Template.BuildID)
	err := os.MkdirAll(templateBuildDir, 0o777)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error creating template build directory: %w", err)
	}
	defer func() {
		err := os.RemoveAll(templateBuildDir)
		if err != nil {
			bb.logger.Error(ctx, "Error while removing template build directory", zap.Error(err))
		}
	}()

	// Created here to be able to pass it to CreateSandbox for populating COW cache
	rootfsPath := filepath.Join(templateBuildDir, rootfsBuildFileName)

	rootfs, memfile, envsImg, err := constructLayerFilesFromOCI(ctx, userLogger, bb.BuildContext, bb.Metadata(), baseMetadata.Template.BuildID, bb.artifactRegistry, bb.dockerhubRepository, bb.featureFlags, rootfsPath)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error building environment: %w", err)
	}

	cacheFiles, err := storage.TemplateFiles{BuildID: baseMetadata.Template.BuildID}.CacheFiles(bb.BuildContext.BuilderConfig.StorageConfig)
	if err != nil {
		err = errors.Join(err, rootfs.Close(), memfile.Close())

		return metadata.Template{}, fmt.Errorf("error creating template files: %w", err)
	}
	localTemplate := sbxtemplate.NewLocalTemplate(cacheFiles, rootfs, memfile)
	defer localTemplate.Close(ctx)

	// Env variables from the Docker image
	baseMetadata.Context.EnvVars = oci.ParseEnvs(envsImg.Env)

	// Provision sandbox with systemd and other vital parts
	userLogger.Info(ctx, "Provisioning sandbox template")

	baseSbxConfig := sandbox.Config{
		Vcpu:      bb.Config.VCpuCount,
		RamMB:     bb.Config.MemoryMB,
		HugePages: bb.Config.HugePages,

		// Allow sandbox internet access during provisioning
		Network: &orchestrator.SandboxNetworkConfig{},

		Envd: sandbox.EnvdMetadata{
			Version: bb.EnvdVersion,
		},

		VMMConfig: vmm.VMMConfig{
			Type:          vmm.BackendType(bb.Config.VMMType).OrDefault(),
			KernelVersion: bb.Config.KernelVersion,
			VMMVersion:    bb.Config.FirecrackerVersion,
			OsType:        bb.Config.GuestOS(),
		},
	}
	err = bb.provisionSandbox(
		ctx,
		userLogger,
		baseSbxConfig,
		sandbox.RuntimeMetadata{
			TemplateID:  bb.Config.TemplateID,
			SandboxID:   config.InstanceBuildPrefix + id.Generate(),
			ExecutionID: uuid.NewString(),
		},
		localTemplate,
		rootfsPath,
		provisionLogPrefix,
	)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error provisioning sandbox: %w", err)
	}

	// Check the rootfs filesystem corruption
	ext4Check, err := filesystem.CheckIntegrity(ctx, rootfsPath, true)
	if err != nil {
		logger.L().Error(ctx, "provisioned filesystem ext4 integrity",
			zap.String("result", ext4Check),
			zap.Error(err),
		)

		return metadata.Template{}, fmt.Errorf("error checking provisioned filesystem integrity: %w", err)
	}
	logger.L().Debug(ctx, "provisioned filesystem ext4 integrity",
		zap.String("result", ext4Check),
	)

	err = bb.enlargeDiskAfterProvisioning(ctx, bb.Config, rootfs)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error enlarging disk after provisioning: %w", err)
	}

	// Create sandbox for building template
	userLogger.Debug(ctx, "Creating base sandbox template layer")

	sandboxCreator := layer.NewCreateSandbox(
		baseSbxConfig,
		bb.sandboxFactory,
		baseLayerTimeout,
		layer.WithRootfsCachePath(rootfsPath),
	)

	actionExecutor := layer.NewFunctionAction(func(ctx context.Context, sbx *sandbox.Sandbox, meta metadata.Template) (metadata.Template, error) {
		err = sandboxtools.SyncChangesToDisk(
			ctx,
			bb.proxy,
			sbx.Runtime.SandboxID,
			bb.Config.GuestOS(),
		)
		if err != nil {
			return metadata.Template{}, fmt.Errorf("error running sync command: %w", err)
		}

		return meta, nil
	})

	templateProvider := layer.NewDirectSourceTemplateProvider(localTemplate)

	baseLayer, err := bb.layerExecutor.BuildLayer(
		ctx,
		userLogger,
		layer.LayerBuildCommand{
			SourceTemplate: templateProvider,
			CurrentLayer:   baseMetadata,
			Hash:           hash,
			UpdateEnvd:     false,
			SandboxCreator: sandboxCreator,
			ActionExecutor: actionExecutor,
		},
	)
	if err != nil {
		return metadata.Template{}, fmt.Errorf("error building base layer: %w", err)
	}

	return baseLayer, nil
}

func (bb *BaseBuilder) Layer(
	ctx context.Context,
	_ phases.LayerResult,
	hash string,
) (phases.LayerResult, error) {
	ctx, span := tracer.Start(ctx, "compute base", trace.WithAttributes(
		attribute.String("hash", hash),
	))
	defer span.End()

	switch {
	case bb.Config.FromTemplate != nil:
		sourceMeta := metadata.FromTemplate{
			Alias:   bb.Config.FromTemplate.GetAlias(),
			BuildID: bb.Config.FromTemplate.GetBuildID(),
		}

		// If the template is built from another template, use its metadata
		tm, err := bb.index.Cached(ctx, bb.Config.FromTemplate.GetBuildID())
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				return phases.LayerResult{}, phases.NewPhaseBuildError(bb.Metadata(), fmt.Errorf("error getting base template, you may need to rebuild it first"))
			}

			return phases.LayerResult{}, fmt.Errorf("error getting base template: %w", err)
		}

		// From template is always cached, never needs to be built
		return phases.LayerResult{
			Metadata: tm.BasedOn(sourceMeta),
			Hash:     hash,
			Cached:   true,
		}, nil
	default:
		cmdMeta := metadata.Context{
			User:    defaultUser,
			WorkDir: nil,
			EnvVars: make(map[string]string),
		}

		// This is a compatibility for v1 template builds
		if bb.IsV1Build {
			cmdMeta.WorkDir = utils.ToPtr("/home/user")
		}

		meta := metadata.Template{
			Version: metadata.CurrentVersion,
			Template: metadata.TemplateMetadata{
				BuildID:            uuid.New().String(),
				KernelVersion:      bb.Config.KernelVersion,
				FirecrackerVersion: bb.Config.FirecrackerVersion,
				VMMType:            string(vmm.BackendType(bb.Config.VMMType).OrDefault()),
				OsType:             string(bb.Config.GuestOS()),
			},
			Context:      cmdMeta,
			FromTemplate: nil,
			Start:        nil,
		}

		if bb.Config.UsesRawImage() {
			meta.FromImageRaw = &bb.Config.FromImageRaw
		} else {
			meta.FromImage = &bb.Config.FromImage
		}
		if bb.Config.IsWindows() {
			meta.Context.User = ""
			meta.Context.WorkDir = nil
			meta.Context.OsType = string(vmm.OsWindows)
		}
		if bb.Config.IsAndroid() {
			meta.Context.User = ""
			meta.Context.WorkDir = nil
			meta.Context.OsType = string(vmm.OsAndroid)
			meta.Template.OsType = string(vmm.OsAndroid)
		}

		notCachedResult := phases.LayerResult{
			Metadata: meta,
			Cached:   false,
			Hash:     hash,
		}

		// Invalidate base cache
		if bb.Config.Force != nil && *bb.Config.Force {
			return notCachedResult, nil
		}

		bm, err := bb.index.LayerMetaFromHash(ctx, hash)
		if err != nil {
			bb.logger.Info(ctx, "base layer not found in cache, building new base layer", zap.Error(err), zap.String("hash", hash))

			return notCachedResult, nil
		}

		meta, err = bb.index.Cached(ctx, bm.Template.BuildID)
		if err != nil {
			logger.L().Info(ctx, "base layer metadata not found in cache, building new base layer", zap.Error(err), zap.String("hash", hash))

			return notCachedResult, nil
		}

		if bb.Config.UsesRawImage() && bb.Config.IsWindows() && meta.Template.EnvdVersion == "" {
			logger.L().Info(ctx, "raw Windows base layer metadata missing guest envd version, building new base layer", zap.String("hash", hash))

			return notCachedResult, nil
		}

		return phases.LayerResult{
			Metadata: meta,
			Cached:   true,
			Hash:     hash,
		}, nil
	}
}
