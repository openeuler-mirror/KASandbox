package catalog

import (
	"fmt"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

// Validate 校验一份 Catalog 快照的引用完整性与唯一性不变量,覆盖六类实体:
//
//   - Team:ID/slug 非空,ID 与 slug 各自唯一;
//   - Template:ID 唯一,source ∈ {template, snapshot_template},team 引用存在;
//   - Alias:ID 唯一,(namespace, alias) 唯一(对应数据库唯一键),template 引用存在;
//   - Build:ID 唯一,team 引用存在(历史 NULL 容忍),status↔status_group 映射一致;
//   - Assignment:ID 唯一,template/build 引用存在,tag/source 非空;
//   - SnapshotTemplate:与 template 一对一,只能挂在 snapshot_template 上。
//
// File Catalog 加载后、PostgreSQL 快照读取末尾、导入 Commit 写入前都会调用,
// 使文件与 PostgreSQL 两种 Catalog 遵守同一套不变量:坏数据在读入口显式
// 失败,而不是在写出口扩散进目标环境。
func Validate(data *model.CatalogData) error {
	if data == nil {
		return fmt.Errorf("catalog is nil")
	}

	teams := make(map[string]struct{}, len(data.Teams))
	teamSlugs := make(map[string]string, len(data.Teams))
	for _, team := range data.Teams {
		if team.ID == "" || team.Slug == "" {
			return fmt.Errorf("team id and slug are required")
		}
		if _, exists := teams[team.ID]; exists {
			return fmt.Errorf("duplicate team id %q", team.ID)
		}
		if prior, exists := teamSlugs[team.Slug]; exists {
			return fmt.Errorf("duplicate team slug %q on %q and %q", team.Slug, prior, team.ID)
		}
		teams[team.ID] = struct{}{}
		teamSlugs[team.Slug] = team.ID
	}

	templates := make(map[string]string, len(data.Templates))
	for _, template := range data.Templates {
		if template.ID == "" {
			return fmt.Errorf("template id is required")
		}
		if template.Source != model.SourceTemplate && template.Source != model.SourceSnapshotTemplate {
			return fmt.Errorf("template %q has unsupported source %q", template.ID, template.Source)
		}
		if _, exists := teams[template.TeamID]; !exists {
			return fmt.Errorf("template %q references missing team %q", template.ID, template.TeamID)
		}
		if _, exists := templates[template.ID]; exists {
			return fmt.Errorf("duplicate template id %q", template.ID)
		}
		templates[template.ID] = template.Source
	}

	// Alias 的数据库唯一键是 (namespace, alias)，而不是 TemplateID。两个
	// Template 不能在同一命名空间占用同一个对外名称。
	type aliasName struct {
		namespace    string
		hasNamespace bool
		alias        string
	}
	aliasIDs := make(map[string]struct{}, len(data.Aliases))
	aliases := make(map[aliasName]string, len(data.Aliases))
	for _, alias := range data.Aliases {
		if alias.ID == "" {
			return fmt.Errorf("alias id is required")
		}
		if _, exists := aliasIDs[alias.ID]; exists {
			return fmt.Errorf("duplicate alias id %q", alias.ID)
		}
		if alias.Alias == "" {
			return fmt.Errorf("alias %q has an empty name", alias.ID)
		}
		if _, exists := templates[alias.TemplateID]; !exists {
			return fmt.Errorf("alias %q references missing template %q", alias.Alias, alias.TemplateID)
		}
		key := aliasName{alias: alias.Alias}
		if alias.Namespace != nil {
			if *alias.Namespace == "" {
				return fmt.Errorf("alias %q has an empty namespace", alias.Alias)
			}
			key.namespace = *alias.Namespace
			key.hasNamespace = true
		}
		if prior, exists := aliases[key]; exists {
			return fmt.Errorf("duplicate alias %q on templates %q and %q", alias.QualifiedName(), prior, alias.TemplateID)
		}
		aliasIDs[alias.ID] = struct{}{}
		aliases[key] = alias.TemplateID
	}

	builds := make(map[string]struct{}, len(data.Builds))
	for _, build := range data.Builds {
		if build.ID == "" {
			return fmt.Errorf("build id is required")
		}
		// 历史 env_builds.team_id 可为 NULL(preflight 也声明其可空),加载为 ""。
		// Build 的 Team 不进入 Bundle,导入时统一重绑,因此只校验非空引用;
		// 否则一条遗留行会让整个 Catalog 的 list/export/import 全部失败。
		if build.TeamID != "" {
			if _, exists := teams[build.TeamID]; !exists {
				return fmt.Errorf("build %q references missing team %q", build.ID, build.TeamID)
			}
		}
		if _, exists := builds[build.ID]; exists {
			return fmt.Errorf("duplicate build id %q", build.ID)
		}
		if err := validateBuildStatusGroup(build); err != nil {
			return err
		}
		builds[build.ID] = struct{}{}
	}

	assignments := make(map[string]struct{}, len(data.Assignments))
	for _, assignment := range data.Assignments {
		if assignment.ID == "" {
			return fmt.Errorf("assignment id is required")
		}
		if _, exists := assignments[assignment.ID]; exists {
			return fmt.Errorf("duplicate assignment id %q", assignment.ID)
		}
		if _, exists := templates[assignment.TemplateID]; !exists {
			return fmt.Errorf("assignment %q references missing template %q", assignment.ID, assignment.TemplateID)
		}
		if _, exists := builds[assignment.BuildID]; !exists {
			return fmt.Errorf("assignment %q references missing build %q", assignment.ID, assignment.BuildID)
		}
		if assignment.Tag == "" {
			return fmt.Errorf("assignment %q has an empty tag", assignment.ID)
		}
		if assignment.Source == "" {
			return fmt.Errorf("assignment %q has an empty source", assignment.ID)
		}
		assignments[assignment.ID] = struct{}{}
	}

	// snapshot_templates 以 TemplateID 表示一对一的来源信息；普通 Template
	// 不能携带该行，同一个 Snapshot Template 也不能出现两份来源记录。
	snapshotTemplates := make(map[string]struct{}, len(data.SnapshotTemplates))
	for _, snapshot := range data.SnapshotTemplates {
		templateSource, exists := templates[snapshot.TemplateID]
		if !exists {
			return fmt.Errorf("snapshot template references missing template %q", snapshot.TemplateID)
		}
		if templateSource != model.SourceSnapshotTemplate {
			return fmt.Errorf("snapshot row references non-snapshot template %q", snapshot.TemplateID)
		}
		if snapshot.SandboxID == "" {
			return fmt.Errorf("snapshot template %q has an empty sandbox id", snapshot.TemplateID)
		}
		if _, exists := snapshotTemplates[snapshot.TemplateID]; exists {
			return fmt.Errorf("duplicate snapshot template row %q", snapshot.TemplateID)
		}
		snapshotTemplates[snapshot.TemplateID] = struct{}{}
	}
	for templateID, source := range templates {
		if source == model.SourceSnapshotTemplate {
			if _, exists := snapshotTemplates[templateID]; !exists {
				return fmt.Errorf("snapshot template %q has no snapshot row", templateID)
			}
		}
	}

	return nil
}

func validateBuildStatusGroup(build model.Build) error {
	expected := model.StatusGroupForBuildStatus(build.Status)
	if build.StatusGroup != expected {
		return fmt.Errorf("build %q status %q maps to status_group %q, got %q", build.ID, build.Status, expected, build.StatusGroup)
	}
	return nil
}
