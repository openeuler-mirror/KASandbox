package agentenv

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestBuildCreatePlanUsesOnlyTemplateAnnotation(t *testing.T) {
	plan, err := buildCreatePlan(criRunRequest(map[string]string{annTemplate: "template-1"}))
	if err != nil {
		t.Fatal(err)
	}
	withLegacyAnnotations, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate:              "template-1",
		"sandbox.cri/sandbox-id": "other-id",
		"sandbox.cri/build":      "old-build",
		"aenv.dev/template-id":   "other-template",
		"aenv.dev/env-vars":      "invalid-json",
		"aenv.dev/network":       "invalid-json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if withLegacyAnnotations.criID != plan.criID || !proto.Equal(withLegacyAnnotations.spec, plan.spec) {
		t.Fatalf("消减的注解影响了 Sandbox 创建请求: %+v", withLegacyAnnotations.spec)
	}
}

func TestBuildCreatePlanRejectsOversizedTemplate(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate: strings.Repeat("x", templateRefMaxLength+1),
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}
