package testfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// AddAndroid converts one fixture Build to Android. Each disk deliberately has a
// different dependency shape; the sdcard includes a zero-filled extent.
func AddAndroid(root, buildID string) error {
	objects := filepath.Join(root, "source", "objects")
	metadataPath := filepath.Join(objects, buildID, "metadata.json")
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	template := meta["template"].(map[string]any)
	template["os_type"] = "android"
	template["vmm_type"] = "stratovirt"
	template["android_version"] = "14"
	template["future_field"] = "preserve-me"
	if err := writeJSON(metadataPath, meta); err != nil {
		return err
	}
	for name, mappings := range map[string][]Mapping{
		"persistent.img": {{0, 4, AncestorBuildID, 4}, {4, 4, buildID, 0}},
		"sdcard.img":     {{0, 4, AncestorBuildID, 0}, {4, 4, "00000000-0000-0000-0000-000000000000", 0}},
	} {
		header, err := encodeHeader(buildID, AncestorBuildID, 3, 4, 8, 2, mappings)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(objects, buildID, name+".header"), header); err != nil {
			return err
		}
	}
	for key, value := range map[string]string{
		AncestorBuildID + "/persistent.img": "unused!!",
		buildID + "/persistent.img":         "PNEW",
		AncestorBuildID + "/sdcard.img":     "SDAT",
	} {
		if err := writeFile(filepath.Join(objects, filepath.FromSlash(key)), []byte(value)); err != nil {
			return err
		}
	}
	return nil
}
