package selection_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/testfixture"
)

func TestLatestFiltersReadyBeforeChoosingLatest(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, err := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := selection.Select(data, selection.Options{TemplateIDs: []string{testfixture.TemplateID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Builds) != 1 || result.Data.Builds[0].ID != testfixture.BuildID {
		t.Fatalf("selected builds = %#v", result.Data.Builds)
	}
}

func TestExactNonReadyBuildFails(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = selection.Select(data, selection.Options{TemplateIDs: []string{testfixture.TemplateID}, BuildIDs: []string{testfixture.PendingBuildID}})
	if err == nil {
		t.Fatal("expected non-ready build error")
	}
}

func TestEmptyTagValueDoesNotDisableDefaultTag(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := selection.Select(data, selection.Options{
		TemplateIDs: []string{testfixture.TemplateID},
		Tags:        []string{""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Builds) != 1 || result.Options.Tags[0] != "default" {
		t.Fatalf("selection = %#v", result)
	}
}

func TestAllowNoReadyBuildsKeepsTemplatesWithoutReadyBuilds(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 追加一个只有非 ready Build 的 Template,模拟运行中环境的常见状态。
	data.Templates = append(data.Templates, model.Template{
		ID: "tpl-stuck", TeamID: testfixture.SourceTeamID, Source: model.SourceTemplate,
	})
	data.Assignments = append(data.Assignments, model.BuildAssignment{
		ID: "bbbbbbbb-0000-4000-8000-000000000009", TemplateID: "tpl-stuck",
		BuildID: testfixture.PendingBuildID, Tag: model.DefaultTag, Source: "app",
	})

	if _, err := selection.Select(data, selection.Options{All: true}); err == nil ||
		!strings.Contains(err.Error(), "no ready build") {
		t.Fatalf("strict Select() error = %v, want no-ready-build error", err)
	}

	result, err := selection.Select(data, selection.Options{All: true, AllowNoReadyBuilds: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Templates) != 3 {
		t.Fatalf("templates = %d, want 3 including the one without ready builds", len(result.Data.Templates))
	}
	for _, build := range result.Data.Builds {
		if build.ID == testfixture.PendingBuildID {
			t.Fatal("non-ready build was selected")
		}
	}
}

func TestExactBuildRequiresMatchForEverySelectedTemplate(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = selection.Select(data, selection.Options{All: true, BuildIDs: []string{testfixture.BuildID}})
	if err == nil || !strings.Contains(err.Error(), "has none of the requested builds") {
		t.Fatalf("Select() error = %v, want unmatched-template error", err)
	}
}

func TestExplicitTemplateSelectorsMustAllMatch(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = selection.Select(data, selection.Options{
		TemplateIDs: []string{testfixture.TemplateID, "typing-error"},
	})
	if err == nil || !strings.Contains(err.Error(), "typing-error") {
		t.Fatalf("Select() error = %v, want unmatched selector error", err)
	}
}

func TestExplicitTagsMustAllMatchEachTemplate(t *testing.T) {
	root := t.TempDir()
	if err := testfixture.Create(root); err != nil {
		t.Fatal(err)
	}
	source, _ := catalog.NewFile(filepath.Join(root, "source", "catalog.json"))
	data, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = selection.Select(data, selection.Options{
		TemplateIDs: []string{testfixture.TemplateID},
		Tags:        []string{"default", "typing-error"},
	})
	if err == nil || !strings.Contains(err.Error(), `tag "typing-error"`) {
		t.Fatalf("Select() error = %v, want unmatched tag error", err)
	}
}
