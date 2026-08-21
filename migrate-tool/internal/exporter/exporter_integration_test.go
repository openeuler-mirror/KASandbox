package exporter_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/exporter"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/importer"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/targetstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/testfixture"
)

func TestFixtureExportAndImport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	sourceCatalog, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	sourceStore, _ := objectstore.NewFileStore(filepath.Join(root, "source", "objects"))
	bundlePath := filepath.Join(root, "bundle")
	result, err := (&exporter.Exporter{Catalog: sourceCatalog, Store: sourceStore, ToolVersion: "test", Now: func() time.Time {
		return time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	}}).Export(ctx, selection.Options{All: true}, bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Counts.Builds != 2 || result.Manifest.Counts.Objects != 13 {
		t.Fatalf("counts = %#v", result.Manifest.Counts)
	}
	verified, err := bundle.Verify(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bundlePath, ".object-tmp")); !os.IsNotExist(err) {
		t.Fatalf("published bundle contains the staging directory: %v", err)
	}
	targetCatalog, _ := catalog.NewFile(filepath.Join(root, "target", "catalog.json"))
	targetStore, _ := objectstore.NewFileStore(filepath.Join(root, "target", "objects"))
	migrator := &importer.Importer{Catalog: targetCatalog, Target: targetstore.FromObjectStore(targetStore)}
	options := importer.Options{TargetTeam: "slug:runtime", LiteralNamespaceMap: map[string]string{"legacy": "archive"}, ConflictPolicy: importer.ConflictFail}
	dryRun, err := migrator.Run(ctx, verified, options, false)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Applied || dryRun.Plan.Create.Templates != 2 || dryRun.Plan.Skip.Aliases != 1 || len(dryRun.Plan.Conflicts) != 0 {
		t.Fatalf("dry-run = %#v", dryRun)
	}
	applied, err := migrator.Run(ctx, verified, options, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied {
		t.Fatal("import did not apply")
	}

	target, err := targetCatalog.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Templates) != 2 || len(target.Builds) != 2 || len(target.Aliases) != 3 {
		t.Fatalf("target counts: templates=%d builds=%d aliases=%d", len(target.Templates), len(target.Builds), len(target.Aliases))
	}
	for _, template := range target.Templates {
		if template.TeamID != testfixture.TargetTeamID || template.ClusterID == nil || *template.ClusterID != testfixture.TargetClusterID {
			t.Fatalf("template was not rebound: %#v", template)
		}
	}
	for _, build := range target.Builds {
		if build.TeamID != testfixture.TargetTeamID || build.ClusterNodeID != nil {
			t.Fatalf("build was not rebound: %#v", build)
		}
	}

	options.ConflictPolicy = importer.ConflictSkipIdentical
	repeated, err := migrator.Run(ctx, verified, options, false)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Plan.Reuse.Objects != 13 || repeated.Plan.Reuse.Templates != 2 || len(repeated.Plan.Conflicts) != 0 {
		t.Fatalf("repeated plan = %#v", repeated.Plan)
	}
	reapplied, err := migrator.Run(ctx, verified, options, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reapplied.Applied || reapplied.Plan.Reuse.Objects != 13 {
		t.Fatalf("reapplied plan = %#v", reapplied)
	}

	target.Assignments[0].Source = "different-source"
	if err := targetCatalog.Commit(ctx, target); err != nil {
		t.Fatal(err)
	}
	differentAssignment, err := migrator.Run(ctx, verified, options, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(differentAssignment.Plan.Conflicts) == 0 || differentAssignment.Plan.Conflicts[0].Kind != "assignment" {
		t.Fatalf("assignment source difference was reused: %#v", differentAssignment.Plan)
	}

	missingClosure := verified.Manifest
	missingClosure.Objects = append([]bundle.ObjectRecord(nil), verified.Manifest.Objects...)
	for index, object := range missingClosure.Objects {
		if object.Type == artifact.TypeMemfile || object.Type == artifact.TypeRootfs {
			missingClosure.Objects = append(missingClosure.Objects[:index], missingClosure.Objects[index+1:]...)
			missingClosure.Counts.Objects--
			missingClosure.Counts.ObjectBytes -= object.Size
			break
		}
	}
	if err := bundle.WriteManifest(bundlePath, &missingClosure); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Verify(bundlePath); err == nil {
		t.Fatal("expected missing Header dependency to fail verification")
	}
	if err := bundle.WriteManifest(bundlePath, &verified.Manifest); err != nil {
		t.Fatal(err)
	}

	objectPath := filepath.Join(bundlePath, filepath.FromSlash(verified.Manifest.Objects[0].BundlePath))
	file, err := os.OpenFile(objectPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Verify(bundlePath); err == nil {
		t.Fatal("expected corrupt bundle verification to fail")
	}
}
