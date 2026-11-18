package main

import (
	"encoding/json"
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
	matcher, err := buildMatcher(providers.NewSandboxTransformer(server.URL, "test-key", time.Second), server.URL, "test-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, runtimeClass, template, canonical string
		allowed, patched                        bool
	}{
		{"ready", "agentenv", "alias", "", true, true},
		{"missing", "agentenv", "", "", true, false},
		{"mismatch", "agentenv", "alias", "wrong", true, false},
		{"unregistered-cube", "cube", "alias", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"sandbox.cri/template": tc.template}}, Spec: corev1.PodSpec{RuntimeClassName: &tc.runtimeClass}}
			if tc.canonical != "" {
				pod.Annotations["aenv.dev/template-id"] = tc.canonical
			}
			response := admitPod(newPodReview(admissionv1.Create, pod), matcher)
			if response.Allowed != tc.allowed {
				t.Fatalf("准入结果: %+v", response)
			}
			if !tc.patched {
				if len(response.Patch) != 0 {
					t.Fatalf("不应修改对象: %s", response.Patch)
				}
				return
			}
			var patches []patchOperation
			if err := json.Unmarshal(response.Patch, &patches); err != nil {
				t.Fatal(err)
			}
			found := map[string]interface{}{}
			for _, patch := range patches {
				found[patch.Path] = patch.Value
			}
			if found["/metadata/annotations/aenv.dev~1template-id"] != "canonical" || found["/metadata/annotations/sandbox.cri~1build"] != "ready-build" {
				t.Fatalf("注解错误: %s", response.Patch)
			}
		})
	}
}
