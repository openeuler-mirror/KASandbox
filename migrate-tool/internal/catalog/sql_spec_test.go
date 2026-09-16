package catalog

import (
	"regexp"
	"strings"
	"testing"
)

// 这些用例把 internal/catalog/sql/*.sql 与唯一列规格表 requiredPostgresColumns
// 锁在一起:schema 变更时,.sql 与规格表必须同步修改,只改一处会在这里失败。

func specColumns(t *testing.T, table string) []string {
	t.Helper()
	var columns []string
	for _, column := range requiredPostgresColumns {
		if column.table == table {
			columns = append(columns, column.column)
		}
	}
	if len(columns) == 0 {
		t.Fatalf("requiredPostgresColumns has no columns for table %q", table)
	}
	return columns
}

// INSERT 的列清单必须与规格表逐列相等:规格里的列一个不能少,也不能写入
// 规格之外的列。
func TestInsertStatementsMatchColumnSpec(t *testing.T) {
	statements := map[string]string{
		"envs":                  sqlInsertTemplate,
		"env_builds":            sqlInsertBuild,
		"env_build_assignments": sqlInsertAssignment,
		"env_aliases":           sqlInsertAlias,
		"snapshot_templates":    sqlInsertSnapshotTemplate,
	}
	pattern := regexp.MustCompile(`INSERT INTO public\.(\w+)\s*\(([^)]+)\)`)
	for table, statement := range statements {
		match := pattern.FindStringSubmatch(statement)
		if match == nil {
			t.Errorf("insert statement for %q has no parsable column list", table)
			continue
		}
		if match[1] != table {
			t.Errorf("insert statement targets %q, want %q", match[1], table)
			continue
		}
		inserted := make(map[string]struct{})
		for _, column := range strings.Split(match[2], ",") {
			inserted[strings.TrimSpace(column)] = struct{}{}
		}
		expected := specColumns(t, table)
		for _, column := range expected {
			if _, ok := inserted[column]; !ok {
				t.Errorf("insert into %s is missing spec column %q", table, column)
			}
		}
		if len(inserted) != len(expected) {
			for column := range inserted {
				found := false
				for _, want := range expected {
					if column == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("insert into %s writes column %q that is not in requiredPostgresColumns", table, column)
				}
			}
		}
	}
}

// SELECT 必须覆盖对应表的全部规格列;preflight 核对过类型/可空性的列,
// 加载时一列都不能漏。
func TestSelectStatementsCoverColumnSpec(t *testing.T) {
	statements := map[string]string{
		"teams":                 sqlSelectTeams,
		"envs":                  sqlSelectTemplates,
		"env_aliases":           sqlSelectAliases,
		"env_builds":            sqlSelectBuilds,
		"env_build_assignments": sqlSelectAssignments,
		"snapshot_templates":    sqlSelectSnapshotTemplates,
	}
	for table, statement := range statements {
		for _, column := range specColumns(t, table) {
			pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(column) + `\b`)
			if !pattern.MatchString(statement) {
				t.Errorf("select for %s does not reference spec column %q", table, column)
			}
		}
	}
}
