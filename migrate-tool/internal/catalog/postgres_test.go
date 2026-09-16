package catalog

import (
	"strings"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

func TestValidatePostgresVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "minimum", value: "150000"},
		{name: "newer", value: "170004"},
		{name: "older", value: "149999", wantErr: "PostgreSQL 15 or newer"},
		{name: "invalid", value: "fifteen", wantErr: `got "fifteen"`},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validatePostgresVersion(test.value)
			assertErrorContains(t, err, test.wantErr)
		})
	}
}

func TestValidatePostgresMigration(t *testing.T) {
	t.Parallel()

	if err := validatePostgresMigration(MinimumPostgresSchema); err != nil {
		t.Fatalf("minimum schema was rejected: %v", err)
	}
	if err := validatePostgresMigration(MinimumPostgresSchema + 1); err != nil {
		t.Fatalf("newer schema was rejected: %v", err)
	}
	err := validatePostgresMigration(MinimumPostgresSchema - 1)
	assertErrorContains(t, err, "older than required baseline")
}

func TestRequiredPostgresTables(t *testing.T) {
	t.Parallel()

	want := []string{
		"env_aliases",
		"env_build_assignments",
		"env_builds",
		"envs",
		"snapshot_templates",
		"teams",
	}
	got := requiredPostgresTables()
	if len(got) != len(want) {
		t.Fatalf("table count = %d, want %d: %v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("table %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestValidatePostgresColumns(t *testing.T) {
	t.Parallel()

	t.Run("accepts current schema", func(t *testing.T) {
		t.Parallel()
		if err := validatePostgresColumns(clonePostgresColumns()); err != nil {
			t.Fatalf("current schema was rejected: %v", err)
		}
	})

	t.Run("rejects missing column", func(t *testing.T) {
		t.Parallel()
		columns := clonePostgresColumns()
		columns = columns[1:]
		assertErrorContains(t, validatePostgresColumns(columns), "missing required column public.env_aliases.alias")
	})

	t.Run("rejects incompatible type", func(t *testing.T) {
		t.Parallel()
		columns := clonePostgresColumns()
		columns[0].udtName = "varchar"
		assertErrorContains(t, validatePostgresColumns(columns), "has type varchar, expected text")
	})

	t.Run("rejects incompatible nullability", func(t *testing.T) {
		t.Parallel()
		columns := clonePostgresColumns()
		columns[0].nullable = true
		assertErrorContains(t, validatePostgresColumns(columns), "is NULL, expected NOT NULL")
	})
}

func TestValidatePostgresCapabilities(t *testing.T) {
	t.Parallel()

	complete := postgresCapabilities{
		aliasNamespaceUnique: true,
		assignmentBuildFK:    true,
		assignmentTemplateFK: true,
		aliasTemplateFK:      true,
		snapshotTemplateFK:   true,
		statusGroupTrigger:   true,
		rowSecurityBypassed:  true,
	}
	if err := validatePostgresCapabilities(complete); err != nil {
		t.Fatalf("complete capabilities were rejected: %v", err)
	}

	for _, test := range []struct {
		name    string
		missing string
		remove  func(*postgresCapabilities)
	}{
		{name: "alias uniqueness", missing: "NULLS NOT DISTINCT", remove: func(value *postgresCapabilities) { value.aliasNamespaceUnique = false }},
		{name: "assignment build fk", missing: "env_build_assignments.build_id", remove: func(value *postgresCapabilities) { value.assignmentBuildFK = false }},
		{name: "assignment template fk", missing: "env_build_assignments.env_id", remove: func(value *postgresCapabilities) { value.assignmentTemplateFK = false }},
		{name: "alias template fk", missing: "env_aliases.env_id", remove: func(value *postgresCapabilities) { value.aliasTemplateFK = false }},
		{name: "snapshot template fk", missing: "snapshot_templates.env_id", remove: func(value *postgresCapabilities) { value.snapshotTemplateFK = false }},
		{name: "status trigger", missing: "status_group trigger", remove: func(value *postgresCapabilities) { value.statusGroupTrigger = false }},
		{name: "row security", missing: "RLS bypass", remove: func(value *postgresCapabilities) { value.rowSecurityBypassed = false }},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			capabilities := complete
			test.remove(&capabilities)
			assertErrorContains(t, validatePostgresCapabilities(capabilities), test.missing)
		})
	}
}

func TestValidatePostgresBuildStatus(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		status string
		group  string
	}{
		{status: "waiting", group: "pending"},
		{status: "building", group: "in_progress"},
		{status: "uploaded", group: model.StatusGroupReady},
		{status: "failed", group: "failed"},
		{status: "cancelled", group: "failed"},
	} {
		test := test
		t.Run(test.status, func(t *testing.T) {
			t.Parallel()
			build := model.Build{ID: "build-1", Status: test.status, StatusGroup: test.group}
			if err := validateBuildStatusGroup(build); err != nil {
				t.Fatalf("valid status mapping was rejected: %v", err)
			}
			build.StatusGroup = "wrong"
			assertErrorContains(t, validateBuildStatusGroup(build), `got "wrong"`)
		})
	}
}

func TestEnsureAdditive(t *testing.T) {
	t.Parallel()

	current := &model.CatalogData{Teams: []model.Team{{ID: "team-1", Slug: "one"}}}
	desired := &model.CatalogData{Teams: []model.Team{{ID: "team-1", Slug: "one"}, {ID: "team-2", Slug: "two"}}}
	if err := ensureAdditive(current, desired); err != nil {
		t.Fatalf("additive desired catalog was rejected: %v", err)
	}

	changed := &model.CatalogData{Teams: []model.Team{{ID: "team-1", Slug: "changed"}}}
	assertErrorContains(t, ensureAdditive(current, changed), `target Team "team-1" changed`)

	unexpected := &model.CatalogData{}
	assertErrorContains(t, ensureAdditive(current, unexpected), `target Team "team-1" appeared after dry-run`)
}

func clonePostgresColumns() []postgresColumnSpec {
	return append([]postgresColumnSpec(nil), requiredPostgresColumns...)
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}
