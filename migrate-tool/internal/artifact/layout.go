package artifact

import (
	"encoding/json"
	"fmt"
	"path"
)

const (
	OSLinux              = "linux"
	OSAndroid            = "android"
	TypePersistentHeader = "persistent-header"
	TypeSDCardHeader     = "sdcard-header"
	TypePersistent       = "persistent"
	TypeSDCard           = "sdcard"
)

// Layer binds a Header to its own data filename, including historical Builds.
// Only these fixed layers are supported; metadata cannot inject arbitrary paths.
type Layer struct {
	Name       string
	HeaderType string
	DataType   string
}

func (l Layer) HeaderKey(buildID string) string { return path.Join(buildID, l.Name+".header") }
func (l Layer) DataKey(buildID string) string   { return path.Join(buildID, l.Name) }

func Layers(osType string) []Layer {
	layers := []Layer{{"memfile", TypeMemfileHeader, TypeMemfile}, {"rootfs.ext4", TypeRootfsHeader, TypeRootfs}}
	if osType == OSAndroid {
		layers = append(layers, Layer{"persistent.img", TypePersistentHeader, TypePersistent}, Layer{"sdcard.img", TypeSDCardHeader, TypeSDCard})
	}
	return layers
}

func DiskNames(osType string) []string {
	var disks []string
	for _, layer := range Layers(osType)[1:] {
		disks = append(disks, layer.Name)
	}
	return disks
}

func IsDataType(objectType string) bool {
	for _, layer := range Layers(OSAndroid) {
		if objectType == layer.DataType {
			return true
		}
	}
	return false
}

func MatchesFilename(filename, objectType string) bool {
	for _, layer := range Layers(OSAndroid) {
		if objectType == layer.DataType {
			return filename == layer.Name
		}
		if objectType == layer.HeaderType {
			return filename == layer.Name+".header"
		}
	}
	return objectType == TypeMetadata && filename == "metadata.json" || objectType == TypeSnapfile && filename == "snapfile"
}

// MetadataOS reads only migration-relevant fields. Callers retain the original
// bytes so VMM, Android version and future runtime metadata survive unchanged.
func MetadataOS(raw []byte, buildID string) (string, error) {
	var metadata struct {
		Template struct {
			BuildID string `json:"build_id"`
			OSType  string `json:"os_type"`
		} `json:"template"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", fmt.Errorf("decode metadata for build %q: %w", buildID, err)
	}
	if metadata.Template.BuildID != buildID {
		return "", fmt.Errorf("metadata build id is %q, expected %q", metadata.Template.BuildID, buildID)
	}
	switch metadata.Template.OSType {
	case "", OSLinux:
		return OSLinux, nil
	case OSAndroid:
		return OSAndroid, nil
	default:
		return "", fmt.Errorf("unsupported template os_type %q for build %q", metadata.Template.OSType, buildID)
	}
}
