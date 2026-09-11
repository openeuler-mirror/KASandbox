package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	artifactheader "gitcode.com/openeuler/KASandbox/migrate-tool/internal/header"
)

// Inspected 表示 Bundle 的结构已经过校验。Inspect 会读取 manifest 和体积很小的
// JSONL 关系记录，但不会读取 objects 下的大对象内容。
type Inspected struct {
	Root     string
	Manifest Manifest
	Records  Records
	layouts  map[string]string
}

// Verified 只由 Verify 返回。嵌入 Inspected 后，导入流程仍可直接访问 Root、
// Manifest 和 Records，同时类型本身明确表示大对象内容也已经校验。
type Verified struct {
	*Inspected
}

type controlRequirement struct {
	objectType string
	buildID    string
}

func WriteManifest(root string, manifest *Manifest) error {
	digest, err := manifestDigest(*manifest)
	if err != nil {
		return err
	}
	manifest.BundleDigest = digest
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

func Inspect(root string) (*Inspected, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle path: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(abs, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode manifest: multiple JSON values")
		}
		return nil, fmt.Errorf("decode manifest trailing data: %w", err)
	}
	if manifest.Format != Format || (manifest.FormatVersion != FormatVersion && manifest.FormatVersion != MultiDiskFormatVersion) {
		return nil, fmt.Errorf("unsupported bundle format %q version %d", manifest.Format, manifest.FormatVersion)
	}
	expectedDigest := manifest.BundleDigest
	actualDigest, err := manifestDigest(manifest)
	if err != nil {
		return nil, err
	}
	if actualDigest != expectedDigest {
		return nil, fmt.Errorf("bundle digest mismatch: got %s, want %s", actualDigest, expectedDigest)
	}
	if err := validateCounts(manifest.Counts); err != nil {
		return nil, err
	}
	if err := validateDescriptors(manifest.Records); err != nil {
		return nil, err
	}

	records, err := ReadRecords(abs, manifest.Records)
	if err != nil {
		return nil, err
	}
	if err := verifyCounts(manifest.Counts, records, manifest.Objects); err != nil {
		return nil, err
	}
	layouts, err := validateLayouts(manifest, records)
	if err != nil {
		return nil, err
	}
	if err := validateObjects(manifest.Objects, records, layouts); err != nil {
		return nil, err
	}

	return &Inspected{Root: abs, Manifest: manifest, Records: records, layouts: layouts}, nil
}

func Verify(root string) (*Verified, error) {
	inspected, err := Inspect(root)
	if err != nil {
		return nil, err
	}
	if err := verifyObjectContents(inspected.Root, inspected.Manifest.Objects); err != nil {
		return nil, err
	}
	if err := verifyObjectClosure(inspected.Root, inspected.Records, inspected.Manifest.Objects, inspected.layouts); err != nil {
		return nil, err
	}
	return &Verified{Inspected: inspected}, nil
}

func validateDescriptors(descriptors []RecordDescriptor) error {
	// v1 的记录集合是封闭协议，不把未知文件悄悄当成可忽略的扩展。
	if len(descriptors) != len(recordOrder) {
		return fmt.Errorf("bundle v1 requires %d record descriptors, got %d", len(recordOrder), len(descriptors))
	}
	for index, name := range recordOrder {
		descriptor := descriptors[index]
		expectedPath := "records/" + name + ".jsonl"
		if descriptor.Name != name {
			return fmt.Errorf("record descriptor %d has name %q, want %q", index, descriptor.Name, name)
		}
		if descriptor.Path != expectedPath {
			return fmt.Errorf("record descriptor %q has path %q, want %q", name, descriptor.Path, expectedPath)
		}
		if descriptor.Count < 0 {
			return fmt.Errorf("record descriptor %q has negative count %d", name, descriptor.Count)
		}
		if err := validateSHA256(descriptor.SHA256); err != nil {
			return fmt.Errorf("record descriptor %q: %w", name, err)
		}
	}
	return nil
}

func validateSHA256(digest string) error {
	if len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return fmt.Errorf("invalid SHA-256 %q", digest)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("invalid SHA-256 %q: %w", digest, err)
	}
	return nil
}

func ObjectBundlePath(digest string) (string, error) {
	if err := validateSHA256(digest); err != nil {
		return "", err
	}
	return path.Join("objects", "sha256", digest[:2], digest), nil
}

func safeBundlePath(root, relative string) (string, error) {
	if relative == "" || strings.ContainsRune(relative, '\\') {
		return "", fmt.Errorf("invalid bundle path %q", relative)
	}
	clean := path.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("bundle path escapes root: %q", relative)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.BundleDigest = ""
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode bundle digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateObjects(objects []ObjectRecord, records Records, layouts map[string]string) error {
	seen := make(map[string]struct{}, len(objects))
	builds := make(map[string]struct{}, len(records.Builds))
	requiredControls := make(map[string]controlRequirement, len(records.Builds)*4)
	for _, build := range records.Builds {
		builds[build.ID] = struct{}{}
		for _, layer := range artifact.Layers(layouts[build.ID]) {
			requiredControls[layer.HeaderKey(build.ID)] = controlRequirement{objectType: layer.HeaderType, buildID: build.ID}
		}
		requiredControls[artifact.Snapfile(build.ID)] = controlRequirement{objectType: artifact.TypeSnapfile, buildID: build.ID}
		requiredControls[artifact.Metadata(build.ID)] = controlRequirement{objectType: artifact.TypeMetadata, buildID: build.ID}
	}
	for _, object := range objects {
		if err := validateLogicalKey(object.LogicalKey); err != nil {
			return err
		}
		if _, exists := seen[object.LogicalKey]; exists {
			return fmt.Errorf("duplicate object logical key %q", object.LogicalKey)
		}
		seen[object.LogicalKey] = struct{}{}
		if object.Size < 0 {
			return fmt.Errorf("object %q has negative size %d", object.LogicalKey, object.Size)
		}
		if !object.Required {
			return fmt.Errorf("bundle object %q must be required", object.LogicalKey)
		}
		if !objectKeyMatchesType(object.LogicalKey, object.Type) {
			return fmt.Errorf("object %q does not match type %q", object.LogicalKey, object.Type)
		}
		expectedPath, err := ObjectBundlePath(object.SHA256)
		if err != nil {
			return fmt.Errorf("object %q: %w", object.LogicalKey, err)
		}
		if object.BundlePath != expectedPath {
			return fmt.Errorf("object %q has unexpected bundle path %q", object.LogicalKey, object.BundlePath)
		}
		if object.Type == artifact.TypePersistent || object.Type == artifact.TypeSDCard {
			if len(object.ReferencingBuildIDs) == 0 {
				return fmt.Errorf("disk object %q has no referencing builds", object.LogicalKey)
			}
			for _, id := range object.ReferencingBuildIDs {
				if layouts[id] != artifact.OSAndroid {
					return fmt.Errorf("Android disk object %q references non-Android build %q", object.LogicalKey, id)
				}
			}
		}
		refs := make(map[string]struct{}, len(object.ReferencingBuildIDs))
		for _, buildID := range object.ReferencingBuildIDs {
			if _, ok := builds[buildID]; !ok {
				return fmt.Errorf("object %q references missing build %q", object.LogicalKey, buildID)
			}
			if _, duplicate := refs[buildID]; duplicate {
				return fmt.Errorf("object %q references build %q more than once", object.LogicalKey, buildID)
			}
			refs[buildID] = struct{}{}
		}
		if control, ok := requiredControls[object.LogicalKey]; ok {
			if object.Type != control.objectType {
				return fmt.Errorf("control object %q has type %q, want %q", object.LogicalKey, object.Type, control.objectType)
			}
			if len(refs) != 1 {
				return fmt.Errorf("control object %q must reference only build %q", object.LogicalKey, control.buildID)
			}
			if _, ok := refs[control.buildID]; !ok {
				return fmt.Errorf("control object %q must reference build %q", object.LogicalKey, control.buildID)
			}
			delete(requiredControls, object.LogicalKey)
		} else if !artifact.IsDataType(object.Type) {
			return fmt.Errorf("unexpected control object %q", object.LogicalKey)
		}
	}
	if len(requiredControls) > 0 {
		keys := make([]string, 0, len(requiredControls))
		for key := range requiredControls {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return fmt.Errorf("bundle is missing required control object %q", keys[0])
	}
	return nil
}

func objectKeyMatchesType(key, objectType string) bool {
	filename := path.Base(key)
	parent := path.Dir(key)
	if parent == "." || parent == ".." || strings.ContainsRune(parent, '/') {
		return false
	}
	return artifact.MatchesFilename(filename, objectType)
}

func validateLogicalKey(key string) error {
	clean := path.Clean(key)
	if key == "" || key == "." || key == ".." ||
		strings.ContainsRune(key, '\\') || clean != key ||
		strings.HasPrefix(key, "/") || strings.HasPrefix(key, "../") {
		return fmt.Errorf("invalid object logical key %q", key)
	}
	return nil
}

func verifyObjectContents(root string, objects []ObjectRecord) error {
	// 对象已经在 Inspect 中完成元数据校验；这里故意只做昂贵的字节级检查。
	// 多个逻辑 key 可以指向同一个内容寻址文件，同一路径只扫描一次。
	verifiedPaths := make(map[string]int64, len(objects))
	for _, object := range objects {
		if size, ok := verifiedPaths[object.BundlePath]; ok {
			if size != object.Size {
				return fmt.Errorf("content-addressed object %q has inconsistent sizes", object.BundlePath)
			}
			continue
		}
		filename, err := safeBundlePath(root, object.BundlePath)
		if err != nil {
			return err
		}
		input, err := os.Open(filename)
		if err != nil {
			return fmt.Errorf("open bundle object %q: %w", object.LogicalKey, err)
		}
		hash := sha256.New()
		read, copyErr := io.Copy(hash, input)
		closeErr := input.Close()
		if copyErr != nil {
			return fmt.Errorf("read bundle object %q: %w", object.LogicalKey, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close bundle object %q: %w", object.LogicalKey, closeErr)
		}
		if read != object.Size {
			return fmt.Errorf("object %q size mismatch: got %d, want %d", object.LogicalKey, read, object.Size)
		}
		actual := hex.EncodeToString(hash.Sum(nil))
		if actual != object.SHA256 {
			return fmt.Errorf("object %q SHA-256 mismatch: got %s, want %s", object.LogicalKey, actual, object.SHA256)
		}
		verifiedPaths[object.BundlePath] = read
	}
	return nil
}

func verifyObjectClosure(root string, records Records, objects []ObjectRecord, layouts map[string]string) error {
	byKey := make(map[string]ObjectRecord, len(objects))
	for _, object := range objects {
		byKey[object.LogicalKey] = object
	}
	expectedRefs := make(map[string]map[string]struct{}, len(objects))
	addReference := func(key, buildID string) {
		if expectedRefs[key] == nil {
			expectedRefs[key] = make(map[string]struct{})
		}
		expectedRefs[key][buildID] = struct{}{}
	}

	for _, build := range records.Builds {
		for _, key := range []string{artifact.Snapfile(build.ID), artifact.Metadata(build.ID)} {
			addReference(key, build.ID)
		}
		if err := verifyMetadataLayout(root, byKey[artifact.Metadata(build.ID)], build.ID, layouts[build.ID]); err != nil {
			return err
		}
		for _, layer := range artifact.Layers(layouts[build.ID]) {
			key := layer.HeaderKey(build.ID)
			addReference(key, build.ID)
			object := byKey[key]
			filename, err := safeBundlePath(root, object.BundlePath)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(filename)
			if err != nil {
				return fmt.Errorf("read header %q: %w", key, err)
			}
			header, err := artifactheader.Parse(raw)
			if err != nil {
				return fmt.Errorf("parse header %q: %w", key, err)
			}
			if err := artifactheader.Validate(header); err != nil {
				return fmt.Errorf("validate header %q: %w", key, err)
			}
			if header.Metadata.BuildID != build.ID {
				return fmt.Errorf("header %q belongs to build %q, want %q", key, header.Metadata.BuildID, build.ID)
			}
			for _, mapping := range header.Mappings {
				if mapping.BuildID == artifactheader.NilUUID {
					continue
				}
				dataKey := layer.DataKey(mapping.BuildID)
				data, ok := byKey[dataKey]
				if !ok {
					return fmt.Errorf("header %q references missing object %q", key, dataKey)
				}
				end := mapping.BuildStorageOffset + mapping.Length
				if uint64(data.Size) < end {
					return fmt.Errorf("object %q is %d bytes, header %q requires at least %d", dataKey, data.Size, key, end)
				}
				addReference(dataKey, build.ID)
			}
		}
	}

	for _, object := range objects {
		expected := expectedRefs[object.LogicalKey]
		if len(expected) == 0 {
			return fmt.Errorf("object %q is outside the selected Build closure", object.LogicalKey)
		}
		if len(expected) != len(object.ReferencingBuildIDs) {
			return fmt.Errorf("object %q has inconsistent referencing builds", object.LogicalKey)
		}
		for _, buildID := range object.ReferencingBuildIDs {
			if _, ok := expected[buildID]; !ok {
				return fmt.Errorf("object %q unexpectedly references build %q", object.LogicalKey, buildID)
			}
		}
	}
	return nil
}

func verifyMetadataLayout(root string, object ObjectRecord, buildID, expectedOS string) error {
	filename, err := safeBundlePath(root, object.BundlePath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read metadata for build %q: %w", buildID, err)
	}
	actualOS, err := artifact.MetadataOS(raw, buildID)
	if err != nil {
		return err
	}
	if actualOS != expectedOS {
		return fmt.Errorf("metadata os_type %q disagrees with bundle layout %q for build %q; Android bundles exported by v0.1.x are incomplete: re-export with v0.2.0 or newer", actualOS, expectedOS, buildID)
	}
	return nil
}

func validateCounts(counts Counts) error {
	values := []struct {
		name  string
		value int
	}{
		{"templates", counts.Templates}, {"aliases", counts.Aliases}, {"builds", counts.Builds},
		{"assignments", counts.Assignments}, {"snapshot templates", counts.SnapshotTemplates}, {"objects", counts.Objects},
	}
	for _, value := range values {
		if value.value < 0 {
			return fmt.Errorf("%s count is negative: %d", value.name, value.value)
		}
	}
	if counts.ObjectBytes < 0 {
		return fmt.Errorf("object byte count is negative: %d", counts.ObjectBytes)
	}
	return nil
}

func verifyCounts(counts Counts, records Records, objects []ObjectRecord) error {
	checks := []struct {
		name      string
		got, want int
	}{
		{"templates", len(records.Templates), counts.Templates}, {"aliases", len(records.Aliases), counts.Aliases},
		{"builds", len(records.Builds), counts.Builds}, {"assignments", len(records.Assignments), counts.Assignments},
		{"snapshot templates", len(records.SnapshotTemplates), counts.SnapshotTemplates}, {"objects", len(objects), counts.Objects},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("%s count mismatch: got %d, want %d", check.name, check.got, check.want)
		}
	}
	var bytes int64
	for _, object := range objects {
		if object.Size < 0 || object.Size > counts.ObjectBytes-bytes {
			return fmt.Errorf("object byte count exceeds manifest total %d", counts.ObjectBytes)
		}
		bytes += object.Size
	}
	if bytes != counts.ObjectBytes {
		return fmt.Errorf("object byte count mismatch: got %d, want %d", bytes, counts.ObjectBytes)
	}
	return nil
}

func SortObjects(objects []ObjectRecord) {
	slices.SortFunc(objects, func(a, b ObjectRecord) int { return strings.Compare(a.LogicalKey, b.LogicalKey) })
}
