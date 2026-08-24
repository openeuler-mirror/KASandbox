package catalog

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

const MinimumPostgresSchema = int64(20260218120000)

// 全部 SQL 语句以原样 .sql 文件维护在 internal/catalog/sql/ 下,Go 侧只做
// 参数绑定与行扫描。列规格的唯一权威是 requiredPostgresColumns;
// sql_spec_test.go 强制各 .sql 的列清单与规格表一致,schema 变更时同步修改
// .sql 与规格表即可,任何漂移都会让单测显式失败。
var (
	//go:embed sql/preflight_server_version.sql
	sqlPreflightServerVersion string
	//go:embed sql/preflight_migration.sql
	sqlPreflightMigration string
	//go:embed sql/preflight_columns.sql
	sqlPreflightColumns string
	//go:embed sql/preflight_capabilities.sql
	sqlPreflightCapabilities string
	//go:embed sql/select_teams.sql
	sqlSelectTeams string
	//go:embed sql/select_templates.sql
	sqlSelectTemplates string
	//go:embed sql/select_aliases.sql
	sqlSelectAliases string
	//go:embed sql/select_builds.sql
	sqlSelectBuilds string
	//go:embed sql/select_assignments.sql
	sqlSelectAssignments string
	//go:embed sql/select_snapshot_templates.sql
	sqlSelectSnapshotTemplates string
	//go:embed sql/insert_template.sql
	sqlInsertTemplate string
	//go:embed sql/insert_build.sql
	sqlInsertBuild string
	//go:embed sql/insert_assignment.sql
	sqlInsertAssignment string
	//go:embed sql/insert_alias.sql
	sqlInsertAlias string
	//go:embed sql/insert_snapshot_template.sql
	sqlInsertSnapshotTemplate string
)

type postgresColumnSpec struct {
	table    string
	column   string
	udtName  string
	nullable bool
}

var requiredPostgresColumns = []postgresColumnSpec{
	{table: "env_aliases", column: "alias", udtName: "text"},
	{table: "env_aliases", column: "env_id", udtName: "text"},
	{table: "env_aliases", column: "id", udtName: "uuid"},
	{table: "env_aliases", column: "is_renamable", udtName: "bool"},
	{table: "env_aliases", column: "namespace", udtName: "text", nullable: true},
	{table: "env_build_assignments", column: "build_id", udtName: "uuid"},
	{table: "env_build_assignments", column: "created_at", udtName: "timestamptz", nullable: true},
	{table: "env_build_assignments", column: "env_id", udtName: "text"},
	{table: "env_build_assignments", column: "id", udtName: "uuid"},
	{table: "env_build_assignments", column: "source", udtName: "text"},
	{table: "env_build_assignments", column: "tag", udtName: "text"},
	{table: "env_builds", column: "cluster_node_id", udtName: "text", nullable: true},
	{table: "env_builds", column: "cpu_architecture", udtName: "text", nullable: true},
	{table: "env_builds", column: "cpu_family", udtName: "text", nullable: true},
	{table: "env_builds", column: "cpu_flags", udtName: "_text", nullable: true},
	{table: "env_builds", column: "cpu_model", udtName: "text", nullable: true},
	{table: "env_builds", column: "cpu_model_name", udtName: "text", nullable: true},
	{table: "env_builds", column: "created_at", udtName: "timestamptz"},
	{table: "env_builds", column: "dockerfile", udtName: "text", nullable: true},
	{table: "env_builds", column: "env_id", udtName: "text", nullable: true},
	{table: "env_builds", column: "envd_version", udtName: "text", nullable: true},
	{table: "env_builds", column: "finished_at", udtName: "timestamptz", nullable: true},
	{table: "env_builds", column: "firecracker_version", udtName: "text"},
	{table: "env_builds", column: "free_disk_size_mb", udtName: "int8"},
	{table: "env_builds", column: "id", udtName: "uuid"},
	{table: "env_builds", column: "kernel_version", udtName: "text"},
	{table: "env_builds", column: "ram_mb", udtName: "int8"},
	{table: "env_builds", column: "ready_cmd", udtName: "text", nullable: true},
	{table: "env_builds", column: "reason", udtName: "jsonb"},
	{table: "env_builds", column: "start_cmd", udtName: "text", nullable: true},
	{table: "env_builds", column: "status", udtName: "text"},
	{table: "env_builds", column: "status_group", udtName: "text"},
	{table: "env_builds", column: "team_id", udtName: "uuid", nullable: true},
	{table: "env_builds", column: "total_disk_size_mb", udtName: "int8", nullable: true},
	{table: "env_builds", column: "updated_at", udtName: "timestamptz"},
	{table: "env_builds", column: "vcpu", udtName: "int8"},
	{table: "env_builds", column: "version", udtName: "text", nullable: true},
	{table: "envs", column: "build_count", udtName: "int4"},
	{table: "envs", column: "cluster_id", udtName: "uuid", nullable: true},
	{table: "envs", column: "created_at", udtName: "timestamptz"},
	{table: "envs", column: "created_by", udtName: "uuid", nullable: true},
	{table: "envs", column: "id", udtName: "text"},
	{table: "envs", column: "last_spawned_at", udtName: "timestamptz", nullable: true},
	{table: "envs", column: "public", udtName: "bool"},
	{table: "envs", column: "source", udtName: "text"},
	{table: "envs", column: "spawn_count", udtName: "int8"},
	{table: "envs", column: "team_id", udtName: "uuid"},
	{table: "envs", column: "updated_at", udtName: "timestamptz"},
	{table: "snapshot_templates", column: "created_at", udtName: "timestamptz", nullable: true},
	{table: "snapshot_templates", column: "env_id", udtName: "text"},
	{table: "snapshot_templates", column: "sandbox_id", udtName: "text"},
	{table: "teams", column: "cluster_id", udtName: "uuid", nullable: true},
	{table: "teams", column: "id", udtName: "uuid"},
	{table: "teams", column: "name", udtName: "text"},
	{table: "teams", column: "slug", udtName: "text"},
}

type postgresCapabilities struct {
	aliasNamespaceUnique bool
	assignmentBuildFK    bool
	assignmentTemplateFK bool
	aliasTemplateFK      bool
	snapshotTemplateFK   bool
	statusGroupTrigger   bool
	rowSecurityBypassed  bool
}

type Postgres struct {
	dsn string
}

func NewPostgres(dsn string) (*Postgres, error) {
	if dsn == "" {
		return nil, fmt.Errorf("PostgreSQL DSN is required")
	}
	return &Postgres{dsn: dsn}, nil
}

func (p *Postgres) Kind() string { return "postgres" }

func (p *Postgres) Snapshot(ctx context.Context) (*model.CatalogData, error) {
	conn, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())
	// Selection 需要看到同一时刻的 Template、Alias、Assignment 和 Build。这里只在
	// 读取 Catalog 时持有短事务，大对象复制不会长期占用数据库快照。
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin PostgreSQL snapshot: %w", err)
	}
	defer tx.Rollback(context.Background())
	version, err := postgresPreflight(ctx, tx)
	if err != nil {
		return nil, err
	}
	data, err := loadPostgres(ctx, tx, version)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit PostgreSQL snapshot: %w", err)
	}
	return data, nil
}

func (p *Postgres) Commit(ctx context.Context, desired *model.CatalogData) error {
	if err := Validate(desired); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())
	// 导入计划基于稍早的 Snapshot。Serializable 事务内重新读取并比较目标，
	// 防止 dry-run 与真正写入之间出现静默覆盖。
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL import transaction: %w", err)
	}
	defer tx.Rollback(context.Background())
	version, err := postgresPreflight(ctx, tx)
	if err != nil {
		return err
	}
	current, err := loadPostgres(ctx, tx, version)
	if err != nil {
		return err
	}
	if err := ensureAdditive(current, desired); err != nil {
		return fmt.Errorf("target changed after dry-run: %w", err)
	}
	if err := insertMissing(ctx, tx, current, desired); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL import: %w", err)
	}
	return nil
}

type pgQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func postgresPreflight(ctx context.Context, query pgQuery) (string, error) {
	// 这里检查的是工具真正依赖的数据库能力，而不只是一串 schema 版本号。
	// 生产部署可能包含额外迁移，但缺列、缺约束或 RLS 未旁路都不能继续。
	var serverVersionText string
	if err := query.QueryRow(ctx, sqlPreflightServerVersion).Scan(&serverVersionText); err != nil {
		return "", fmt.Errorf("read PostgreSQL version: %w", err)
	}
	if err := validatePostgresVersion(serverVersionText); err != nil {
		return "", err
	}

	var migration int64
	if err := query.QueryRow(ctx, sqlPreflightMigration).Scan(&migration); err != nil {
		return "", fmt.Errorf("read public._migrations: %w", err)
	}
	if err := validatePostgresMigration(migration); err != nil {
		return "", err
	}

	tables := requiredPostgresTables()
	rows, err := query.Query(ctx, sqlPreflightColumns, tables)
	if err != nil {
		return "", fmt.Errorf("inspect PostgreSQL columns: %w", err)
	}
	defer rows.Close()
	found := make([]postgresColumnSpec, 0, len(requiredPostgresColumns))
	for rows.Next() {
		var column postgresColumnSpec
		var nullable string
		if err := rows.Scan(&column.table, &column.column, &column.udtName, &nullable); err != nil {
			return "", fmt.Errorf("scan PostgreSQL column capability: %w", err)
		}
		column.nullable = nullable == "YES"
		found = append(found, column)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read PostgreSQL column capabilities: %w", err)
	}
	if err := validatePostgresColumns(found); err != nil {
		return "", err
	}

	var capabilities postgresCapabilities
	err = query.QueryRow(ctx, sqlPreflightCapabilities).Scan(
		&capabilities.aliasNamespaceUnique,
		&capabilities.assignmentBuildFK,
		&capabilities.assignmentTemplateFK,
		&capabilities.aliasTemplateFK,
		&capabilities.snapshotTemplateFK,
		&capabilities.statusGroupTrigger,
		&capabilities.rowSecurityBypassed,
	)
	if err != nil {
		return "", fmt.Errorf("inspect PostgreSQL constraints and triggers: %w", err)
	}
	if err := validatePostgresCapabilities(capabilities); err != nil {
		return "", err
	}

	return strconv.FormatInt(migration, 10), nil
}

func validatePostgresVersion(value string) error {
	version, err := strconv.Atoi(value)
	if err != nil || version < 150000 {
		return fmt.Errorf("PostgreSQL 15 or newer is required, got %q", value)
	}
	return nil
}

func validatePostgresMigration(version int64) error {
	if version < MinimumPostgresSchema {
		return fmt.Errorf("database schema %d is older than required baseline %d", version, MinimumPostgresSchema)
	}
	return nil
}

func requiredPostgresTables() []string {
	tables := make([]string, 0, 6)
	last := ""
	for _, column := range requiredPostgresColumns {
		if column.table == last {
			continue
		}
		tables = append(tables, column.table)
		last = column.table
	}
	return tables
}

func validatePostgresColumns(found []postgresColumnSpec) error {
	byName := make(map[string]postgresColumnSpec, len(found))
	for _, column := range found {
		byName[column.table+"."+column.column] = column
	}
	for _, required := range requiredPostgresColumns {
		name := required.table + "." + required.column
		actual, ok := byName[name]
		if !ok {
			return fmt.Errorf("database is missing required column public.%s", name)
		}
		if actual.udtName != required.udtName {
			return fmt.Errorf("database column public.%s has type %s, expected %s", name, actual.udtName, required.udtName)
		}
		if actual.nullable != required.nullable {
			actualValue := "NOT NULL"
			if actual.nullable {
				actualValue = "NULL"
			}
			expectedValue := "NOT NULL"
			if required.nullable {
				expectedValue = "NULL"
			}
			return fmt.Errorf("database column public.%s is %s, expected %s", name, actualValue, expectedValue)
		}
	}
	return nil
}

func validatePostgresCapabilities(capabilities postgresCapabilities) error {
	checks := []struct {
		available bool
		name      string
	}{
		{capabilities.aliasNamespaceUnique, "unique (alias, namespace) NULLS NOT DISTINCT index"},
		{capabilities.assignmentBuildFK, "env_build_assignments.build_id foreign key"},
		{capabilities.assignmentTemplateFK, "env_build_assignments.env_id foreign key"},
		{capabilities.aliasTemplateFK, "env_aliases.env_id foreign key"},
		{capabilities.snapshotTemplateFK, "snapshot_templates.env_id foreign key"},
		{capabilities.statusGroupTrigger, "enabled env_builds status_group trigger"},
		{capabilities.rowSecurityBypassed, "RLS bypass for template catalog tables"},
	}
	for _, check := range checks {
		if !check.available {
			return fmt.Errorf("database is missing required capability: %s", check.name)
		}
	}
	return nil
}

func loadPostgres(ctx context.Context, query pgQuery, version string) (*model.CatalogData, error) {
	data := &model.CatalogData{SchemaVersion: version}

	rows, err := query.Query(ctx, sqlSelectTeams)
	if err != nil {
		return nil, fmt.Errorf("load teams: %w", err)
	}
	for rows.Next() {
		var value model.Team
		if err := rows.Scan(&value.ID, &value.Slug, &value.Name, &value.ClusterID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan team: %w", err)
		}
		data.Teams = append(data.Teams, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load teams: %w", err)
	}

	rows, err = query.Query(ctx, sqlSelectTemplates)
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	for rows.Next() {
		var value model.Template
		if err := rows.Scan(
			&value.ID,
			&value.CreatedAt,
			&value.UpdatedAt,
			&value.Public,
			&value.BuildCount,
			&value.SpawnCount,
			&value.LastSpawnedAt,
			&value.TeamID,
			&value.CreatedBy,
			&value.ClusterID,
			&value.Source,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan template: %w", err)
		}
		data.Templates = append(data.Templates, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}

	rows, err = query.Query(ctx, sqlSelectAliases)
	if err != nil {
		return nil, fmt.Errorf("load aliases: %w", err)
	}
	for rows.Next() {
		var value model.Alias
		if err := rows.Scan(&value.ID, &value.TemplateID, &value.Namespace, &value.Alias, &value.IsRenamable); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		data.Aliases = append(data.Aliases, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load aliases: %w", err)
	}

	rows, err = query.Query(ctx, sqlSelectBuilds)
	if err != nil {
		return nil, fmt.Errorf("load builds: %w", err)
	}
	for rows.Next() {
		var value model.Build
		var reason string
		if err := rows.Scan(
			&value.ID,
			&value.CreatedAt,
			&value.UpdatedAt,
			&value.FinishedAt,
			&value.Status,
			&value.StatusGroup,
			&value.Dockerfile,
			&value.StartCommand,
			&value.ReadyCommand,
			&value.VCPU,
			&value.RAMMB,
			&value.FreeDiskSizeMB,
			&value.TotalDiskSizeMB,
			&value.KernelVersion,
			&value.FirecrackerVersion,
			&value.LegacyTemplateID,
			&value.EnvdVersion,
			&value.ClusterNodeID,
			&reason,
			&value.Version,
			&value.CPUArchitecture,
			&value.CPUFamily,
			&value.CPUModel,
			&value.CPUModelName,
			&value.CPUFlags,
			&value.TeamID,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan build: %w", err)
		}
		value.Reason = json.RawMessage(reason)
		if err := validateBuildStatusGroup(value); err != nil {
			rows.Close()
			return nil, err
		}
		data.Builds = append(data.Builds, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load builds: %w", err)
	}

	rows, err = query.Query(ctx, sqlSelectAssignments)
	if err != nil {
		return nil, fmt.Errorf("load assignments: %w", err)
	}
	for rows.Next() {
		var value model.BuildAssignment
		if err := rows.Scan(&value.ID, &value.TemplateID, &value.BuildID, &value.Tag, &value.Source, &value.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan assignment: %w", err)
		}
		data.Assignments = append(data.Assignments, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load assignments: %w", err)
	}

	rows, err = query.Query(ctx, sqlSelectSnapshotTemplates)
	if err != nil {
		return nil, fmt.Errorf("load snapshot templates: %w", err)
	}
	for rows.Next() {
		var value model.SnapshotTemplate
		if err := rows.Scan(&value.TemplateID, &value.SandboxID, &value.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan snapshot template: %w", err)
		}
		data.SnapshotTemplates = append(data.SnapshotTemplates, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load snapshot templates: %w", err)
	}
	if err := Validate(data); err != nil {
		return nil, fmt.Errorf("validate PostgreSQL catalog: %w", err)
	}
	return data, nil
}

func ensureAdditive(current, desired *model.CatalogData) error {
	// desired 是 dry-run 所见 Catalog 的副本再加上本次迁移记录。提交事务中
	// 重新读取 current，要求其中每一行仍存在于 desired 且内容未变；因此并发
	// 新增或更新会失败，而本次计划中的新增仍可继续插入。
	if err := ensureRows("Team", current.Teams, desired.Teams, func(v model.Team) string { return v.ID }); err != nil {
		return err
	}
	if err := ensureRows("Template", current.Templates, desired.Templates, func(v model.Template) string { return v.ID }); err != nil {
		return err
	}
	if err := ensureRows("Alias", current.Aliases, desired.Aliases, func(v model.Alias) string { return v.ID }); err != nil {
		return err
	}
	if err := ensureRows("Build", current.Builds, desired.Builds, func(v model.Build) string { return v.ID }); err != nil {
		return err
	}
	if err := ensureRows("Assignment", current.Assignments, desired.Assignments, func(v model.BuildAssignment) string { return v.ID }); err != nil {
		return err
	}
	if err := ensureRows(
		"Snapshot Template",
		current.SnapshotTemplates,
		desired.SnapshotTemplates,
		func(v model.SnapshotTemplate) string { return v.TemplateID },
	); err != nil {
		return err
	}
	return nil
}

func ensureRows[T any](name string, current, desired []T, id func(T) string) error {
	set := make(map[string]T, len(desired))
	for _, value := range desired {
		set[id(value)] = value
	}
	for _, value := range current {
		desiredValue, ok := set[id(value)]
		if !ok {
			return fmt.Errorf("target %s %q appeared after dry-run", name, id(value))
		}
		if !reflect.DeepEqual(value, desiredValue) {
			return fmt.Errorf("target %s %q changed after dry-run", name, id(value))
		}
	}
	return nil
}

func insertMissing(ctx context.Context, tx pgx.Tx, current, desired *model.CatalogData) error {
	teamIDs := ids(current.Teams, func(v model.Team) string { return v.ID })
	for _, value := range desired.Teams {
		if _, ok := teamIDs[value.ID]; !ok {
			return fmt.Errorf("import does not create target Team %q", value.ID)
		}
	}
	templateIDs := ids(current.Templates, func(v model.Template) string { return v.ID })
	for _, v := range desired.Templates {
		if _, ok := templateIDs[v.ID]; !ok {
			_, err := tx.Exec(ctx, sqlInsertTemplate,
				v.ID,
				v.CreatedAt,
				v.UpdatedAt,
				v.Public,
				v.BuildCount,
				v.SpawnCount,
				v.LastSpawnedAt,
				v.TeamID,
				v.CreatedBy,
				v.ClusterID,
				v.Source,
			)
			if err != nil {
				return fmt.Errorf("insert template %q: %w", v.ID, err)
			}
		}
	}
	buildIDs := ids(current.Builds, func(v model.Build) string { return v.ID })
	for _, v := range desired.Builds {
		if _, ok := buildIDs[v.ID]; !ok {
			// 目标触发器会按原始 status 重算 status_group；写入前先用同一
			// 领域映射核对，避免提交后状态被静默改写。
			if err := validateBuildStatusGroup(v); err != nil {
				return err
			}
			reason := string(v.Reason)
			if reason == "" {
				reason = "{}"
			}
			_, err := tx.Exec(ctx, sqlInsertBuild,
				v.ID,
				v.CreatedAt,
				v.UpdatedAt,
				v.FinishedAt,
				v.Status,
				v.Dockerfile,
				v.StartCommand,
				v.VCPU,
				v.RAMMB,
				v.FreeDiskSizeMB,
				v.TotalDiskSizeMB,
				v.KernelVersion,
				v.FirecrackerVersion,
				v.LegacyTemplateID,
				v.EnvdVersion,
				v.ReadyCommand,
				reason,
				v.Version,
				v.CPUArchitecture,
				v.CPUFamily,
				v.CPUModel,
				v.CPUModelName,
				v.CPUFlags,
				v.StatusGroup,
				v.TeamID,
			)
			if err != nil {
				return fmt.Errorf("insert build %q: %w", v.ID, err)
			}
		}
	}
	assignmentIDs := ids(current.Assignments, func(v model.BuildAssignment) string { return v.ID })
	for _, v := range desired.Assignments {
		if _, ok := assignmentIDs[v.ID]; !ok {
			_, err := tx.Exec(ctx, sqlInsertAssignment,
				v.ID, v.TemplateID, v.BuildID, v.Tag, v.Source, v.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert assignment %q: %w", v.ID, err)
			}
		}
	}
	aliasIDs := ids(current.Aliases, func(v model.Alias) string { return v.ID })
	for _, v := range desired.Aliases {
		if _, ok := aliasIDs[v.ID]; !ok {
			_, err := tx.Exec(ctx, sqlInsertAlias,
				v.ID, v.Alias, v.TemplateID, v.IsRenamable, v.Namespace,
			)
			if err != nil {
				return fmt.Errorf("insert alias %q: %w", v.Alias, err)
			}
		}
	}
	snapshotIDs := ids(current.SnapshotTemplates, func(v model.SnapshotTemplate) string { return v.TemplateID })
	for _, v := range desired.SnapshotTemplates {
		if _, ok := snapshotIDs[v.TemplateID]; !ok {
			_, err := tx.Exec(ctx, sqlInsertSnapshotTemplate, v.TemplateID, v.SandboxID, v.CreatedAt)
			if err != nil {
				return fmt.Errorf("insert snapshot template %q: %w", v.TemplateID, err)
			}
		}
	}
	return nil
}

func ids[T any](values []T, id func(T) string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[id(value)] = struct{}{}
	}
	return result
}
