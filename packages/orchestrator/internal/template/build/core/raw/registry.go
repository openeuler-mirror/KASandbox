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

type ResolvedSource struct {
	RequestedRef   string
	ResolvedRef    string
	ManifestDigest string
	LayerDigest    string
}

func Resolve(ctx context.Context, source Source, authProvider auth.RegistryAuthProvider) (ResolvedSource, error) {
	desc, err := getRegistryDescriptor(ctx, source, authProvider)
	if err != nil {
		return ResolvedSource{}, err
	}
	img, err := desc.Image()
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("error reading raw image from registry: %w", err)
	}
	manifestDigest, err := img.Digest()
	if err != nil {
		return ResolvedSource{}, err
	}
	layer, err := selectRawLayer(img)
	if err != nil {
		return ResolvedSource{}, err
	}
	if manifestDigest.Algorithm != "sha256" || layer.Digest.Algorithm != "sha256" {
		return ResolvedSource{}, fmt.Errorf("raw image requires SHA-256 manifest and layer digests")
	}
	ref, err := parseRegistryReference(source)
	if err != nil {
		return ResolvedSource{}, err
	}
	return ResolvedSource{
		RequestedRef:   source.Ref,
		ResolvedRef:    ref.Context().Digest(manifestDigest.String()).Name(),
		ManifestDigest: manifestDigest.String(),
		LayerDigest:    layer.Digest.String(),
	}, nil
}

func FetchResolved(ctx context.Context, resolved ResolvedSource, destPath string, authProvider auth.RegistryAuthProvider) (e error) {
	ref, err := name.NewDigest(resolved.ResolvedRef, name.StrictValidation)
	if err != nil {
		return fmt.Errorf("invalid resolved raw reference: %w", err)
	}
	if ref.DigestStr() != resolved.ManifestDigest {
		return fmt.Errorf("resolved raw manifest digest does not match reference")
	}
	desc, err := getRegistryDescriptor(ctx, Source{Ref: ref.Name()}, authProvider)
	if err != nil {
		return err
	}
	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("error reading resolved raw image: %w", err)
	}
	layerDesc, err := selectRawLayer(img)
	if err != nil {
		return err
	}
	if layerDesc.Digest.String() != resolved.LayerDigest {
		return fmt.Errorf("raw layer digest mismatch: expected %s, got %s", resolved.LayerDigest, layerDesc.Digest)
	}
	layer, err := img.LayerByDigest(layerDesc.Digest)
	if err != nil {
		return err
	}
	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("error opening raw image layer %s: %w", layerDesc.Digest, err)
	}
	defer func() { e = errors.Join(e, rc.Close()) }()
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("error creating raw image file: %w", err)
	}
	defer func() {
		e = errors.Join(e, f.Close())
		if e != nil {
			e = errors.Join(e, os.Remove(destPath))
		}
	}()
	// The registry reader verifies the layer digest when read to EOF.
	if _, err := io.Copy(f, rc); err != nil {
		return fmt.Errorf("error downloading raw image layer: %w", err)
	}
	return nil
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
