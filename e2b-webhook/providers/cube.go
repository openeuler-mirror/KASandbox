package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/e2b/e2b-webhook/provider"
)

const (
	annCubeTemplate = "cube.dev/template-id"
	annCubeVCPU     = "cube.dev/vcpu"
	annCubeRAM      = "cube.dev/ram-mb"
	annCubeDisk     = "cube.dev/disk-mb"
)

// CubeProvider 把模板解析成 cube.dev/* 注解。
// 注解里已经有 template-id、vcpu、ram-mb 时不再请求 CubeAPI。
type CubeProvider struct {
	apiURL string
	client *http.Client
}

// NewCubeProvider 创建 cube Provider。apiURL 为空时只接受调用方已经写好的规格注解。
func NewCubeProvider(apiURL string) *CubeProvider {
	return &CubeProvider{
		apiURL: strings.TrimRight(strings.TrimSpace(apiURL), "/"),
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (p *CubeProvider) Name() string    { return "cube" }
func (p *CubeProvider) Handler() string { return "cube" }

// Resolve 只产出 cube.dev/*（与 engine coldCreate 一致，不双写 sandbox.cri/*）。
// 没有模板时只建议 runtimeClass。
func (p *CubeProvider) Resolve(ctx context.Context, req provider.Request) (*provider.Result, error) {
	annotatedID := annotation(req, annCubeTemplate)
	vcpu := annotation(req, annCubeVCPU)
	ram := annotation(req, annCubeRAM)
	disk := annotation(req, annCubeDisk)
	templateID := annotatedID
	if annotatedID == "" || vcpu == "" || ram == "" {
		template := firstNonEmpty(req.Template, annotatedID)
		if template == "" {
			return &provider.Result{RuntimeClass: p.Handler()}, nil
		}
		if annotatedID == "" {
			templateID = template
		}
		if vcpu == "" || ram == "" {
			resolved, err := p.resolveTemplate(ctx, template)
			if err != nil {
				return nil, err
			}
			templateID = resolved.TemplateID
			vcpu = fmt.Sprintf("%d", resolved.VCPU)
			ram = fmt.Sprintf("%d", resolved.RAMMB)
		}
	}
	annotations := map[string]string{
		annCubeTemplate: templateID,
		annCubeVCPU:     vcpu,
		annCubeRAM:      ram,
	}
	if disk != "" {
		annotations[annCubeDisk] = disk
	}
	return &provider.Result{Annotations: annotations, RuntimeClass: p.Handler()}, nil
}

type resolveResponse struct {
	TemplateID string `json:"template_id"`
	VCPU       int32  `json:"vcpu"`
	RAMMB      int32  `json:"ram_mb"`
}

func (p *CubeProvider) resolveTemplate(ctx context.Context, template string) (resolveResponse, error) {
	if p.apiURL == "" {
		return resolveResponse{}, fmt.Errorf("cube template %q is missing vcpu/ram and CUBE_API_URL is unset", template)
	}
	body, err := json.Marshal(map[string]string{"template": template})
	if err != nil {
		return resolveResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/templates/resolve", bytes.NewReader(body))
	if err != nil {
		return resolveResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return resolveResponse{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resolveResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return resolveResponse{}, fmt.Errorf("cube resolve %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	var out resolveResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return resolveResponse{}, err
	}
	if out.TemplateID == "" || out.VCPU <= 0 || out.RAMMB <= 0 {
		return resolveResponse{}, fmt.Errorf("cube resolve returned an incomplete spec")
	}
	return out, nil
}

func annotation(req provider.Request, key string) string {
	if req.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(req.Annotations[key])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
