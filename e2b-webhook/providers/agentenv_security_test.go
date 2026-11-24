package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentENVResolverRejectsUnsafeBaseURL(t *testing.T) {
	for _, address := range []string{"ftp://example.invalid", "http:///missing", "http://user:secret@example.invalid", "http://example.invalid?token=secret", "http://example.invalid#fragment"} {
		if _, err := NewAgentENVTemplateResolver(address, "", time.Second); err == nil {
			t.Fatalf("接受了不安全 URL: %s", address)
		}
	}
}

func TestAgentENVResolverDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	r, _ := NewAgentENVTemplateResolver(origin.URL, "test-key", time.Second)
	if _, err := r.ResolveTemplate(context.Background(), "alias"); err == nil {
		t.Fatal("重定向应失败")
	}
	if calls.Load() != 0 {
		t.Fatal("向重定向目标发送了请求")
	}
}

func TestAgentENVResolverRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"unauthorized", 401, "private-response"},
		{"unavailable", 503, "private-response"},
		{"malformed", 200, "{"},
		{"oversized", 200, strings.Repeat("x", agentENVTemplateResponseLimit+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.Method != http.MethodGet || req.Header.Get("X-API-Key") != "test-key" {
					t.Error("模板解析必须是携带鉴权的只读请求")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			r, _ := NewAgentENVTemplateResolver(server.URL, "test-key", time.Second)
			_, err := r.ResolveTemplate(context.Background(), "alias")
			if err == nil || strings.Contains(err.Error(), "private-response") || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("错误或信息泄露: %v", err)
			}
			if calls != 1 {
				t.Fatalf("异常后不应回退或重试: %d", calls)
			}
		})
	}
}

func TestAgentENVResolverCancellationAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	r, _ := NewAgentENVTemplateResolver(server.URL, "", 20*time.Millisecond)
	if _, err := r.ResolveTemplate(context.Background(), "alias"); err == nil {
		t.Fatal("等待响应应超时")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ResolveTemplate(ctx, "alias"); err == nil {
		t.Fatal("已取消请求不应继续解析")
	}
}

func TestAgentENVResolverSharesTimeoutAcrossRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		if strings.Contains(r.URL.Path, "/aliases/") {
			_, _ = w.Write([]byte(`{"templateID":"canonical"}`))
		} else {
			_, _ = w.Write([]byte(`{"templateID":"canonical","builds":[{"buildID":"ready","status":"ready"}]}`))
		}
	}))
	defer server.Close()
	r, _ := NewAgentENVTemplateResolver(server.URL, "", 150*time.Millisecond)
	// 每个请求单独都能在预算内完成，但完整解析不能获得两份预算。
	if _, err := r.ResolveTemplate(context.Background(), "alias"); err == nil {
		t.Fatal("多个模板请求未共享超时预算")
	}
}
