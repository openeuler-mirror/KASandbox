package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/e2b/e2b-webhook/provider"
)

const (
	agentENVProviderName = "agentenv"

	agentENVTemplateRefMaxLen     = 512
	agentENVTemplateResponseLimit = 1 << 20
	defaultAgentENVResolveTimeout = 2 * time.Second
)

// TemplateResolver 将模板别名或 ID 解析为 Runtime 使用的模板 ID。
type TemplateResolver interface {
	ResolveTemplate(ctx context.Context, templateRef string) (string, error)
}

// AgentENVProvider 将 RuntimeClass 命中的对象解析为 AgentENV 注入注解。
type AgentENVProvider struct {
	resolver TemplateResolver
}

// NewAgentENVProvider 创建 AgentENV Provider；resolver 不能为空。
func NewAgentENVProvider(resolver TemplateResolver) *AgentENVProvider {
	return &AgentENVProvider{resolver: resolver}
}

func (p *AgentENVProvider) Name() string    { return agentENVProviderName }
func (p *AgentENVProvider) Handler() string { return agentENVProviderName }

// Resolve 只读取 sandbox.cri/template，不读取 TEMPLATE_NAME、image 或 Pod 名称。
func (p *AgentENVProvider) Resolve(ctx context.Context, req provider.Request) (*provider.Result, error) {
	if p == nil || p.resolver == nil {
		return nil, fmt.Errorf("agentenv template resolver is not configured")
	}
	rawTemplate := req.Annotations[provider.AnnTemplate]
	templateRef := strings.TrimSpace(rawTemplate)
	if templateRef == "" {
		return nil, fmt.Errorf("annotation %s is required", provider.AnnTemplate)
	}
	canonicalID, err := p.resolver.ResolveTemplate(ctx, templateRef)
	if err != nil {
		return nil, err
	}
	canonicalID = strings.TrimSpace(canonicalID)
	if canonicalID == "" || len(canonicalID) > agentENVTemplateRefMaxLen {
		return nil, fmt.Errorf("resolved template ID must be 1..%d bytes", agentENVTemplateRefMaxLen)
	}

	result := &provider.Result{RuntimeClass: agentENVProviderName}
	if rawTemplate != canonicalID {
		result.Annotations = map[string]string{provider.AnnTemplate: canonicalID}
	}
	return result, nil
}

// AgentENVTemplateResolver 通过 AgentENV 只读模板 API 解析模板。
type AgentENVTemplateResolver struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAgentENVTemplateResolver 创建只读模板解析客户端。
func NewAgentENVTemplateResolver(apiURL, apiKey string, timeout time.Duration) (*AgentENVTemplateResolver, error) {
	parsed, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil {
		return nil, fmt.Errorf("parse template API URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("template API URL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("template API URL must not contain userinfo, query or fragment")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("template API URL host is required")
	}
	if timeout <= 0 {
		timeout = defaultAgentENVResolveTimeout
	}

	return &AgentENVTemplateResolver{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: timeout,
			// 不跟随重定向，避免 API key 被转发到未授权主机。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// ResolveTemplate 先解析 alias，再校验 build 状态并返回 canonical 身份。
func (r *AgentENVTemplateResolver) ResolveTemplate(ctx context.Context, templateRef string) (string, error) {
	if r == nil || r.client == nil {
		return "", fmt.Errorf("template resolver is not configured")
	}
	// 一次解析共用超时预算，避免多个只读请求累计超过准入超时。
	ctx, cancel := context.WithTimeout(ctx, r.client.Timeout)
	defer cancel()
	templateRef = strings.TrimSpace(templateRef)
	if templateRef == "" || len(templateRef) > agentENVTemplateRefMaxLen {
		return "", fmt.Errorf("template reference must be 1..%d characters", agentENVTemplateRefMaxLen)
	}

	canonicalTemplateID, err := r.resolveTemplateID(ctx, templateRef)
	if err != nil {
		return "", err
	}
	template, err := r.getTemplate(ctx, canonicalTemplateID)
	if err != nil {
		return "", err
	}
	if template.TemplateID != "" {
		canonicalTemplateID = strings.TrimSpace(template.TemplateID)
	}
	if canonicalTemplateID == "" {
		return "", fmt.Errorf("template API returned an empty template ID")
	}

	if err := validateAgentENVLatestBuild(template.Builds); err != nil {
		return "", err
	}
	return canonicalTemplateID, nil
}

func (r *AgentENVTemplateResolver) resolveTemplateID(ctx context.Context, templateRef string) (string, error) {
	var alias agentENVTemplateAliasResponse
	status, err := r.getJSON(ctx, "/templates/aliases/"+url.PathEscape(templateRef), &alias)
	if err == nil && status == http.StatusOK {
		canonicalID := strings.TrimSpace(alias.TemplateID)
		if canonicalID == "" {
			return "", fmt.Errorf("template alias API returned an empty templateID")
		}
		return canonicalID, nil
	}
	if status != http.StatusNotFound && err != nil {
		return "", err
	}

	// alias 不存在时按直接 template ID 查询；不使用任何本地推断规则。
	template, directErr := r.getTemplate(ctx, templateRef)
	if directErr != nil {
		return "", directErr
	}
	canonicalID := strings.TrimSpace(template.TemplateID)
	if canonicalID == "" {
		canonicalID = templateRef
	}
	return canonicalID, nil
}

func (r *AgentENVTemplateResolver) getTemplate(ctx context.Context, templateRef string) (agentENVTemplateWithBuilds, error) {
	var template agentENVTemplateWithBuilds
	status, err := r.getJSON(ctx, "/templates/"+url.PathEscape(templateRef), &template)
	if err != nil {
		return agentENVTemplateWithBuilds{}, err
	}
	if status != http.StatusOK {
		return agentENVTemplateWithBuilds{}, fmt.Errorf("template API returned HTTP %d", status)
	}
	return template, nil
}

func (r *AgentENVTemplateResolver) getJSON(ctx context.Context, path string, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("create template API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("X-API-Key", r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call template API: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, agentENVTemplateResponseLimit+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read template API response: %w", err)
	}
	if len(body) > agentENVTemplateResponseLimit {
		return resp.StatusCode, fmt.Errorf("template API response exceeds %d bytes", agentENVTemplateResponseLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("template API returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return resp.StatusCode, fmt.Errorf("decode template API response: %w", err)
	}
	return resp.StatusCode, nil
}

type agentENVTemplateAliasResponse struct {
	TemplateID string `json:"templateID"`
}

type agentENVTemplateWithBuilds struct {
	TemplateID string                  `json:"templateID"`
	Builds     []agentENVTemplateBuild `json:"builds"`
}

type agentENVTemplateBuild struct {
	BuildID   string    `json:"buildID"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

func validateAgentENVLatestBuild(builds []agentENVTemplateBuild) error {
	if len(builds) == 0 {
		return fmt.Errorf("template has no builds")
	}
	latest := builds[0]
	for _, build := range builds[1:] {
		if build.CreatedAt.After(latest.CreatedAt) {
			latest = build
		}
	}
	if !strings.EqualFold(strings.TrimSpace(latest.Status), "ready") {
		return fmt.Errorf("latest template build %s is not ready", latest.BuildID)
	}
	if strings.TrimSpace(latest.BuildID) == "" {
		return fmt.Errorf("template API returned an empty build ID")
	}
	return nil
}
