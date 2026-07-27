package base

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/buildcontext"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci/auth"
	coreraw "github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/raw"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/constants"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func constructLayerFilesFromMultiDisk(
	ctx context.Context,
	userLogger logger.Logger,
	buildContext buildcontext.BuildContext,
	baseBuildID string,
	disks *config.MultiDiskConfig,
	dir string,
	authProvider auth.RegistryAuthProvider,
) ([]sbxtemplate.Disk, block.ReadonlyDevice, error) {
	buildID, err := uuid.Parse(baseBuildID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse build id: %w", err)
	}

	specs := []struct {
		name string
		url  string
		typ  build.DiffType
	}{{storage.RootfsName, disks.OS, build.Rootfs}, {storage.PersistentName, disks.Persistent, build.RootfsPersistent}, {storage.SDCardName, disks.SDCard, build.RootfsSDCard}}
	result := make([]sbxtemplate.Disk, 0, len(specs))
	for _, spec := range specs {
		path := filepath.Join(dir, spec.name)
		if err := fetchRawImage(ctx, userLogger, spec.url, path, authProvider); err != nil {
			return nil, nil, fmt.Errorf("error fetching %s disk: %w", spec.name, err)
		}
		if err := alignFileToBlockSize(path, buildContext.Config.RootfsBlockSize()); err != nil {
			return nil, nil, fmt.Errorf("error aligning %s disk: %w", spec.name, err)
		}
		device, err := block.NewLocal(path, buildContext.Config.RootfsBlockSize(), buildID)
		if err != nil {
			return nil, nil, fmt.Errorf("error opening %s disk: %w", spec.name, err)
		}
		result = append(result, sbxtemplate.Disk{Name: spec.name, DiffType: spec.typ, Device: device})
	}

	memfile, err := block.NewEmpty(buildContext.Config.MemoryMB<<constants.ToMBShift, config.MemfilePageSize(buildContext.Config.HugePages), buildID)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating memfile: %w", err)
	}
	return result, memfile, nil
}

// constructLayerFilesFromRaw registers a raw disk image as the template rootfs.
// Unlike the OCI import path it performs no filesystem conversion: the
// image is fetched as-is, padded to the rootfs block size, and wrapped as the
// rootfs device. The guest image already contains envd, so no provisioning
// happens here.
func constructLayerFilesFromRaw(
	ctx context.Context,
	userLogger logger.Logger,
	buildContext buildcontext.BuildContext,
	// The base build ID can be different from the final requested template build ID.
	baseBuildID string,
	rawImageURL string,
	rootfsPath string,
	authProvider auth.RegistryAuthProvider,
) (r *block.Local, m block.ReadonlyDevice, c containerregistry.Config, e error) {
	ctx, span := tracer.Start(ctx, "construct-layer-files-from-raw")
	defer span.End()

	if err := fetchRawImage(ctx, userLogger, rawImageURL, rootfsPath, authProvider); err != nil {
		return nil, nil, containerregistry.Config{}, fmt.Errorf("error fetching raw image: %w", err)
	}

	// The COW/diff storage works in fixed-size blocks, so the raw disk must be a
	// whole number of blocks.
	if err := alignFileToBlockSize(rootfsPath, buildContext.Config.RootfsBlockSize()); err != nil {
		return nil, nil, containerregistry.Config{}, fmt.Errorf("error aligning raw image to block size: %w", err)
	}

	buildIDParsed, err := uuid.Parse(baseBuildID)
	if err != nil {
		return nil, nil, containerregistry.Config{}, fmt.Errorf("failed to parse build id: %w", err)
	}

	rootfs, err := block.NewLocal(rootfsPath, buildContext.Config.RootfsBlockSize(), buildIDParsed)
	if err != nil {
		return nil, nil, containerregistry.Config{}, fmt.Errorf("error reading rootfs blocks: %w", err)
	}

	// Create empty memfile (no booted-VM memory snapshot at this point).
	memfile, err := block.NewEmpty(
		buildContext.Config.MemoryMB<<constants.ToMBShift,
		config.MemfilePageSize(buildContext.Config.HugePages),
		buildIDParsed,
	)
	if err != nil {
		return nil, nil, containerregistry.Config{}, errors.Join(fmt.Errorf("error creating memfile: %w", err), rootfs.Close())
	}

	return rootfs, memfile, containerregistry.Config{}, nil
}

// fetchRawImage downloads the raw disk image at url to destPath.
func fetchRawImage(ctx context.Context, userLogger logger.Logger, url, destPath string, authProvider auth.RegistryAuthProvider) error {
	source, err := coreraw.ParseSource(url)
	if err != nil {
		return err
	}

	userLogger.Info(ctx, "Downloading raw disk image")
	return coreraw.Fetch(ctx, source, destPath, authProvider)
}

// alignFileToBlockSize pads the file at path with zeros so its size is a whole
// multiple of blockSize. An empty file is rejected as an invalid disk image.
func alignFileToBlockSize(path string, blockSize int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("error stating raw image: %w", err)
	}

	size := info.Size()
	if size == 0 {
		return fmt.Errorf("raw image is empty")
	}

	aligned := alignUp(size, blockSize)
	if aligned == size {
		return nil
	}

	if err := os.Truncate(path, aligned); err != nil {
		return fmt.Errorf("error padding raw image to block size: %w", err)
	}

	return nil
}

// alignUp rounds size up to the next multiple of blockSize.
func alignUp(size, blockSize int64) int64 {
	if blockSize <= 0 {
		return size
	}

	remainder := size % blockSize
	if remainder == 0 {
		return size
	}

	return size + (blockSize - remainder)
}
