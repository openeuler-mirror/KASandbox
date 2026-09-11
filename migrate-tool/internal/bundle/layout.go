package bundle

import (
	"fmt"
	"slices"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
)

// Inspect checks the declared layout without reading object bytes. Verify then
// binds it to the original metadata.json and checks every Header dependency.
func validateLayouts(manifest Manifest, records Records) (map[string]string, error) {
	layouts := make(map[string]string, len(records.Builds))
	for _, build := range records.Builds {
		layouts[build.ID] = artifact.OSLinux
	}
	if manifest.FormatVersion == FormatVersion {
		if len(manifest.BuildLayouts) != 0 {
			return nil, fmt.Errorf("bundle v1 cannot declare build_layouts")
		}
		return layouts, nil
	}
	if len(manifest.BuildLayouts) != len(records.Builds) {
		return nil, fmt.Errorf("bundle v2 requires one layout per selected Build")
	}
	seen := make(map[string]bool, len(manifest.BuildLayouts))
	for _, layout := range manifest.BuildLayouts {
		if _, ok := layouts[layout.BuildID]; !ok {
			return nil, fmt.Errorf("layout references unknown build %q", layout.BuildID)
		}
		if seen[layout.BuildID] {
			return nil, fmt.Errorf("duplicate layout for build %q", layout.BuildID)
		}
		seen[layout.BuildID] = true
		if layout.OSType != artifact.OSLinux && layout.OSType != artifact.OSAndroid {
			return nil, fmt.Errorf("unsupported layout os_type %q", layout.OSType)
		}
		if !slices.Equal(layout.Disks, artifact.DiskNames(layout.OSType)) {
			return nil, fmt.Errorf("invalid %s disk layout for build %q: %v", layout.OSType, layout.BuildID, layout.Disks)
		}
		layouts[layout.BuildID] = layout.OSType
	}
	return layouts, nil
}
