package raw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci/auth"
)

func downloadRegistryRawImage(ctx context.Context, source Source, destPath string, authProvider auth.RegistryAuthProvider) (e error) {
	layerDesc, layer, err := pullRawLayer(ctx, source, authProvider)
	if err != nil {
		return err
	}

	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("error opening raw image layer %s: %w", layerDesc.Digest, err)
	}
	defer func() {
		e = errors.Join(e, rc.Close())
	}()

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("error creating raw image file: %w", err)
	}
	defer func() {
		e = errors.Join(e, f.Close())
	}()

	if _, err := io.Copy(f, rc); err != nil {
		return fmt.Errorf("error writing raw image layer: %w", err)
	}

	return nil
}

func pullRawLayer(ctx context.Context, source Source, authProvider auth.RegistryAuthProvider) (v1.Descriptor, v1.Layer, error) {
	desc, err := getRegistryDescriptor(ctx, source, authProvider)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}

	img, err := desc.Image()
	if err != nil {
		return v1.Descriptor{}, nil, fmt.Errorf("error reading raw image from registry: %w", err)
	}

	layerDesc, err := selectRawLayer(img)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}

	layer, err := img.LayerByDigest(layerDesc.Digest)
	if err != nil {
		return v1.Descriptor{}, nil, fmt.Errorf("error locating raw image layer %s: %w", layerDesc.Digest, err)
	}

	return layerDesc, layer, nil
}

func getRegistryDescriptor(ctx context.Context, source Source, authProvider auth.RegistryAuthProvider) (*remote.Descriptor, error) {
	ref, err := parseRegistryReference(source)
	if err != nil {
		return nil, err
	}

	opts := []remote.Option{remote.WithContext(ctx)}
	if authProvider != nil {
		authOption, err := authProvider.GetAuthOption(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting registry authentication: %w", err)
		}
		if authOption != nil {
			opts = append(opts, authOption)
		}
	}

	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("error pulling raw image %q from registry: %w", source.Ref, err)
	}

	return desc, nil
}

func parseRegistryReference(source Source) (name.Reference, error) {
	opts := []name.Option{name.StrictValidation}

	ref, err := name.ParseReference(source.Ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("invalid raw image registry reference %q: %w", source.Ref, err)
	}

	return ref, nil
}

func selectRawLayer(img v1.Image) (v1.Descriptor, error) {
	manifest, err := img.Manifest()
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("error reading raw image manifest: %w", err)
	}

	for _, layer := range manifest.Layers {
		if layer.MediaType == types.MediaType(RawLayerMediaType) {
			return layer, nil
		}
	}

	if len(manifest.Layers) == 1 {
		return manifest.Layers[0], nil
	}

	return v1.Descriptor{}, fmt.Errorf("raw image must contain exactly one layer or one %s layer", RawLayerMediaType)
}
