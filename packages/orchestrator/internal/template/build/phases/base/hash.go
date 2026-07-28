package base

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/phases"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/storage/cache"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func rawImageBaseSource(osType vmm.OsType, url, persistentDigest string, sdcardSizeMB int64, generatorVersion string) (string, error) {
	switch osType {
	case vmm.OsWindows:
		return fmt.Sprintf("raw:windows:%s", url), nil
	case vmm.OsAndroid:
		return fmt.Sprintf("raw:android:%s:%s:%d:%s", url, persistentDigest, sdcardSizeMB, generatorVersion), nil
	default:
		return "", fmt.Errorf("unsupported raw-image guest OS %q", osType)
	}
}

func (bb *BaseBuilder) Hash(ctx context.Context, _ phases.LayerResult) (string, error) {
	ctx, span := tracer.Start(ctx, "hash base")
	defer span.End()

	var baseSource string
	var err error
	switch {
	case bb.Config.FromTemplate != nil:
		// When building from template, use the base template metadata
		baseSource = fmt.Sprintf("template:%s", bb.Config.FromTemplate.GetBuildID())
	case bb.Config.UsesRawImage():
		persistentDigest := ""
		if bb.Config.IsAndroid() {
			persistentDigest, err = bb.androidPersistentDigest(ctx)
			if err != nil {
				return "", err
			}
		}
		baseSource, err = rawImageBaseSource(bb.Config.GuestOS(), bb.Config.FromImageRaw, persistentDigest, androidSDCardImageSizeMB, androidSDCardGeneratorVersion)
		if err != nil {
			return "", err
		}
	default:
		// Note: When "default" tag is used, the cached version might become ambiguous (not always default)
		// To update it now, you need to force the rebuild of the template, which will update this layer for all templates
		// in the team. This is okay for now, as the cache is not shared between teams, but it might need to be changed
		// when global caches are implemented.

		// When building from image, use the image name
		baseSource = bb.Config.FromImage
	}

	provisionVersion := provisionScriptFile
	if val := bb.featureFlags.IntFlag(
		ctx,
		featureflags.BuildProvisionVersion,
		featureflags.TemplateContext(bb.Config.TemplateID),
		featureflags.TeamContext(bb.Config.TeamID),
		// for dev environments (fallback value), use the provision script hash
	); val != featureflags.BuildProvisionVersion.Fallback() {
		provisionVersion = strconv.FormatInt(int64(val), 10)
	}

	telemetry.SetAttributes(ctx,
		attribute.String("index_version", bb.index.Version()),
		attribute.String("provision_version", provisionVersion),
		attribute.String("base_source", baseSource),
		attribute.Int64("disk_size_mb", bb.Config.DiskSizeMB),
	)

	return cache.HashKeys(
		bb.index.Version(),
		provisionVersion,
		strconv.FormatInt(bb.Config.DiskSizeMB, 10),
		baseSource,
	), nil
}
