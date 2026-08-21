package importer

import (
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

func TestTargetNamespaceRebindsByKind(t *testing.T) {
	source := "source-team"
	options := Options{
		IncludeGlobalAliases: true,
		LiteralNamespaceMap:  map[string]string{"archive": "target-archive"},
	}
	tests := []struct {
		name        string
		record      bundle.AliasRecord
		want        *string
		wantInclude bool
	}{
		{name: "global", record: bundle.AliasRecord{NamespaceKind: bundle.NamespaceGlobal}, wantInclude: true},
		{name: "team", record: bundle.AliasRecord{NamespaceKind: bundle.NamespaceTeam, SourceNamespace: &source}, want: stringPointer("target-team"), wantInclude: true},
		{name: "literal", record: bundle.AliasRecord{NamespaceKind: bundle.NamespaceLiteral, SourceNamespace: stringPointer("archive")}, want: stringPointer("target-archive"), wantInclude: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, include, err := targetNamespace(test.record, "target-team", options)
			if err != nil {
				t.Fatal(err)
			}
			if include != test.wantInclude || !equalOptionalString(got, test.want) {
				t.Fatalf("targetNamespace() = (%v, %t), want (%v, %t)", got, include, test.want, test.wantInclude)
			}
		})
	}
}

func TestTargetNamespaceSkipsGlobalByDefault(t *testing.T) {
	namespace, include, err := targetNamespace(bundle.AliasRecord{NamespaceKind: bundle.NamespaceGlobal}, "team", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if namespace != nil || include {
		t.Fatalf("targetNamespace() = (%v, %t), want (nil, false)", namespace, include)
	}
}

func TestDeterministicLegacyTemplatesUsesSmallestTemplateID(t *testing.T) {
	got := deterministicLegacyTemplates([]bundle.AssignmentRecord{
		{TemplateID: "template-z", BuildID: "build"},
		{TemplateID: "template-a", BuildID: "build"},
	})
	if got["build"] != "template-a" {
		t.Fatalf("legacy template = %q, want template-a", got["build"])
	}
}

func TestAliasIdentityDoesNotUseDisplayString(t *testing.T) {
	namespace := "team"
	global := identifyAlias(model.Alias{Alias: "team/name"})
	scoped := identifyAlias(model.Alias{Namespace: &namespace, Alias: "name"})
	if global == scoped {
		t.Fatal("global and scoped aliases collapsed to the same identity")
	}
}

func stringPointer(value string) *string { return &value }

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
