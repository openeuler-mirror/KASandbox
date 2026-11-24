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
// 注解里已经有 template-id、vcpu、ram-mb、disk-mb 时不再请求 CubeAPI。
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
//
// disk-mb 必须与模板可写层一致，否则 cubelet 对快照 rootfs 做 resize 会
// invalid_argument: '...-rootfs-gen0' is a snapshot; cannot resize。
// resolve 当前只回 id/vcpu/ram；缺 disk 时再 GET /templates/{id} 从
// createRequest.annotations["cube.master.rootfs.writable_layer_size"]（或 volume size_limit）补齐。
func (p *CubeProvider) Resolve(ctx context.Context, req provider.Request) (*provider.Result, error) {
	annotatedID := annotation(req, annCubeTemplate)
	vcpu := annotation(req, annCubeVCPU)
	ram := annotation(req, annCubeRAM)
	disk := annotation(req, annCubeDisk)
	templateID := annotatedID
	needSpec := annotatedID == "" || vcpu == "" || ram == ""
	needDisk := disk == ""

	if needSpec || needDisk {
		template := firstNonEmpty(req.Template, annotatedID)
		if template == "" {
			return &provider.Result{RuntimeClass: p.Handler()}, nil
		}
		if annotatedID == "" {
			templateID = template
		}
		if needSpec {
			resolved, err := p.resolveTemplate(ctx, template)
			if err != nil {
				return nil, err
			}
			templateID = resolved.TemplateID
			vcpu = fmt.Sprintf("%d", resolved.VCPU)
			ram = fmt.Sprintf("%d", resolved.RAMMB)
			if disk == "" && resolved.DiskMB != "" {
				disk = resolved.DiskMB
			}
		}
		if disk == "" {
			got, err := p.diskFromTemplate(ctx, firstNonEmpty(templateID, template))
			if err != nil {
				return nil, err
			}
			disk = got
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
	// DiskMB 可选：现行 CubeAPI resolve 不返回；若后续补齐则直接用，免二次 GET。
	DiskMB string `json:"disk_mb,omitempty"`
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
	if out.DiskMB == "" {
		var raw map[string]json.RawMessage
		if json.Unmarshal(payload, &raw) == nil {
			if b, ok := raw["disk_mb"]; ok {
				out.DiskMB = normalizeDiskQuantity(b)
			}
		}
	}
	if out.TemplateID == "" || out.VCPU <= 0 || out.RAMMB <= 0 {
		return resolveResponse{}, fmt.Errorf("cube resolve returned an incomplete spec")
	}
	return out, nil
}

func (p *CubeProvider) diskFromTemplate(ctx context.Context, templateID string) (string, error) {
	if p.apiURL == "" {
		return "", fmt.Errorf("cube template %q is missing disk-mb and CUBE_API_URL is unset", templateID)
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return "", fmt.Errorf("cube template id is required to resolve disk-mb")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL+"/templates/"+templateID, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cube get template %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	disk, err := diskFromTemplatePayload(payload)
	if err != nil {
		return "", fmt.Errorf("cube template %q: %w", templateID, err)
	}
	return disk, nil
}

func diskFromTemplatePayload(payload []byte) (string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return "", err
	}
	crRaw, ok := root["createRequest"]
	if !ok {
		crRaw, ok = root["create_request"]
	}
	if !ok {
		return "", fmt.Errorf("createRequest missing; cannot derive disk-mb")
	}
	var cr map[string]json.RawMessage
	if err := json.Unmarshal(crRaw, &cr); err != nil {
		return "", err
	}
	if annRaw, ok := cr["annotations"]; ok {
		var anns map[string]string
		if json.Unmarshal(annRaw, &anns) == nil {
			if v := strings.TrimSpace(anns["cube.master.rootfs.writable_layer_size"]); v != "" {
				return v, nil
			}
		}
	}
	if volRaw, ok := cr["volumes"]; ok {
		var volumes []map[string]json.RawMessage
		if json.Unmarshal(volRaw, &volumes) == nil {
			for _, vol := range volumes {
				if disk := sizeLimitFromVolume(vol); disk != "" {
					return disk, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no writable_layer_size or volume size_limit for disk-mb")
}

func sizeLimitFromVolume(vol map[string]json.RawMessage) string {
	for _, key := range []string{"volume_source", "volumeSource"} {
		srcRaw, ok := vol[key]
		if !ok {
			continue
		}
		var src map[string]json.RawMessage
		if json.Unmarshal(srcRaw, &src) != nil {
			continue
		}
		for _, edKey := range []string{"empty_dir", "emptyDir"} {
			edRaw, ok := src[edKey]
			if !ok {
				continue
			}
			var ed map[string]json.RawMessage
			if json.Unmarshal(edRaw, &ed) != nil {
				continue
			}
			for _, limKey := range []string{"size_limit", "sizeLimit"} {
				if limRaw, ok := ed[limKey]; ok {
					if v := normalizeDiskQuantity(limRaw); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

func normalizeDiskQuantity(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, "\"") {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return strings.TrimSpace(str)
		}
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	return strings.Trim(s, `"`)
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
