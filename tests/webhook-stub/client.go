package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal HTTP wrapper around the E2B API internal endpoints.
// Every request carries the X-Admin-Token header unless explicitly overridden.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Response holds a decoded HTTP exchange for assertions and error reporting.
type Response struct {
	Status int
	Body   []byte
}

// do issues one request. tokenOverride:
//   - nil          -> use the configured admin token
//   - &""          -> send no X-Admin-Token header
//   - &"anything"  -> send that value
func (c *Client) do(method, path string, body any, tokenOverride *string) (*Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	token := c.token
	if tokenOverride != nil {
		token = *tokenOverride
	}
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}

	// 打印请求：方法、路径、token 标记（不打印值）、请求体
	tokenMark := "none"
	if tokenOverride == nil && c.token != "" {
		tokenMark = "configured"
	} else if tokenOverride != nil && *tokenOverride != "" {
		tokenMark = "override"
	}
	fmt.Printf("    --> %s %s (token: %s)\n", method, path, tokenMark)
	if body != nil {
		fmt.Printf("        request: %s\n", prettyJSON(readerBytes(body)))
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 打印响应：状态码 + 响应体（空体标注）
	if len(raw) == 0 {
		fmt.Printf("    <-- %d (empty body)\n", resp.StatusCode)
	} else {
		fmt.Printf("    <-- %d %s\n", resp.StatusCode, prettyJSON(raw))
	}

	return &Response{Status: resp.StatusCode, Body: raw}, nil
}

// readerBytes 与 do 中 marshal 逻辑保持一致，用于打印请求体。
func readerBytes(body any) []byte {
	raw, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf("<marshal error: %v>", err))
	}
	return raw
}

// prettyJSON 把 JSON 压缩打印为单行；非 JSON 原样输出。
func prettyJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

// get/post/del use the configured admin token.
func (c *Client) get(path string) (*Response, error)  { return c.do(http.MethodGet, path, nil, nil) }
func (c *Client) post(path string, body any) (*Response, error) {
	return c.do(http.MethodPost, path, body, nil)
}
func (c *Client) del(path string) (*Response, error) { return c.do(http.MethodDelete, path, nil, nil) }

func (r *Response) decode(v any) error {
	if len(r.Body) == 0 {
		return fmt.Errorf("empty response body")
	}
	return json.Unmarshal(r.Body, v)
}

// ---- DTOs (field names verified against packages/api/internal/api/api.gen.go) ----

type pauseSnapshotResponse struct {
	TemplateID string `json:"template_id"`
	BuildID    string `json:"build_id"`
}

type snapshotTemplateResponse struct {
	SnapshotID string `json:"snapshot_id"`
	TemplateID string `json:"template_id"`
	BuildID    string `json:"build_id"`
}

type snapshotStateResponse struct {
	Paused     bool    `json:"paused"`
	TemplateID *string `json:"template_id"`
	BuildID    *string `json:"build_id"`
}

type resumeMetadata struct {
	TemplateID         string            `json:"template_id"`
	BuildID            string            `json:"build_id"`
	BaseTemplateID     *string           `json:"base_template_id"`
	OriginNode         string            `json:"origin_node"`
	Vcpu               int64             `json:"vcpu"`
	RamMb              int64             `json:"ram_mb"`
	TotalDiskSizeMb    *int64            `json:"total_disk_size_mb"`
	KernelVersion      string            `json:"kernel_version"`
	FirecrackerVersion string            `json:"firecracker_version"`
	EnvdVersion        *string           `json:"envd_version"`
	EnvSecure          bool              `json:"env_secure"`
	Metadata           map[string]string `json:"metadata"`
}
