package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	// egressCAGuestCertPath 是 guest 内 CA cert 的落盘路径（debian 系
	// update-ca-certificates 约定目录，只拾取 .crt 扩展名）。
	// egressCAGuestTrustedPath 是其进入系统信任库后的校验路径——
	// update-ca-certificates 会把 <name>.crt 重命名为 <name>.pem 落进
	// /etc/ssl/certs，因此这里必须是 .pem（e2e 实测：写成 .crt 恒失败）。
	egressCAGuestCertPath    = "/usr/local/share/ca-certificates/e2b-egress-ca.crt"
	egressCAGuestTrustedPath = "/etc/ssl/certs/e2b-egress-ca.pem"

	// egressCAEnvdUser 是注入写文件与命令执行的 guest 用户（信任库目录只有
	// root 可写）。
	egressCAEnvdUser = "root"

	// egressCAInjectTimeout 约束整个注入序列（读/写文件 + 两条 guest 命令）。
	egressCAInjectTimeout = 60 * time.Second
)

// egressProxyCANeedsInjection 判定该沙箱是否需要 guest CA 注入（§7.4.2）：
// 节点开了 SANDBOX_PROXY_CA_AUTO + 沙箱解析为 per-sandbox + egress-mitm 未显式
// 关闭。egress-mitm=false 的沙箱不解密，不需要 CA 信任，整体短路跳过。
func egressProxyCANeedsInjection(netCfg network.Config, egressMode network.EgressMode, identity map[string]string) bool {
	return netCfg.ProxyCAAutoEnabled() &&
		egressMode == network.EgressModePerSandbox &&
		identity[network.MetadataKeyEgressMitm] != "false"
}

// injectEgressProxyCA 把 confdir 的 CA cert 注入 guest 系统信任库
// （§7.4.2 注入序列，以沙箱的 envd 为通道）：
//  1. 读 guest 内既有 cert 计算 SHA256 指纹，与宿主 cert 一致则跳过写入与
//     update-ca-certificates（快照恢复/重试幂等）；
//  2. 经 envd 写 /usr/local/share/ca-certificates/e2b-egress-ca.crt；
//  3. 执行 update-ca-certificates；
//  4. 校验 openssl x509 -in /etc/ssl/certs/e2b-egress-ca.pem -noout -subject。
//
// 任一步失败即返回错误，由创建路径回滚。
func (s *Sandbox) injectEgressProxyCA(ctx context.Context, netCfg network.Config) error {
	certPEM, err := os.ReadFile(network.ProxyCACertPath(netCfg.SandboxProxyConfDir))
	if err != nil {
		return fmt.Errorf("read proxy CA cert from confdir: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", s.Slot.HostIPString(), consts.DefaultEnvdServerPort)
	accessToken := utils.DerefOrDefault(s.Config.Envd.AccessToken, "")

	injectCtx, cancel := context.WithTimeout(ctx, egressCAInjectTimeout)
	defer cancel()

	return injectEgressProxyCA(injectCtx, baseURL, accessToken, certPEM)
}

// injectEgressProxyCA 是注入序列的纯函数实现（envd 地址直连形态），与 Sandbox
// 解耦以便 httptest 假 envd 单测。
func injectEgressProxyCA(ctx context.Context, envdBaseURL, accessToken string, certPEM []byte) error {
	wantFP := sha256.Sum256(certPEM)

	// ① 指纹幂等检查：guest 已有同指纹 cert → 跳过写入与信任库刷新。
	existing, status, err := envdReadFile(ctx, envdBaseURL, accessToken, egressCAGuestCertPath)
	if err != nil {
		return fmt.Errorf("read existing guest CA cert: %w", err)
	}
	if status == http.StatusOK && sha256.Sum256(existing) == wantFP {
		logger.L().Info(ctx, "guest already trusts the egress proxy CA (fingerprint match), skipping injection",
			zap.String("fingerprint", hex.EncodeToString(wantFP[:])),
		)

		return nil
	}

	// ② 写入 cert。
	if err := envdWriteFile(ctx, envdBaseURL, accessToken, egressCAGuestCertPath, certPEM); err != nil {
		return fmt.Errorf("write guest CA cert: %w", err)
	}

	// ③ 刷新系统信任库。
	if _, err := runEgressCAGuestCommand(ctx, envdBaseURL, accessToken, "update-ca-certificates"); err != nil {
		return fmt.Errorf("update-ca-certificates failed: %w", err)
	}

	// ④ 校验信任库内的 cert 可解析。
	subject, err := runEgressCAGuestCommand(ctx, envdBaseURL, accessToken,
		"openssl x509 -in "+egressCAGuestTrustedPath+" -noout -subject")
	if err != nil {
		return fmt.Errorf("verify trusted guest CA cert failed: %w", err)
	}

	logger.L().Info(ctx, "egress proxy CA injected into guest trust store",
		zap.String("fingerprint", hex.EncodeToString(wantFP[:])),
		zap.String("subject", strings.TrimSpace(subject)),
	)

	return nil
}

// envdReadFile 经 envd REST GET /files 读 guest 文件。返回 (内容, 状态码, 错误)；
// 404 表示文件不存在（返回 nil 内容，不算错误）。
func envdReadFile(ctx context.Context, envdBaseURL, accessToken, path string) ([]byte, int, error) {
	u := fmt.Sprintf("%s/files?path=%s&username=%s", envdBaseURL, url.QueryEscape(path), egressCAEnvdUser)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if accessToken != "" {
		req.Header.Set("X-Access-Token", accessToken)
	}

	resp, err := sandboxHttpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	return body, resp.StatusCode, nil
}

// envdWriteFile 经 envd REST POST /files（multipart）写 guest 文件（已存在则覆盖，
// 父目录自动创建）。
func envdWriteFile(ctx context.Context, envdBaseURL, accessToken, path string, content []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "e2b-egress-ca.crt")
	if err != nil {
		return fmt.Errorf("create multipart form file: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("write multipart content: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	u := fmt.Sprintf("%s/files?path=%s&username=%s", envdBaseURL, url.QueryEscape(path), egressCAEnvdUser)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if accessToken != "" {
		req.Header.Set("X-Access-Token", accessToken)
	}

	resp, err := sandboxHttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, utils.Truncate(string(respBody), 200))
	}

	return nil
}

// runEgressCAGuestCommand 经 envd connect-RPC process 服务在 guest 内以 root
// 执行命令并等待结束；非零退出码返回错误（含 stderr 摘要）。返回 stdout。
func runEgressCAGuestCommand(ctx context.Context, envdBaseURL, accessToken, command string) (string, error) {
	client := processconnect.NewProcessClient(&http.Client{
		Timeout:   egressCAInjectTimeout,
		Transport: SandboxHttpTransport,
	}, envdBaseURL)

	req := connect.NewRequest(&process.StartRequest{
		Process: &process.ProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-l", "-c", command},
		},
	})
	if accessToken != "" {
		req.Header().Set("X-Access-Token", accessToken)
	}
	// envd 的 BasicAuth 用户名头（user: 空密码），注入命令必须以 root 运行。
	grpc.SetUserHeader(req.Header(), egressCAEnvdUser)

	stream, err := client.Start(ctx, req)
	if err != nil {
		return "", fmt.Errorf("start guest command %q: %w", command, err)
	}
	defer stream.Close()

	var stdout, stderr strings.Builder
	for stream.Receive() {
		event := stream.Msg().GetEvent()
		if event == nil {
			continue
		}
		if data := event.GetData(); data != nil {
			stdout.WriteString(string(data.GetStdout()))
			stderr.WriteString(string(data.GetStderr()))
		}
		if end := event.GetEnd(); end != nil {
			if end.GetExitCode() != 0 {
				return "", fmt.Errorf("guest command %q exited %d (%s): %s",
					command, end.GetExitCode(), end.GetStatus(), utils.Truncate(stderr.String(), 200))
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("guest command %q stream: %w", command, err)
	}

	return stdout.String(), nil
}
