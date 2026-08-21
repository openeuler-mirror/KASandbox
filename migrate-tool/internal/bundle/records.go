package bundle

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
)

var recordOrder = []string{"templates", "aliases", "builds", "assignments", "snapshot-templates"}

func RecordsFromSelection(result *selection.Result) (Records, error) {
	teamByID := make(map[string]model.Team, len(result.Data.Teams))
	for _, team := range result.Data.Teams {
		teamByID[team.ID] = team
	}
	templateByID := make(map[string]model.Template, len(result.Data.Templates))

	records := Records{}
	for _, template := range result.Data.Templates {
		templateByID[template.ID] = template
		records.Templates = append(records.Templates, TemplateRecord{
			ID: template.ID, CreatedAt: template.CreatedAt, UpdatedAt: template.UpdatedAt,
			Public: template.Public, Source: template.Source, BuildCount: template.BuildCount,
			SpawnCount: template.SpawnCount, LastSpawnedAt: template.LastSpawnedAt,
		})
	}
	for _, alias := range result.Data.Aliases {
		template, ok := templateByID[alias.TemplateID]
		if !ok {
			return Records{}, fmt.Errorf("alias %q references unselected template %q", alias.Alias, alias.TemplateID)
		}
		team, ok := teamByID[template.TeamID]
		if !ok {
			return Records{}, fmt.Errorf("template %q references missing source team %q", template.ID, template.TeamID)
		}
		kind := NamespaceGlobal
		if alias.Namespace != nil {
			kind = NamespaceLiteral
			if *alias.Namespace == team.Slug {
				kind = NamespaceTeam
			}
		}
		records.Aliases = append(records.Aliases, AliasRecord{
			SourceID: alias.ID, TemplateID: alias.TemplateID, Alias: alias.Alias,
			NamespaceKind: kind, SourceNamespace: alias.Namespace, IsRenamable: alias.IsRenamable,
		})
	}
	for _, build := range result.Data.Builds {
		records.Builds = append(records.Builds, BuildRecord{
			ID: build.ID, CreatedAt: build.CreatedAt, UpdatedAt: build.UpdatedAt,
			FinishedAt: build.FinishedAt, Status: build.Status, StatusGroup: build.StatusGroup,
			Dockerfile: build.Dockerfile, StartCommand: build.StartCommand, ReadyCommand: build.ReadyCommand,
			VCPU: build.VCPU, RAMMB: build.RAMMB, FreeDiskSizeMB: build.FreeDiskSizeMB,
			TotalDiskSizeMB: build.TotalDiskSizeMB, KernelVersion: build.KernelVersion,
			FirecrackerVersion: build.FirecrackerVersion, EnvdVersion: build.EnvdVersion,
			Reason: build.Reason, Version: build.Version, CPUArchitecture: build.CPUArchitecture,
			CPUFamily: build.CPUFamily, CPUModel: build.CPUModel, CPUModelName: build.CPUModelName,
			CPUFlags: build.CPUFlags,
		})
	}
	for _, assignment := range result.Data.Assignments {
		records.Assignments = append(records.Assignments, AssignmentRecord{
			SourceID: assignment.ID, TemplateID: assignment.TemplateID, BuildID: assignment.BuildID,
			Tag: assignment.Tag, Source: assignment.Source, CreatedAt: assignment.CreatedAt,
		})
	}
	for _, snapshot := range result.Data.SnapshotTemplates {
		records.SnapshotTemplates = append(records.SnapshotTemplates, SnapshotTemplateRecord(snapshot))
	}

	sortRecords(&records)
	if err := ValidateRecords(records); err != nil {
		return Records{}, err
	}
	return records, nil
}

func WriteRecords(root string, records Records) ([]RecordDescriptor, error) {
	if err := os.MkdirAll(filepath.Join(root, "records"), 0o755); err != nil {
		return nil, fmt.Errorf("create records directory: %w", err)
	}

	// 五种 record 是 v1 的封闭协议。显式调用保留固定顺序，同时让编译器检查
	// 每个 JSONL 的元素类型，不再通过 any 和运行时 type switch 分发。
	templates, err := writeRecordFile(root, "templates", records.Templates)
	if err != nil {
		return nil, err
	}
	aliases, err := writeRecordFile(root, "aliases", records.Aliases)
	if err != nil {
		return nil, err
	}
	builds, err := writeRecordFile(root, "builds", records.Builds)
	if err != nil {
		return nil, err
	}
	assignments, err := writeRecordFile(root, "assignments", records.Assignments)
	if err != nil {
		return nil, err
	}
	snapshots, err := writeRecordFile(root, "snapshot-templates", records.SnapshotTemplates)
	if err != nil {
		return nil, err
	}
	return []RecordDescriptor{templates, aliases, builds, assignments, snapshots}, nil
}

func ReadRecords(root string, descriptors []RecordDescriptor) (Records, error) {
	byName := make(map[string]RecordDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}
	for _, name := range recordOrder {
		if _, ok := byName[name]; !ok {
			return Records{}, fmt.Errorf("manifest is missing record descriptor %q", name)
		}
	}

	var records Records
	var err error
	if records.Templates, err = readJSONLines[TemplateRecord](root, byName["templates"]); err != nil {
		return Records{}, err
	}
	if records.Aliases, err = readJSONLines[AliasRecord](root, byName["aliases"]); err != nil {
		return Records{}, err
	}
	if records.Builds, err = readJSONLines[BuildRecord](root, byName["builds"]); err != nil {
		return Records{}, err
	}
	if records.Assignments, err = readJSONLines[AssignmentRecord](root, byName["assignments"]); err != nil {
		return Records{}, err
	}
	if records.SnapshotTemplates, err = readJSONLines[SnapshotTemplateRecord](root, byName["snapshot-templates"]); err != nil {
		return Records{}, err
	}
	if err := ValidateRecords(records); err != nil {
		return Records{}, err
	}
	return records, nil
}

func ValidateRecords(records Records) error {
	templates := make(map[string]string, len(records.Templates))
	for _, template := range records.Templates {
		if template.ID == "" {
			return fmt.Errorf("template id is empty")
		}
		if template.Source != model.SourceTemplate && template.Source != model.SourceSnapshotTemplate {
			return fmt.Errorf("template %q has unsupported source %q", template.ID, template.Source)
		}
		if _, ok := templates[template.ID]; ok {
			return fmt.Errorf("duplicate template %q", template.ID)
		}
		templates[template.ID] = template.Source
	}
	builds := make(map[string]struct{}, len(records.Builds))
	for _, build := range records.Builds {
		if build.ID == "" {
			return fmt.Errorf("build id is required")
		}
		if build.StatusGroup != model.StatusGroupReady {
			return fmt.Errorf("build %q is %q, not ready", build.ID, build.StatusGroup)
		}
		if expected := model.StatusGroupForBuildStatus(build.Status); expected != build.StatusGroup {
			return fmt.Errorf("build %q status %q maps to status_group %q, got %q", build.ID, build.Status, expected, build.StatusGroup)
		}
		if _, ok := builds[build.ID]; ok {
			return fmt.Errorf("duplicate build %q", build.ID)
		}
		builds[build.ID] = struct{}{}
	}
	aliasIDs := make(map[string]struct{}, len(records.Aliases))
	aliases := make(map[string]string, len(records.Aliases))
	for _, alias := range records.Aliases {
		if alias.SourceID == "" || alias.Alias == "" {
			return fmt.Errorf("alias source id and name are required")
		}
		if _, exists := aliasIDs[alias.SourceID]; exists {
			return fmt.Errorf("duplicate alias id %q", alias.SourceID)
		}
		if _, ok := templates[alias.TemplateID]; !ok {
			return fmt.Errorf("alias %q references missing template %q", alias.Alias, alias.TemplateID)
		}
		if alias.NamespaceKind != NamespaceTeam && alias.NamespaceKind != NamespaceGlobal && alias.NamespaceKind != NamespaceLiteral {
			return fmt.Errorf("alias %q has invalid namespace kind %q", alias.Alias, alias.NamespaceKind)
		}
		if alias.NamespaceKind == NamespaceGlobal && alias.SourceNamespace != nil {
			return fmt.Errorf("global alias %q unexpectedly has namespace %q", alias.Alias, *alias.SourceNamespace)
		}
		if alias.NamespaceKind != NamespaceGlobal && (alias.SourceNamespace == nil || *alias.SourceNamespace == "") {
			return fmt.Errorf("%s alias %q has no namespace", alias.NamespaceKind, alias.Alias)
		}
		namespace := ""
		if alias.NamespaceKind == NamespaceLiteral {
			namespace = *alias.SourceNamespace
		}
		// TemplateID 不能进入唯一键：两个模板占用同一个可见名称，导入后仍会冲突。
		key := alias.NamespaceKind + "\x00" + namespace + "\x00" + alias.Alias
		if prior, ok := aliases[key]; ok {
			return fmt.Errorf("duplicate alias %q and %q", prior, alias.SourceID)
		}
		aliasIDs[alias.SourceID] = struct{}{}
		aliases[key] = alias.SourceID
	}
	assignments := make(map[string]struct{}, len(records.Assignments))
	templatesWithBuild := make(map[string]struct{}, len(records.Templates))
	assignedBuilds := make(map[string]struct{}, len(records.Builds))
	for _, assignment := range records.Assignments {
		if assignment.SourceID == "" {
			return fmt.Errorf("assignment id is required")
		}
		if _, exists := assignments[assignment.SourceID]; exists {
			return fmt.Errorf("duplicate assignment id %q", assignment.SourceID)
		}
		if _, ok := templates[assignment.TemplateID]; !ok {
			return fmt.Errorf("assignment %q references missing template %q", assignment.SourceID, assignment.TemplateID)
		}
		if _, ok := builds[assignment.BuildID]; !ok {
			return fmt.Errorf("assignment %q references missing build %q", assignment.SourceID, assignment.BuildID)
		}
		if assignment.Tag == "" {
			return fmt.Errorf("assignment %q has an empty tag", assignment.SourceID)
		}
		if assignment.Source == "" {
			return fmt.Errorf("assignment %q has an empty source", assignment.SourceID)
		}
		assignments[assignment.SourceID] = struct{}{}
		templatesWithBuild[assignment.TemplateID] = struct{}{}
		assignedBuilds[assignment.BuildID] = struct{}{}
	}
	for templateID := range templates {
		if _, exists := templatesWithBuild[templateID]; !exists {
			return fmt.Errorf("template %q has no build assignment", templateID)
		}
	}
	for buildID := range builds {
		if _, exists := assignedBuilds[buildID]; !exists {
			return fmt.Errorf("build %q has no assignment", buildID)
		}
	}
	snapshots := make(map[string]struct{}, len(records.SnapshotTemplates))
	for _, snapshot := range records.SnapshotTemplates {
		if templates[snapshot.TemplateID] != model.SourceSnapshotTemplate {
			return fmt.Errorf("snapshot row references non-snapshot template %q", snapshot.TemplateID)
		}
		if snapshot.SandboxID == "" {
			return fmt.Errorf("snapshot template %q has an empty sandbox id", snapshot.TemplateID)
		}
		if _, exists := snapshots[snapshot.TemplateID]; exists {
			return fmt.Errorf("duplicate snapshot template %q", snapshot.TemplateID)
		}
		snapshots[snapshot.TemplateID] = struct{}{}
	}
	for templateID, source := range templates {
		if source == model.SourceSnapshotTemplate {
			if _, exists := snapshots[templateID]; !exists {
				return fmt.Errorf("snapshot template %q has no snapshot row", templateID)
			}
		}
	}
	return nil
}

func writeRecordFile[T any](root, name string, values []T) (RecordDescriptor, error) {
	relative := "records/" + name + ".jsonl"
	digest, err := writeJSONLines(filepath.Join(root, filepath.FromSlash(relative)), values)
	if err != nil {
		return RecordDescriptor{}, err
	}
	return RecordDescriptor{Name: name, Path: relative, Count: len(values), SHA256: digest}, nil
}

func writeJSONLines[T any](filename string, values []T) (string, error) {
	output, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("create record file %q: %w", filename, err)
	}
	hash := sha256.New()
	writer := io.MultiWriter(output, hash)
	encoder := json.NewEncoder(writer)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			output.Close()
			return "", fmt.Errorf("encode record file %q: %w", filename, err)
		}
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readJSONLines[T any](root string, descriptor RecordDescriptor) ([]T, error) {
	filename, err := safeBundlePath(root, descriptor.Path)
	if err != nil {
		return nil, err
	}
	input, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open record file %q: %w", descriptor.Path, err)
	}
	defer input.Close()
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(input, hash))
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	// Count 来自外部 Bundle，只参与最终一致性比较，不能作为内存分配提示。
	var result []T
	line := 0
	for scanner.Scan() {
		line++
		var value T
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s line %d: %w", descriptor.Path, line, err)
		}
		var trailing json.RawMessage
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("decode %s line %d: multiple JSON values", descriptor.Path, line)
			}
			return nil, fmt.Errorf("decode %s line %d trailing data: %w", descriptor.Path, line, err)
		}
		result = append(result, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %q: %w", descriptor.Path, err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != descriptor.SHA256 {
		return nil, fmt.Errorf("record %q SHA-256 mismatch: got %s, want %s", descriptor.Path, actual, descriptor.SHA256)
	}
	if len(result) != descriptor.Count {
		return nil, fmt.Errorf("record %q count mismatch: got %d, want %d", descriptor.Path, len(result), descriptor.Count)
	}
	return result, nil
}

func sortRecords(records *Records) {
	slices.SortFunc(records.Templates, func(a, b TemplateRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(records.Aliases, func(a, b AliasRecord) int {
		if a.TemplateID != b.TemplateID {
			return strings.Compare(a.TemplateID, b.TemplateID)
		}
		if a.NamespaceKind != b.NamespaceKind {
			return strings.Compare(a.NamespaceKind, b.NamespaceKind)
		}
		return strings.Compare(a.Alias, b.Alias)
	})
	slices.SortFunc(records.Builds, func(a, b BuildRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(records.Assignments, func(a, b AssignmentRecord) int {
		if a.TemplateID != b.TemplateID {
			return strings.Compare(a.TemplateID, b.TemplateID)
		}
		if a.Tag != b.Tag {
			return strings.Compare(a.Tag, b.Tag)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.SourceID, b.SourceID)
	})
	slices.SortFunc(records.SnapshotTemplates, func(a, b SnapshotTemplateRecord) int { return strings.Compare(a.TemplateID, b.TemplateID) })
}
