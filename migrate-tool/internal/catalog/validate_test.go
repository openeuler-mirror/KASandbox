package catalog

import (
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

func TestValidateRejectsRelationshipInvariantViolations(t *testing.T) {
	team := model.Team{ID: "team", Slug: "team"}
	tests := []struct {
		name string
		data model.CatalogData
	}{
		{
			name: "empty alias id",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Aliases: []model.Alias{{TemplateID: "a", Alias: "name"}}},
		},
		{
			name: "duplicate alias id",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Aliases: []model.Alias{{ID: "same", TemplateID: "a", Alias: "one"}, {ID: "same", TemplateID: "a", Alias: "two"}}},
		},
		{
			name: "duplicate alias name across templates",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{
				{ID: "a", TeamID: team.ID, Source: model.SourceTemplate},
				{ID: "b", TeamID: team.ID, Source: model.SourceTemplate},
			}, Aliases: []model.Alias{
				{ID: "one", TemplateID: "a", Namespace: stringPointer("namespace"), Alias: "name"},
				{ID: "two", TemplateID: "b", Namespace: stringPointer("namespace"), Alias: "name"},
			}},
		},
		{
			name: "build references missing team",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Builds: []model.Build{{ID: "build", TeamID: "ghost", Status: "uploaded", StatusGroup: model.StatusGroupReady}}},
		},
		{
			name: "empty assignment id",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Builds:      []model.Build{{ID: "build", TeamID: team.ID, Status: "uploaded", StatusGroup: model.StatusGroupReady}},
				Assignments: []model.BuildAssignment{{TemplateID: "a", BuildID: "build", Tag: "default", Source: "app"}}},
		},
		{
			name: "duplicate assignment id",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Builds: []model.Build{{ID: "build", TeamID: team.ID, Status: "uploaded", StatusGroup: model.StatusGroupReady}},
				Assignments: []model.BuildAssignment{
					{ID: "same", TemplateID: "a", BuildID: "build", Tag: "default", Source: "app"},
					{ID: "same", TemplateID: "a", BuildID: "build", Tag: "next", Source: "app"},
				}},
		},
		{
			name: "empty assignment source",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				Builds:      []model.Build{{ID: "build", TeamID: team.ID, Status: "uploaded", StatusGroup: model.StatusGroupReady}},
				Assignments: []model.BuildAssignment{{ID: "assignment", TemplateID: "a", BuildID: "build", Tag: "default"}}},
		},
		{
			name: "snapshot row on ordinary template",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
				SnapshotTemplates: []model.SnapshotTemplate{{TemplateID: "a"}}},
		},
		{
			name: "duplicate snapshot row",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceSnapshotTemplate}},
				SnapshotTemplates: []model.SnapshotTemplate{{TemplateID: "a", SandboxID: "one"}, {TemplateID: "a", SandboxID: "two"}}},
		},
		{
			name: "snapshot template without snapshot row",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{
				{ID: "a", TeamID: team.ID, Source: model.SourceSnapshotTemplate},
			}},
		},
		{
			name: "empty snapshot sandbox id",
			data: model.CatalogData{Teams: []model.Team{team}, Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceSnapshotTemplate}},
				SnapshotTemplates: []model.SnapshotTemplate{{TemplateID: "a"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(&test.data); err == nil {
				t.Fatal("expected catalog validation error")
			}
		})
	}
}

func TestValidateAllowsLegacyBuildWithoutTeam(t *testing.T) {
	team := model.Team{ID: "team", Slug: "team"}
	data := model.CatalogData{
		Teams:     []model.Team{team},
		Templates: []model.Template{{ID: "a", TeamID: team.ID, Source: model.SourceTemplate}},
		Builds:    []model.Build{{ID: "build", Status: "uploaded", StatusGroup: model.StatusGroupReady}},
		Assignments: []model.BuildAssignment{
			{ID: "assignment", TemplateID: "a", BuildID: "build", Tag: "default", Source: "app"},
		},
	}
	if err := Validate(&data); err != nil {
		t.Fatalf("legacy NULL-team build was rejected: %v", err)
	}
}

func stringPointer(value string) *string {
	return &value
}
