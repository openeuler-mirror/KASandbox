package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/config"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/exporter"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/importer"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
)

const Version = "0.3.1"

type CLI struct {
	Stdout io.Writer
	Stderr io.Writer
}

func (c CLI) Run(ctx context.Context, args []string) error {
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if len(args) == 0 {
		return c.usageError("a command is required")
	}
	switch args[0] {
	case "version", "--version", "-version":
		if len(args) != 1 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(args[1:], " "))
		}
		fmt.Fprintln(c.Stdout, Version)
		return nil
	case "list":
		return c.runList(ctx, args[1:])
	case "export":
		return c.runExport(ctx, args[1:])
	case "inspect":
		return c.runInspect(args[1:])
	case "verify":
		return c.runVerify(args[1:])
	case "import":
		return c.runImport(ctx, args[1:])
	case "help", "--help", "-h":
		c.printUsage()
		return nil
	default:
		return c.usageError("unknown command %q", args[0])
	}
}

func (c CLI) runList(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	catalogEndpoint := flags.String("catalog", os.Getenv("TM_SOURCE_CATALOG"), "source catalog: postgresql:// URI, local JSON path, or file:// URI (list reads only the catalog)")
	format := flags.String("format", "table", "table or json")
	options := bindSelectionFlags(flags)
	help, err := parseFlags(flags, args)
	if err != nil || help {
		return err
	}
	if err := validateFormat(*format); err != nil {
		return err
	}
	if len(options.TemplateIDs) == 0 && len(options.Names) == 0 && len(options.NameGlobs) == 0 {
		options.All = true
		// list 的全量查询是目录发现:缺 ready Build 的 Template 显示为 0 个
		// Assignment,而不是让整条命令失败。显式选择器保持严格。
		options.AllowNoReadyBuilds = true
	}
	source, err := config.OpenCatalog(*catalogEndpoint)
	if err != nil {
		return err
	}
	data, err := source.Snapshot(ctx)
	if err != nil {
		return err
	}
	selected, err := selection.Select(data, *options)
	if err != nil {
		return err
	}
	if *format == "json" {
		return writeJSON(c.Stdout, selected)
	}
	buildCount := make(map[string]int)
	for _, assignment := range selected.Data.Assignments {
		buildCount[assignment.TemplateID]++
	}
	aliases := make(map[string][]string)
	for _, alias := range selected.Data.Aliases {
		aliases[alias.TemplateID] = append(aliases[alias.TemplateID], alias.QualifiedName())
	}
	w := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "TEMPLATE ID\tSOURCE\tALIASES\tSELECTED ASSIGNMENTS")
	for _, template := range selected.Data.Templates {
		sort.Strings(aliases[template.ID])
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", template.ID, template.Source, strings.Join(aliases[template.ID], ","), buildCount[template.ID])
	}
	return nil
}

func (c CLI) runExport(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	catalogEndpoint := flags.String("catalog", os.Getenv("TM_SOURCE_CATALOG"), "source catalog: postgresql:// URI, local JSON path, or file:// URI")
	storeEndpoint := flags.String("store", os.Getenv("TM_SOURCE_STORE"), "source object store: s3:// URI, local directory, or file:// URI")
	output := flags.String("out", "", "output bundle directory")
	options := bindSelectionFlags(flags)
	help, err := parseFlags(flags, args)
	if err != nil || help {
		return err
	}
	if *output == "" {
		return c.usageError("--out is required")
	}
	source, err := config.OpenCatalog(*catalogEndpoint)
	if err != nil {
		return err
	}
	store, err := config.OpenStore(ctx, *storeEndpoint)
	if err != nil {
		return err
	}
	result, err := (&exporter.Exporter{Catalog: source, Store: store, ToolVersion: Version}).Export(ctx, *options, *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "bundle: %s\ntemplates: %d\nbuilds: %d\nobjects: %d\nbytes: %d\ndigest: %s\n", result.Path,
		result.Manifest.Counts.Templates, result.Manifest.Counts.Builds, result.Manifest.Counts.Objects,
		result.Manifest.Counts.ObjectBytes, result.Manifest.BundleDigest)
	return nil
}

func (c CLI) runInspect(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	format := flags.String("format", "table", "table or json")
	bundlePath, help, err := parseBundleFlags(flags, args)
	if err != nil || help {
		return err
	}
	if err := validateFormat(*format); err != nil {
		return err
	}
	inspected, err := bundle.Inspect(bundlePath)
	if err != nil {
		return err
	}
	if *format == "json" {
		return writeJSON(c.Stdout, inspected.Manifest)
	}
	m := inspected.Manifest
	fmt.Fprintf(c.Stdout, "format: %s/%d\ncreated: %s\ntemplates: %d\naliases: %d\nbuilds: %d\nassignments: %d\nobjects: %d\nbytes: %d\ndigest: %s\n",
		m.Format, m.FormatVersion, m.CreatedAt, m.Counts.Templates, m.Counts.Aliases, m.Counts.Builds,
		m.Counts.Assignments, m.Counts.Objects, m.Counts.ObjectBytes, m.BundleDigest)
	return nil
}

func (c CLI) runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	format := flags.String("format", "table", "table or json")
	bundlePath, help, err := parseBundleFlags(flags, args)
	if err != nil || help {
		return err
	}
	if err := validateFormat(*format); err != nil {
		return err
	}
	verified, err := bundle.Verify(bundlePath)
	if err != nil {
		return err
	}
	if *format == "json" {
		return writeJSON(c.Stdout, map[string]any{"verified": true, "bundle_digest": verified.Manifest.BundleDigest})
	}
	fmt.Fprintf(
		c.Stdout,
		"verified: %s (%d objects, %d bytes)\n",
		verified.Manifest.BundleDigest,
		verified.Manifest.Counts.Objects,
		verified.Manifest.Counts.ObjectBytes,
	)
	return nil
}

func (c CLI) runImport(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(c.Stderr)
	catalogEndpoint := flags.String("catalog", os.Getenv("TM_TARGET_CATALOG"), "target catalog: postgresql:// URI, local JSON path, or file:// URI")
	storeEndpoint := flags.String("store", os.Getenv("TM_TARGET_STORE"), "target object store: mooncake://NAMESPACE, s3:// URI, local directory, or file:// URI")
	targetTeam := flags.String("target-team", "", "target Team as id:<UUID> or slug:<SLUG>")
	includeGlobal := flags.Bool("include-global-aliases", false, "publish global aliases")
	policy := flags.String("conflict-policy", importer.ConflictFail, "fail or skip-identical")
	apply := flags.Bool("apply", false, "apply changes; without this flag only a dry-run is performed")
	format := flags.String("format", "table", "table or json")
	var namespaceMappings stringList
	flags.Var(&namespaceMappings, "literal-namespace", "literal namespace mapping SOURCE=TARGET; repeatable")
	bundlePath, help, err := parseBundleFlags(flags, args)
	if err != nil || help {
		return err
	}
	// 输出格式属于命令参数，必须在校验 Bundle、复制对象或写 Catalog 之前失败。
	if err := validateFormat(*format); err != nil {
		return err
	}
	if *targetTeam == "" {
		return c.usageError("--target-team is required")
	}
	mappings, err := parseMappings(namespaceMappings)
	if err != nil {
		return err
	}
	verified, err := bundle.Verify(bundlePath)
	if err != nil {
		return err
	}
	target, err := config.OpenCatalog(*catalogEndpoint)
	if err != nil {
		return err
	}
	store, err := config.OpenTargetStore(ctx, *storeEndpoint)
	if err != nil {
		return err
	}
	defer store.Close()
	options := importer.Options{TargetTeam: *targetTeam, IncludeGlobalAliases: *includeGlobal, LiteralNamespaceMap: mappings, ConflictPolicy: *policy}
	result, err := (&importer.Importer{Catalog: target, Target: store}).Run(ctx, verified, options, *apply)
	if err != nil {
		return err
	}
	if *format == "json" {
		if err := writeJSON(c.Stdout, result); err != nil {
			return err
		}
	} else {
		printImportPlan(c.Stdout, result)
	}
	if len(result.Plan.Conflicts) > 0 {
		return ConflictError{Count: len(result.Plan.Conflicts)}
	}
	return nil
}

type ConflictError struct{ Count int }

func (e ConflictError) Error() string { return fmt.Sprintf("import has %d conflict(s)", e.Count) }

func bindSelectionFlags(flags *flag.FlagSet) *selection.Options {
	result := &selection.Options{}
	flags.Var((*stringList)(&result.TemplateIDs), "template-id", "Template ID; repeatable")
	flags.Var((*stringList)(&result.Names), "name", "full Alias name; repeatable")
	flags.Var((*stringList)(&result.NameGlobs), "name-glob", "Alias glob; repeatable")
	flags.BoolVar(&result.All, "all", false, "select all Templates")
	flags.StringVar(&result.SourceTeam, "source-team", "", "source Team filter as id:<UUID> or slug:<SLUG>")
	flags.Var((*stringList)(&result.Tags), "tag", "Tag; repeatable")
	flags.BoolVar(&result.AllTags, "all-tags", false, "select all Tags")
	flags.StringVar(&result.BuildScope, "build-scope", "latest", "latest or all")
	flags.Var((*stringList)(&result.BuildIDs), "build-id", "exact ready Build UUID; repeatable")
	return result
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func parseFlags(flags *flag.FlagSet, args []string) (bool, error) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	if flags.NArg() != 0 {
		return false, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return false, nil
}

func parseBundleFlags(flags *flag.FlagSet, args []string) (string, bool, error) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		help, err := parseFlags(flags, args)
		return "", help, err
	}
	bundlePath, remaining, err := positionalBundle(args)
	if err != nil {
		return "", false, err
	}
	help, err := parseFlags(flags, remaining)
	return bundlePath, help, err
}

func validateFormat(value string) error {
	if value != "table" && value != "json" {
		return fmt.Errorf("format must be table or json")
	}
	return nil
}

func positionalBundle(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("bundle directory must be the first argument")
	}
	return args[0], args[1:], nil
}

func parseMappings(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		source, target, ok := strings.Cut(value, "=")
		if !ok || source == "" || target == "" {
			return nil, fmt.Errorf("literal namespace mapping must use SOURCE=TARGET")
		}
		if _, exists := result[source]; exists {
			return nil, fmt.Errorf("duplicate literal namespace mapping for %q", source)
		}
		result[source] = target
	}
	return result, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printImportPlan(output io.Writer, result *importer.Result) {
	mode := "dry-run"
	if result.Applied {
		mode = "applied"
	}
	fmt.Fprintf(output, "mode: %s\ntarget team: %s (%s)\n", mode, result.Plan.TargetTeam.Slug, result.Plan.TargetTeam.ID)
	fmt.Fprintf(
		output,
		"create: templates=%d aliases=%d builds=%d assignments=%d snapshots=%d objects=%d\n",
		result.Plan.Create.Templates,
		result.Plan.Create.Aliases,
		result.Plan.Create.Builds,
		result.Plan.Create.Assignments,
		result.Plan.Create.SnapshotTemplates,
		result.Plan.Create.Objects,
	)
	fmt.Fprintf(
		output,
		"reuse: templates=%d aliases=%d builds=%d assignments=%d snapshots=%d objects=%d\n",
		result.Plan.Reuse.Templates,
		result.Plan.Reuse.Aliases,
		result.Plan.Reuse.Builds,
		result.Plan.Reuse.Assignments,
		result.Plan.Reuse.SnapshotTemplates,
		result.Plan.Reuse.Objects,
	)
	fmt.Fprintf(output, "skip: aliases=%d\n", result.Plan.Skip.Aliases)
	for _, conflict := range result.Plan.Conflicts {
		fmt.Fprintf(output, "conflict: %s %s: %s\n", conflict.Kind, conflict.Key, conflict.Reason)
	}
}

func (c CLI) printUsage() {
	fmt.Fprintln(c.Stdout, "usage: template-migrate <list|export|inspect|verify|import|version> [options]")
}

func (c CLI) usageError(format string, values ...any) error {
	c.printUsage()
	return fmt.Errorf(format, values...)
}

func IsConflict(err error) bool { var target ConflictError; return errors.As(err, &target) }
