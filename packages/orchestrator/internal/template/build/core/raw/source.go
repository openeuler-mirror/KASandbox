package raw

import (
	"fmt"
	"strings"
)

const RawLayerMediaType = "application/vnd.e2b.raw.image"

type Source struct {
	Ref string
}

func ParseSource(rawURL string) (Source, error) {
	ref := strings.TrimSpace(rawURL)
	if ref == "" {
		return Source{}, fmt.Errorf("raw image registry reference must not be empty")
	}
	if strings.Contains(ref, "://") {
		return Source{}, fmt.Errorf("raw image must be a registry reference without a URL scheme, got %q", rawURL)
	}

	source := Source{Ref: ref}
	if _, err := parseRegistryReference(source); err != nil {
		return Source{}, err
	}

	return source, nil
}
