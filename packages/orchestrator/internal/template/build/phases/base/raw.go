package base

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

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

const (
	androidSDCardImageSizeMB      int64  = 2048
	androidSDCardGeneratorVersion string = "1"

	androidSDCardSectorSize      int64 = 512
	androidSDCardOffsetBytes     int64 = 1 << 20
	androidSDCardFirstLBA              = uint32(androidSDCardOffsetBytes / androidSDCardSectorSize)
	androidSDCardPartitionType   byte  = 0x0c
	androidSDCardPartitionOffset       = 446
)

func constructLayerFilesFromAndroidRaw(
	ctx context.Context,
	userLogger logger.Logger,
	buildContext buildcontext.BuildContext,
	baseBuildID string,
	rawImageURL string,
	persistentSourcePath string,
	persistentDigest string,
	newfsMsdosPath string,
	dir string,
	authProvider auth.RegistryAuthProvider,
) (_ []sbxtemplate.Disk, _ block.ReadonlyDevice, resultErr error) {
	buildID, err := uuid.Parse(baseBuildID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse build id: %w", err)
	}

	specs := []struct {
		name    string
		typ     build.DiffType
		prepare func(string) error
	}{
		{storage.RootfsName, build.Rootfs, func(path string) error {
			return fetchRawImage(ctx, userLogger, rawImageURL, path, authProvider)
		}},
		{storage.PersistentName, build.RootfsPersistent, func(path string) error {
			return copyPersistentImage(persistentSourcePath, path, persistentDigest)
		}},
		{storage.SDCardName, build.RootfsSDCard, func(path string) error {
			return generateAndroidSDCard(ctx, newfsMsdosPath, path)
		}},
	}
	result := make([]sbxtemplate.Disk, 0, len(specs))
	defer func() {
		if resultErr == nil {
			return
		}
		for i := len(result) - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, result[i].Device.Close())
		}
	}()

	for _, spec := range specs {
		path := filepath.Join(dir, spec.name)
		if err := spec.prepare(path); err != nil {
			return nil, nil, fmt.Errorf("error preparing %s disk: %w", spec.name, err)
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

// constructLayerFilesFromWindowsRaw registers a Windows raw disk image as the template rootfs.
// Unlike the OCI import path it performs no filesystem conversion: the
// image is fetched as-is, padded to the rootfs block size, and wrapped as the
// rootfs device. The guest image already contains envd, so no provisioning
// happens here.
func constructLayerFilesFromWindowsRaw(
	ctx context.Context,
	userLogger logger.Logger,
	buildContext buildcontext.BuildContext,
	// The base build ID can be different from the final requested template build ID.
	baseBuildID string,
	rawImageURL string,
	rootfsPath string,
	authProvider auth.RegistryAuthProvider,
) (r *block.Local, m block.ReadonlyDevice, c containerregistry.Config, e error) {
	ctx, span := tracer.Start(ctx, "construct-layer-files-from-windows-raw")
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

func validateRegularNonemptyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%q is empty", path)
	}

	return nil
}

func validateExecutableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable", path)
	}

	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open %q: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%q is empty", path)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", fmt.Errorf("failed to hash %q: %w", path, err)
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func copyPersistentImage(sourcePath, targetPath, expectedDigest string) (resultErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to open persistent image %q: %w", sourcePath, err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, source.Close())
	}()

	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat persistent image %q: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("persistent image %q is not a regular file", sourcePath)
	}
	if info.Size() == 0 {
		return fmt.Errorf("persistent image %q is empty", sourcePath)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return fmt.Errorf("failed to create persistent image copy %q: %w", targetPath, err)
	}
	complete := false
	targetClosed := false
	defer func() {
		if !targetClosed {
			resultErr = errors.Join(resultErr, target.Close())
		}
		if !complete {
			resultErr = errors.Join(resultErr, removeFileIfExists(targetPath))
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(target, hash), source); err != nil {
		return fmt.Errorf("failed to copy persistent image: %w", err)
	}

	actualDigest := fmt.Sprintf("%x", hash.Sum(nil))
	if actualDigest != expectedDigest {
		return fmt.Errorf("persistent image changed during build: expected SHA-256 %s, got %s", expectedDigest, actualDigest)
	}
	if err := target.Close(); err != nil {
		targetClosed = true
		return fmt.Errorf("failed to close persistent image copy: %w", err)
	}
	targetClosed = true

	complete = true
	return nil
}

func androidSDCardImageSizeBytes() int64 {
	return androidSDCardImageSizeMB << 20
}

func androidSDCardPartitionSectors() uint32 {
	return uint32((androidSDCardImageSizeBytes() - androidSDCardOffsetBytes) / androidSDCardSectorSize)
}

func generateAndroidSDCard(ctx context.Context, toolPath, targetPath string) (resultErr error) {
	complete := false
	defer func() {
		if !complete {
			resultErr = errors.Join(resultErr, removeFileIfExists(targetPath))
		}
	}()

	sectorCount := androidSDCardPartitionSectors()
	args := []string{
		"-F", "32",
		"-m", "0xf8",
		"-o", "0",
		"-c", "8",
		"-h", "255",
		"-u", "63",
		"-S", strconv.FormatInt(androidSDCardSectorSize, 10),
		"-s", strconv.FormatUint(uint64(sectorCount), 10),
		"-C", strconv.FormatInt(androidSDCardImageSizeMB, 10) + "M",
		"-@", strconv.FormatInt(androidSDCardOffsetBytes, 10),
		targetPath,
	}
	output, err := exec.CommandContext(ctx, toolPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("newfs_msdos failed: %w: %s", err, string(output))
	}

	mbr := make([]byte, androidSDCardSectorSize)
	mbr[androidSDCardPartitionOffset+4] = androidSDCardPartitionType
	binary.LittleEndian.PutUint32(mbr[androidSDCardPartitionOffset+8:], androidSDCardFirstLBA)
	binary.LittleEndian.PutUint32(mbr[androidSDCardPartitionOffset+12:], sectorCount)
	mbr[510] = 0x55
	mbr[511] = 0xaa

	f, err := os.OpenFile(targetPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open sdcard image for MBR write: %w", err)
	}
	if _, err := f.WriteAt(mbr, 0); err != nil {
		return errors.Join(fmt.Errorf("failed to write sdcard MBR: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close sdcard image after MBR write: %w", err)
	}

	if err := validateAndroidSDCard(targetPath); err != nil {
		return err
	}

	complete = true
	return nil
}

func removeFileIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

func validateAndroidSDCard(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat sdcard image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("sdcard image is not a regular file")
	}
	if info.Size() != androidSDCardImageSizeBytes() {
		return fmt.Errorf("invalid sdcard image size: got %d, want %d", info.Size(), androidSDCardImageSizeBytes())
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open sdcard image for validation: %w", err)
	}
	defer f.Close()

	mbr := make([]byte, androidSDCardSectorSize)
	if _, err := io.ReadFull(f, mbr); err != nil {
		return fmt.Errorf("failed to read sdcard MBR: %w", err)
	}
	if got := mbr[androidSDCardPartitionOffset+4]; got != androidSDCardPartitionType {
		return fmt.Errorf("invalid sdcard partition type: got 0x%02x, want 0x%02x", got, androidSDCardPartitionType)
	}
	if got := binary.LittleEndian.Uint32(mbr[androidSDCardPartitionOffset+8:]); got != androidSDCardFirstLBA {
		return fmt.Errorf("invalid sdcard first LBA: got %d, want %d", got, androidSDCardFirstLBA)
	}
	if got := binary.LittleEndian.Uint32(mbr[androidSDCardPartitionOffset+12:]); got != androidSDCardPartitionSectors() {
		return fmt.Errorf("invalid sdcard sector count: got %d, want %d", got, androidSDCardPartitionSectors())
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return fmt.Errorf("invalid sdcard MBR signature")
	}

	return nil
}
