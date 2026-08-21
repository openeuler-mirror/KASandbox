package selection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

type Options struct {
	TemplateIDs []string `json:"template_ids,omitempty"`
	Names       []string `json:"names,omitempty"`
	NameGlobs   []string `json:"name_globs,omitempty"`
	All         bool     `json:"all"`
	SourceTeam  string   `json:"source_team,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	AllTags     bool     `json:"all_tags"`
	BuildScope  string   `json:"build_scope"`
	BuildIDs    []string `json:"build_ids,omitempty"`
	// AllowNoReadyBuilds 供 list 的目录发现场景使用:缺 ready Build 的 Template
	// 保留在结果中(0 个 Assignment)而不是整体失败。不参与序列化,export 不设置。
	AllowNoReadyBuilds bool `json:"-"`
}

type Result struct {
	Data        model.CatalogData `json:"data"`
	Options     Options           `json:"options"`
	Fingerprint string            `json:"fingerprint"`
}

func Select(data *model.CatalogData, options Options) (*Result, error) {
	options = normalizeOptions(options)
	if err := validateOptions(options); err != nil {
		return nil, err
	}

	teamFilter, err := resolveTeamFilter(data.Teams, options.SourceTeam)
	if err != nil {
		return nil, err
	}

	aliasesByTemplate := make(map[string][]model.Alias)
	for _, alias := range data.Aliases {
		aliasesByTemplate[alias.TemplateID] = append(aliasesByTemplate[alias.TemplateID], alias)
	}

	// Runtime Snapshot 带有实例身份和节点状态，不属于 Template 迁移范围；
	// Snapshot Template 已经具备普通 Template/Build/Tag 关系，因此可以一起选择。
	selectedTemplateIDs := make(map[string]struct{})
	eligibleTemplateIDs := make(map[string]struct{})
	matchedNames := make(map[string]struct{}, len(options.Names))
	matchedGlobs := make(map[string]struct{}, len(options.NameGlobs))
	for _, template := range data.Templates {
		if template.Source != model.SourceTemplate && template.Source != model.SourceSnapshotTemplate {
			continue
		}
		if teamFilter != "" && template.TeamID != teamFilter {
			continue
		}
		eligibleTemplateIDs[template.ID] = struct{}{}
		matched := options.All || slices.Contains(options.TemplateIDs, template.ID)
		for _, alias := range aliasesByTemplate[template.ID] {
			name := alias.QualifiedName()
			if slices.Contains(options.Names, name) {
				matchedNames[name] = struct{}{}
				matched = true
			}
			for _, glob := range options.NameGlobs {
				fullMatch, _ := path.Match(glob, name)
				aliasMatch, _ := path.Match(glob, alias.Alias)
				if fullMatch || aliasMatch {
					matchedGlobs[glob] = struct{}{}
					matched = true
				}
			}
		}
		if matched {
			selectedTemplateIDs[template.ID] = struct{}{}
		}
	}
	// 迁移命令不能把拼写错误静默降级成“部分成功”；每个显式选择器都必须
	// 在 Source Team 过滤后命中至少一个可迁移 Template。
	for _, templateID := range options.TemplateIDs {
		if _, ok := eligibleTemplateIDs[templateID]; !ok {
			return nil, fmt.Errorf("template id %q did not match an eligible template", templateID)
		}
	}
	for _, name := range options.Names {
		if _, ok := matchedNames[name]; !ok {
			return nil, fmt.Errorf("template name %q did not match an eligible template", name)
		}
	}
	for _, glob := range options.NameGlobs {
		if _, ok := matchedGlobs[glob]; !ok {
			return nil, fmt.Errorf("template name glob %q matched no eligible templates", glob)
		}
	}
	if len(selectedTemplateIDs) == 0 {
		return nil, fmt.Errorf("template selection matched no templates")
	}

	buildByID := make(map[string]model.Build, len(data.Builds))
	for _, build := range data.Builds {
		buildByID[build.ID] = build
	}

	assignments, selectedBuildIDs, err := selectAssignments(data.Assignments, buildByID, selectedTemplateIDs, options)
	if err != nil {
		return nil, err
	}

	selected := model.CatalogData{SchemaVersion: data.SchemaVersion}
	selectedTeamIDs := make(map[string]struct{})
	for _, template := range data.Templates {
		if _, ok := selectedTemplateIDs[template.ID]; ok {
			selected.Templates = append(selected.Templates, template)
			selectedTeamIDs[template.TeamID] = struct{}{}
		}
	}
	for _, team := range data.Teams {
		if _, ok := selectedTeamIDs[team.ID]; ok {
			selected.Teams = append(selected.Teams, team)
		}
	}
	for _, alias := range data.Aliases {
		if _, ok := selectedTemplateIDs[alias.TemplateID]; ok {
			selected.Aliases = append(selected.Aliases, alias)
		}
	}
	for _, build := range data.Builds {
		if _, ok := selectedBuildIDs[build.ID]; ok {
			selected.Builds = append(selected.Builds, build)
		}
	}
	selected.Assignments = assignments
	for _, snapshot := range data.SnapshotTemplates {
		if _, ok := selectedTemplateIDs[snapshot.TemplateID]; ok {
			selected.SnapshotTemplates = append(selected.SnapshotTemplates, snapshot)
		}
	}

	sortCatalog(&selected)
	fingerprint, err := fingerprint(selected)
	if err != nil {
		return nil, err
	}

	return &Result{Data: selected, Options: options, Fingerprint: fingerprint}, nil
}

func validateOptions(options Options) error {
	if !options.All && len(options.TemplateIDs) == 0 && len(options.Names) == 0 && len(options.NameGlobs) == 0 {
		return fmt.Errorf("a template selector is required; use --all explicitly")
	}
	if options.All && (len(options.TemplateIDs) > 0 || len(options.Names) > 0 || len(options.NameGlobs) > 0) {
		return fmt.Errorf("--all is mutually exclusive with template id, name, and name-glob selectors")
	}
	if options.AllTags && len(options.Tags) > 0 {
		return fmt.Errorf("--all-tags and --tag are mutually exclusive")
	}
	if len(options.BuildIDs) > 0 && (options.AllTags || len(options.Tags) > 0 || (options.BuildScope != "" && options.BuildScope != "latest")) {
		return fmt.Errorf("--build-id is mutually exclusive with tag and build-scope options")
	}
	if options.BuildScope != "" && options.BuildScope != "latest" && options.BuildScope != "all" {
		return fmt.Errorf("build scope must be latest or all")
	}
	for _, glob := range options.NameGlobs {
		if _, err := path.Match(glob, "probe"); err != nil {
			return fmt.Errorf("invalid name glob %q: %w", glob, err)
		}
	}
	return nil
}

func normalizeOptions(options Options) Options {
	// 先去掉 CLI 重复参数中的空值，再决定是否补默认 Tag。否则 --tag ''
	// 会看似指定了 Tag，实际规范化后却留下一个空选择集。
	options.TemplateIDs = uniqueSorted(options.TemplateIDs)
	options.Names = uniqueSorted(options.Names)
	options.NameGlobs = uniqueSorted(options.NameGlobs)
	options.Tags = uniqueSorted(options.Tags)
	options.BuildIDs = uniqueSorted(options.BuildIDs)
	if options.BuildScope == "" {
		options.BuildScope = "latest"
	}
	if len(options.BuildIDs) == 0 && !options.AllTags && len(options.Tags) == 0 {
		options.Tags = []string{model.DefaultTag}
	}
	return options
}

func resolveTeamFilter(teams []model.Team, reference string) (string, error) {
	if reference == "" {
		return "", nil
	}
	prefix, value, ok := strings.Cut(reference, ":")
	if !ok || value == "" || (prefix != "id" && prefix != "slug") {
		return "", fmt.Errorf("team reference must use id:<UUID> or slug:<SLUG>")
	}
	for _, team := range teams {
		if (prefix == "id" && team.ID == value) || (prefix == "slug" && team.Slug == value) {
			return team.ID, nil
		}
	}
	return "", fmt.Errorf("source team %q was not found", reference)
}

func selectAssignments(
	assignments []model.BuildAssignment,
	buildByID map[string]model.Build,
	templateIDs map[string]struct{},
	options Options,
) ([]model.BuildAssignment, map[string]struct{}, error) {
	if len(options.BuildIDs) > 0 {
		return selectExactBuilds(assignments, buildByID, templateIDs, options.BuildIDs)
	}

	tagSet := make(map[string]struct{}, len(options.Tags))
	for _, tag := range options.Tags {
		tagSet[tag] = struct{}{}
	}
	// 先过滤 ready Build，再按 Template+Tag 分组选择 latest。pending 的新 Build
	// 不会遮住较早但仍可运行的 ready Build。
	candidates := make(map[string][]model.BuildAssignment)
	templatesWithCandidates := make(map[string]struct{}, len(templateIDs))
	templateTagsWithCandidates := make(map[string]struct{})
	for _, assignment := range assignments {
		if _, ok := templateIDs[assignment.TemplateID]; !ok {
			continue
		}
		build, ok := buildByID[assignment.BuildID]
		if !ok || build.StatusGroup != model.StatusGroupReady {
			continue
		}
		if !options.AllTags {
			if _, ok := tagSet[assignment.Tag]; !ok {
				continue
			}
		}
		key := assignment.TemplateID + "\x00" + assignment.Tag
		candidates[key] = append(candidates[key], assignment)
		templatesWithCandidates[assignment.TemplateID] = struct{}{}
		templateTagsWithCandidates[key] = struct{}{}
	}
	// 目录发现(list 隐式/显式 --all)不因个别 Template 缺 ready Build 而失败;
	// export 保持 fail-loud,拼写错误和空选择必须显式暴露。
	if !options.AllowNoReadyBuilds {
		for templateID := range templateIDs {
			if options.AllTags {
				if _, ok := templatesWithCandidates[templateID]; !ok {
					return nil, nil, fmt.Errorf("template %q has no ready build", templateID)
				}
				continue
			}
			for _, tag := range options.Tags {
				key := templateID + "\x00" + tag
				if _, ok := templateTagsWithCandidates[key]; !ok {
					return nil, nil, fmt.Errorf("template %q has no ready build for tag %q", templateID, tag)
				}
			}
		}
	}

	selected := make([]model.BuildAssignment, 0)
	selectedBuilds := make(map[string]struct{})
	for key, group := range candidates {
		slices.SortFunc(group, compareAssignments)
		if options.BuildScope == "latest" {
			last := group[len(group)-1]
			// 同一纳秒出现两条 latest 无法可靠判定先后，宁可要求调用者精确指定 Build。
			if len(group) > 1 && group[len(group)-2].CreatedAt.Equal(last.CreatedAt) {
				return nil, nil, fmt.Errorf("latest ready assignment is ambiguous for %q at %s", strings.ReplaceAll(key, "\x00", ":"), last.CreatedAt)
			}
			group = group[len(group)-1:]
		}
		selected = append(selected, group...)
		for _, assignment := range group {
			selectedBuilds[assignment.BuildID] = struct{}{}
		}
	}
	slices.SortFunc(selected, compareAssignments)
	return selected, selectedBuilds, nil
}

func selectExactBuilds(
	assignments []model.BuildAssignment,
	buildByID map[string]model.Build,
	templateIDs map[string]struct{},
	requested []string,
) ([]model.BuildAssignment, map[string]struct{}, error) {
	requestedSet := make(map[string]struct{}, len(requested))
	for _, buildID := range requested {
		requestedSet[buildID] = struct{}{}
	}
	found := make(map[string]struct{}, len(requested))
	templatesWithBuilds := make(map[string]struct{}, len(templateIDs))
	selected := make([]model.BuildAssignment, 0)
	for _, assignment := range assignments {
		if _, ok := templateIDs[assignment.TemplateID]; !ok {
			continue
		}
		if _, ok := requestedSet[assignment.BuildID]; !ok {
			continue
		}
		build, ok := buildByID[assignment.BuildID]
		if !ok {
			return nil, nil, fmt.Errorf("requested build %q has no build row", assignment.BuildID)
		}
		if build.StatusGroup != model.StatusGroupReady {
			return nil, nil, fmt.Errorf("requested build %q is %q, not ready", build.ID, build.StatusGroup)
		}
		selected = append(selected, assignment)
		found[assignment.BuildID] = struct{}{}
		templatesWithBuilds[assignment.TemplateID] = struct{}{}
	}
	for _, buildID := range requested {
		if _, ok := found[buildID]; !ok {
			return nil, nil, fmt.Errorf("requested build %q is not assigned to a selected template", buildID)
		}
	}
	for templateID := range templateIDs {
		if _, ok := templatesWithBuilds[templateID]; !ok {
			return nil, nil, fmt.Errorf("template %q has none of the requested builds", templateID)
		}
	}
	slices.SortFunc(selected, compareAssignments)
	return selected, found, nil
}

func compareAssignments(left, right model.BuildAssignment) int {
	if left.TemplateID != right.TemplateID {
		return strings.Compare(left.TemplateID, right.TemplateID)
	}
	if left.Tag != right.Tag {
		return strings.Compare(left.Tag, right.Tag)
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		if left.CreatedAt.Before(right.CreatedAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(left.ID, right.ID)
}

func sortCatalog(data *model.CatalogData) {
	// fingerprint 来自 CatalogData 的 JSON；所有切片必须稳定排序，避免数据库返回顺序
	// 或 map 遍历顺序让同一选择产生不同 Bundle digest。
	slices.SortFunc(data.Teams, func(a, b model.Team) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(data.Templates, func(a, b model.Template) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(data.Aliases, func(a, b model.Alias) int {
		if a.TemplateID != b.TemplateID {
			return strings.Compare(a.TemplateID, b.TemplateID)
		}
		return strings.Compare(a.QualifiedName(), b.QualifiedName())
	})
	slices.SortFunc(data.Builds, func(a, b model.Build) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(data.Assignments, compareAssignments)
	slices.SortFunc(data.SnapshotTemplates, func(a, b model.SnapshotTemplate) int { return strings.Compare(a.TemplateID, b.TemplateID) })
}

func fingerprint(data model.CatalogData) (string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("encode selection fingerprint: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
