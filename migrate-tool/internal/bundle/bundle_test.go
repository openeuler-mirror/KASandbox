package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

func TestValidateDescriptorsRejectsInvalidV1Shape(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]RecordDescriptor) []RecordDescriptor
	}{
		{"unknown descriptor", func(values []RecordDescriptor) []RecordDescriptor {
			return append(values, RecordDescriptor{Name: "future", Path: "records/future.jsonl"})
		}},
		{"negative count", func(values []RecordDescriptor) []RecordDescriptor {
			values[0].Count = -1
			return values
		}},
		{"wrong canonical path", func(values []RecordDescriptor) []RecordDescriptor {
			values[0].Path = "records/other.jsonl"
			return values
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptors := canonicalDescriptors()
			if err := validateDescriptors(test.mutate(descriptors)); err == nil {
				t.Fatal("expected descriptor validation error")
			}
		})
	}
}

func TestVerifyRejectsNegativeRecordCountWithoutPanic(t *testing.T) {
	root := t.TempDir()
	descriptors, err := WriteRecords(root, Records{})
	if err != nil {
		t.Fatal(err)
	}
	descriptors[0].Count = -1
	manifest := Manifest{Format: Format, FormatVersion: FormatVersion, Records: descriptors}
	if err := WriteManifest(root, &manifest); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Verify panicked: %v", recovered)
		}
	}()
	if _, err := Verify(root); err == nil {
		t.Fatal("expected verification error")
	}
}

func TestManifestRejectsTrailingJSON(t *testing.T) {
	root := writeEmptyBundle(t)
	manifestPath := filepath.Join(root, "manifest.json")
	file, err := os.OpenFile(manifestPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(root); err == nil {
		t.Fatal("expected trailing JSON error")
	}
}

func TestRecordLineRejectsTrailingJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "records"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"template","source":"template"} {}` + "\n")
	filename := filepath.Join(root, "records", "templates.jsonl")
	if err := os.WriteFile(filename, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	descriptor := RecordDescriptor{
		Name: "templates", Path: "records/templates.jsonl", Count: 1,
		SHA256: hex.EncodeToString(digest[:]),
	}
	if _, err := readJSONLines[TemplateRecord](root, descriptor); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestInspectDoesNotReadObjectBytes(t *testing.T) {
	root := t.TempDir()
	descriptors, err := WriteRecords(root, Records{})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("original")
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	relative, err := ObjectBundlePath(digest)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
	objects := []ObjectRecord{{
		LogicalKey: "build/rootfs.ext4", Type: artifact.TypeRootfs,
		Size: int64(len(content)), SHA256: digest, BundlePath: relative, Required: true,
	}}
	manifest := Manifest{Format: Format, FormatVersion: FormatVersion, Records: descriptors, Objects: objects,
		Counts: Counts{Objects: 1, ObjectBytes: int64(len(content))}}
	if err := WriteManifest(root, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("corrupt!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(root); err != nil {
		t.Fatalf("Inspect() returned %v", err)
	}
	if _, err := Verify(root); err == nil {
		t.Fatal("Verify() accepted corrupt object")
	}
}

func TestVerifyRejectsBuildWithoutObjectClosure(t *testing.T) {
	root := t.TempDir()
	records := Records{
		Templates: []TemplateRecord{{ID: "template", Source: model.SourceTemplate}},
		Builds: []BuildRecord{{
			ID: "00112233-4455-6677-8899-aabbccddeeff", Status: "uploaded", StatusGroup: model.StatusGroupReady,
		}},
		Assignments: []AssignmentRecord{{
			SourceID: "assignment", TemplateID: "template",
			BuildID: "00112233-4455-6677-8899-aabbccddeeff", Tag: "default", Source: "app",
		}},
	}
	descriptors, err := WriteRecords(root, records)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Format: Format, FormatVersion: FormatVersion, Records: descriptors,
		Counts: Counts{Templates: 1, Builds: 1, Assignments: 1},
	}
	if err := WriteManifest(root, &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root); err == nil || !strings.Contains(err.Error(), "missing required control object") {
		t.Fatalf("Verify() error = %v, want missing object error", err)
	}
}

func TestValidateObjectsRejectsInvalidV1Metadata(t *testing.T) {
	digest := strings.Repeat("0", sha256.Size*2)
	path, err := ObjectBundlePath(digest)
	if err != nil {
		t.Fatal(err)
	}
	tests := []ObjectRecord{
		{LogicalKey: "build/rootfs.ext4", Size: 1, SHA256: digest, BundlePath: path},
		{LogicalKey: ".", Size: 1, SHA256: digest, BundlePath: path, Required: true},
		{LogicalKey: "build/memfile", Type: artifact.TypeRootfs, Size: 1, SHA256: digest, BundlePath: path, Required: true},
	}
	for _, object := range tests {
		if err := validateObjects([]ObjectRecord{object}, Records{}, nil); err == nil {
			t.Fatalf("validateObjects(%+v) succeeded, want error", object)
		}
	}
}

func TestValidateRecordsRejectsBrokenRelationships(t *testing.T) {
	teamNamespace := "team"
	tests := []struct {
		name    string
		records Records
	}{
		{
			name: "alias name occupied by two templates",
			records: Records{
				Templates: []TemplateRecord{{ID: "a", Source: model.SourceTemplate}, {ID: "b", Source: model.SourceTemplate}},
				Aliases: []AliasRecord{
					{SourceID: "alias-a", TemplateID: "a", Alias: "same", NamespaceKind: NamespaceTeam, SourceNamespace: &teamNamespace},
					{SourceID: "alias-b", TemplateID: "b", Alias: "same", NamespaceKind: NamespaceTeam, SourceNamespace: &teamNamespace},
				},
			},
		},
		{
			name:    "empty build id",
			records: Records{Builds: []BuildRecord{{StatusGroup: model.StatusGroupReady}}},
		},
		{
			name: "duplicate assignment id",
			records: Records{
				Templates: []TemplateRecord{{ID: "a", Source: model.SourceTemplate}},
				Builds:    []BuildRecord{{ID: "build", Status: "uploaded", StatusGroup: model.StatusGroupReady}},
				Assignments: []AssignmentRecord{
					{SourceID: "assignment", TemplateID: "a", BuildID: "build", Tag: "default", Source: "app"},
					{SourceID: "assignment", TemplateID: "a", BuildID: "build", Tag: "default", Source: "app"},
				},
			},
		},
		{
			name: "template without assignment",
			records: Records{
				Templates: []TemplateRecord{{ID: "a", Source: model.SourceTemplate}},
			},
		},
		{
			name: "snapshot row on ordinary template",
			records: Records{
				Templates:         []TemplateRecord{{ID: "a", Source: model.SourceTemplate}},
				SnapshotTemplates: []SnapshotTemplateRecord{{TemplateID: "a"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateRecords(test.records); err == nil {
				t.Fatal("expected record validation error")
			}
		})
	}
}

func canonicalDescriptors() []RecordDescriptor {
	result := make([]RecordDescriptor, 0, len(recordOrder))
	for _, name := range recordOrder {
		result = append(result, RecordDescriptor{Name: name, Path: "records/" + name + ".jsonl"})
	}
	return result
}

func writeEmptyBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	descriptors, err := WriteRecords(root, Records{})
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Format: Format, FormatVersion: FormatVersion, Records: descriptors}
	if err := WriteManifest(root, &manifest); err != nil {
		t.Fatal(err)
	}
	return root
}
