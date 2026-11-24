package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/e2b/e2b-webhook/providers"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 通过实际 Provider 和 HTTP 模板桩验证准入结果，不访问集群或真实 Runtime。
func TestAgentENVAdmissionWithTemplateAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("X-API-Key") != "test-key" {
			t.Error("模板 API 请求不符合只读鉴权契约")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/templates/aliases/alias":
			_, _ = w.Write([]byte(`{"templateID":"canonical"}`))
		case "/templates/canonical":
			_, _ = w.Write([]byte(`{"templateID":"canonical","builds":[{"buildID":"ready-build","status":"ready"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("AGENTENV_API_URL", server.URL)
	t.Setenv("AGENTENV_API_KEY", "test-key")
	e2b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("AgentENV 准入请求访问了 E2B API")
		http.Error(w, "wrong API", http.StatusInternalServerError)
	}))
	defer e2b.Close()
	matcher, err := buildMatcher(providers.NewSandboxTransformer(e2b.URL, "e2b-key", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, runtimeClass, template string
	}{
		{"ready-alias", "agentenv", "alias"},
		{"missing", "agentenv", ""},
		{"canonical", "agentenv", "canonical"},
		{"unregistered-runtime", "unregistered", "alias"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"sandbox.cri/template": tc.template}}, Spec: corev1.PodSpec{RuntimeClassName: &tc.runtimeClass}}
			response := admitPod(newPodReview(admissionv1.Create, pod), matcher)
			if !response.Allowed {
				t.Fatalf("准入结果: %+v", response)
			}
			if len(response.Patch) != 0 {
				t.Fatalf("不应修改对象: %s", response.Patch)
			}
		})
	}

	t.Run("batchsandbox", func(t *testing.T) {
		raw := []byte(`{
			"apiVersion": "sandbox.opensandbox.io/v1alpha1",
			"kind": "BatchSandbox",
			"spec": {"template": {
				"metadata": {"annotations": {"sandbox.cri/template": "alias"}},
				"spec": {"runtimeClassName": "agentenv"}
			}}
		}`)
		response := admitBatchSandbox(newAdmissionReview("BatchSandbox", admissionv1.Create, raw), matcher)
		if !response.Allowed {
			t.Fatalf("准入失败: %+v", response)
		}
		if len(response.Patch) != 0 {
			t.Fatalf("不应修改 BatchSandbox: %s", response.Patch)
		}
	})
}

func TestAgentENVProviderDisabledWithoutAPIURL(t *testing.T) {
	t.Setenv("AGENTENV_API_URL", "")
	t.Setenv("AGENTENV_API_KEY", "unused-key")
	matcher, err := buildMatcher(providers.NewSandboxTransformer("http://example.invalid", "e2b-key", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matcher.Resolve("agentenv"); ok {
		t.Fatal("未配置 AgentENV API 时注册了 AgentENV Provider")
	}
	for _, handler := range []string{"e2b", "android"} {
		if _, ok := matcher.Resolve(handler); !ok {
			t.Fatalf("缺少原有 Provider %s", handler)
		}
	}
}
