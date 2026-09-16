package exporter_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/exporter"
	artifactheader "gitcode.com/openeuler/KASandbox/migrate-tool/internal/header"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/importer"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/targetstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/testfixture"
)

func androidFixture(t *testing.T) (string, *exporter.Exporter) {
	t.Helper()
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	if err := testfixture.AddAndroid(root, testfixture.BuildID); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := objectstore.NewFileStore(filepath.Join(root, "source", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	return root, &exporter.Exporter{Catalog: cat, Store: store, ToolVersion: "test"}
}

func TestAndroidExportAndImport(t *testing.T) {
	root, exp := androidFixture(t)
	out := filepath.Join(root, "bundle")
	result, err := exp.Export(context.Background(), selection.Options{All: true}, out)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, object := range result.Manifest.Objects {
		keys[object.LogicalKey] = true
	}
	for _, key := range []string{
		testfixture.BuildID + "/persistent.img.header", testfixture.BuildID + "/sdcard.img.header",
		testfixture.BuildID + "/persistent.img", testfixture.AncestorBuildID + "/persistent.img",
		testfixture.AncestorBuildID + "/sdcard.img",
	} {
		if !keys[key] {
			t.Errorf("Android export omitted required object %s", key)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if result.Manifest.FormatVersion != 2 {
		t.Fatalf("Android bundle version = %d, want 2", result.Manifest.FormatVersion)
	}
	verified, err := bundle.Verify(out)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.NewFile(filepath.Join(root, "target", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := objectstore.NewFileStore(filepath.Join(root, "target", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	imp := &importer.Importer{Catalog: cat, Target: targetstore.FromObjectStore(store)}
	opts := importer.Options{TargetTeam: "slug:runtime", LiteralNamespaceMap: map[string]string{"legacy": "archive"}}
	plan, err := imp.Run(context.Background(), verified, opts, false)
	if err != nil || plan.Applied || len(plan.Plan.Conflicts) != 0 {
		t.Fatalf("dry-run: %v %v", plan, err)
	}
	applied, err := imp.Run(context.Background(), verified, opts, true)
	if err != nil || !applied.Applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	for _, object := range verified.Manifest.Objects {
		src, err := os.ReadFile(filepath.Join(root, "source", "objects", filepath.FromSlash(object.LogicalKey)))
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.ReadFile(filepath.Join(root, "target", "objects", filepath.FromSlash(object.LogicalKey)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, dst) {
			t.Fatalf("import changed object %s", object.LogicalKey)
		}
	}
	// Reconstruct logical disk bytes from the imported headers, including an
	// ancestor offset and a zero-filled extent, as the runtime reader does.
	for name, expected := range map[string][]byte{
		"persistent.img": []byte("ed!!PNEW"),
		"sdcard.img":     append([]byte("SDAT"), make([]byte, 4)...),
	} {
		base := filepath.Join(root, "target", "objects")
		raw, err := os.ReadFile(filepath.Join(base, testfixture.BuildID, name+".header"))
		if err != nil {
			t.Fatal(err)
		}
		header, err := artifactheader.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		restored := make([]byte, header.Metadata.Size)
		for _, mapping := range header.Mappings {
			if mapping.BuildID == artifactheader.NilUUID {
				continue
			}
			data, err := os.ReadFile(filepath.Join(base, mapping.BuildID, name))
			if err != nil {
				t.Fatal(err)
			}
			copy(restored[mapping.Offset:mapping.Offset+mapping.Length], data[mapping.BuildStorageOffset:mapping.BuildStorageOffset+mapping.Length])
		}
		if !bytes.Equal(restored, expected) {
			t.Fatalf("restored %s = %q, want %q", name, restored, expected)
		}
	}
}

func TestAndroidExportRejectsMissingDiskDependency(t *testing.T) {
	for _, key := range []string{
		testfixture.BuildID + "/persistent.img.header", testfixture.BuildID + "/sdcard.img.header",
		testfixture.AncestorBuildID + "/persistent.img", testfixture.AncestorBuildID + "/sdcard.img",
	} {
		t.Run(key, func(t *testing.T) {
			root, exp := androidFixture(t)
			if err := os.Remove(filepath.Join(root, "source", "objects", filepath.FromSlash(key))); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(root, "bundle")
			_, err := exp.Export(context.Background(), selection.Options{All: true}, out)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing %s: got %v", key, err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("incomplete bundle published: %v", err)
			}
		})
	}
}

func TestAndroidBundleRejectsIncompleteOrMismatchedLayout(t *testing.T) {
	for _, scenario := range []string{"missing-header", "missing-data", "truncated-data", "legacy-android", "metadata-mismatch", "unknown-os"} {
		t.Run(scenario, func(t *testing.T) {
			root, exp := androidFixture(t)
			out := filepath.Join(root, "bundle")
			result, err := exp.Export(context.Background(), selection.Options{All: true}, out)
			if err != nil {
				t.Fatal(err)
			}
			manifest := result.Manifest
			switch scenario {
			case "missing-header":
				removeObjects(&manifest, func(o bundle.ObjectRecord) bool { return o.LogicalKey == testfixture.BuildID+"/sdcard.img.header" })
			case "missing-data":
				removeObjects(&manifest, func(o bundle.ObjectRecord) bool { return o.LogicalKey == testfixture.AncestorBuildID+"/persistent.img" })
			case "legacy-android":
				manifest.FormatVersion = 1
				manifest.BuildLayouts = nil
				removeObjects(&manifest, func(o bundle.ObjectRecord) bool {
					return strings.Contains(o.LogicalKey, "persistent.img") || strings.Contains(o.LogicalKey, "sdcard.img")
				})
			case "metadata-mismatch", "unknown-os":
				osType := "linux"
				if scenario == "unknown-os" {
					osType = "future-os"
				}
				for i, obj := range manifest.Objects {
					if obj.LogicalKey != testfixture.BuildID+"/metadata.json" {
						continue
					}
					raw, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(obj.BundlePath)))
					if err != nil {
						t.Fatal(err)
					}
					var meta map[string]any
					if err := json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}
					meta["template"].(map[string]any)["os_type"] = osType
					raw, err = json.Marshal(meta)
					if err != nil {
						t.Fatal(err)
					}
					replaceBundleObject(t, out, &manifest, i, raw)
				}
			case "truncated-data":
				for i, obj := range manifest.Objects {
					if obj.LogicalKey == testfixture.AncestorBuildID+"/persistent.img" {
						replaceBundleObject(t, out, &manifest, i, []byte("short"))
					}
				}
			}
			if err := bundle.WriteManifest(out, &manifest); err != nil {
				t.Fatal(err)
			}
			if _, err := bundle.Verify(out); err == nil {
				t.Fatal("invalid Android bundle accepted")
			} else if scenario == "legacy-android" && !strings.Contains(err.Error(), "re-export") {
				t.Fatalf("expected actionable re-export error: %v", err)
			}
		})
	}
}

func removeObjects(m *bundle.Manifest, remove func(bundle.ObjectRecord) bool) {
	objects := make([]bundle.ObjectRecord, 0, len(m.Objects))
	for _, obj := range m.Objects {
		if remove(obj) {
			m.Counts.Objects--
			m.Counts.ObjectBytes -= obj.Size
		} else {
			objects = append(objects, obj)
		}
	}
	m.Objects = objects
}

// Recalculate every checksum to test semantic validation, not a hash mismatch.
func replaceBundleObject(t *testing.T, root string, m *bundle.Manifest, index int, raw []byte) {
	t.Helper()
	obj := &m.Objects[index]
	m.Counts.ObjectBytes += int64(len(raw)) - obj.Size
	obj.Size = int64(len(raw))
	digest := sha256.Sum256(raw)
	obj.SHA256 = hex.EncodeToString(digest[:])
	var err error
	obj.BundlePath, err = bundle.ObjectBundlePath(obj.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, filepath.FromSlash(obj.BundlePath))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
