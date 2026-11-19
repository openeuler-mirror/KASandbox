package agentenv

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildCreatePlanRejectsUnknownAgentENVAnnotation(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate:        "template-1",
		"aenv.dev/unknown": "value",
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestBuildCreatePlanRejectsReservedVolumeMounts(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate:     "template-1",
		annVolumeMounts: "[]",
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestBuildCreatePlanRejectsInlineSecret(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate: "template-1",
		annEnvVars:  `{"API_TOKEN":"abc"}`,
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestBuildCreatePlanRejectsUnknownAutoResumeField(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate:   "template-1",
		annAutoResume: `{"enabled":true,"policy":"always"}`,
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestBuildCreatePlanRejectsDuplicateJSONKey(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate: "template-1",
		annEnvVars:  `{"MODE":"one","MODE":"two"}`,
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestBuildCreatePlanRejectsOutOfRangeLifecycle(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"timeout": {
			annTemplate:       "template-1",
			annTimeoutSeconds: "86401",
		},
		"max-length": {
			annTemplate:       "template-1",
			annMaxLengthHours: "8761",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildCreatePlan(criRunRequest(annotations))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
			}
		})
	}
}

func TestBuildCreatePlanRejectsNonObjectCustomParams(t *testing.T) {
	_, err := buildCreatePlan(criRunRequest(map[string]string{
		annTemplate:     "template-1",
		annCustomParams: `["not-an-object"]`,
	}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}
