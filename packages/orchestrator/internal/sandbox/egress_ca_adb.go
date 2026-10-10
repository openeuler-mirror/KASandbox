package sandbox

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Android 信任库文件名沿用 openssl subject_hash_old（MD5），非安全用途
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	// androidCAStoreDir 是 Android system 信任库目录；Android 7+ 应用默认只信
	// 任该目录（用户 CA 对应用不生效），因此 MITM 场景的 CA 必须落到这里。
	androidCAStoreDir = "/system/etc/security/cacerts"

	// egressCAADBBinaryEnv 覆盖 adb 二进制位置（默认 PATH 中的 adb）。
	egressCAADBBinaryEnv = "SANDBOX_ADB_BINARY"

	// egressCAADBReconnectTimeout 约束 adb root 后 adbd 重启的等待：
	// root 切换会断开现有连接，需要重新 connect + wait-for-device。
	egressCAADBReconnectTimeout = 30 * time.Second
)

// egressCAADBRun 执行一条 adb 命令（已带 -s <addr>），返回合并输出。
// 声明为变量以便单测打桩。
var egressCAADBRun = runEgressCAADBCommand

// runEgressCAADBCommand 以 adb 二进制执行命令：adb -s <addr> <args...>。
func runEgressCAADBCommand(ctx context.Context, adbAddr string, args ...string) (string, error) {
	binary := os.Getenv(egressCAADBBinaryEnv)
	if binary == "" {
		binary = "adb"
	}

	full := append([]string{"-s", adbAddr}, args...)
	cmd := exec.CommandContext(ctx, binary, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("adb %s: %w (output: %s)", strings.Join(args, " "), err, utils.Truncate(string(out), 200))
	}

	return string(out), nil
}

// androidCAFileName 计算 Android system 信任库用的 CA 文件名（<subject_hash_old>.0）。
// subject_hash_old = 证书 DER subject 的 MD5，取前 4 字节按小端序格式化为 %08x，
// 与 `openssl x509 -subject_hash_old` 一致。
func androidCAFileName(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("proxy CA cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse proxy CA cert: %w", err)
	}

	sum := md5.Sum(cert.RawSubject) //nolint:gosec // 见函数注释，对齐 openssl 命名约定
	hash := binary.LittleEndian.Uint32(sum[:4])

	return fmt.Sprintf("%08x.0", hash), nil
}

// normalizeADBReadBack 归一化 adb 读回内容：CRLF → LF 并去首尾空白，
// 防止 adb 通道的换行转换造成与本地 certPEM 的假不一致。
func normalizeADBReadBack(b []byte) []byte {
	return bytes.TrimSpace(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) 
}

// adbConnect 建立（或复用）到 adb 地址的连接并等待设备可用。
func adbConnect(ctx context.Context, adbAddr string) error {
	if _, err := egressCAADBRun(ctx, adbAddr, "connect", adbAddr); err != nil {
		return fmt.Errorf("adb connect %s: %w", adbAddr, err)
	}
	if _, err := egressCAADBRun(ctx, adbAddr, "wait-for-device"); err != nil {
		return fmt.Errorf("adb wait-for-device %s: %w", adbAddr, err)
	}

	return nil
}

// adbEnsureRoot 请求 adbd 以 root 重启；adbd 重启会断开连接，因此之后做有界
// 重连。已在 root 态（"already running as root"）时直接返回。生产 user 镜像
// 不允许 root，此时 fail-fast 并指引改用镜像内置 CA。
func adbEnsureRoot(ctx context.Context, adbAddr string) error {
	out, err := egressCAADBRun(ctx, adbAddr, "root")
	if err != nil {
		if strings.Contains(out, "already running as root") {
			return nil
		}

		return fmt.Errorf("adb root: %w（生产 user 镜像不允许 adbd root，请改用模板构建期内置 CA）", err)
	}
	if strings.Contains(out, "already running as root") {
		return nil
	}

	// adbd 正在以 root 重启，连接会断开，做有界重连。
	deadline := time.Now().Add(egressCAADBReconnectTimeout)
	for {
		if _, derr := egressCAADBRun(ctx, adbAddr, "disconnect", adbAddr); derr != nil {
			logger.L().Debug(ctx, "adb disconnect before reconnect failed", zap.Error(derr))
		}
		if err := adbConnect(ctx, adbAddr); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("adb reconnect after root timeout (%s)", egressCAADBReconnectTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("adb reconnect after root aborted: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// injectEgressProxyCAAndroid 把代理 CA cert 注入 Android system 信任库
// （§19.4，经 AndroidServices 的 ADB vsock proxy 通道）：
//  1. 计算 <subject_hash_old>.0 文件名；
//  2. 幂等探测：读回既有文件比对 SHA-256 指纹，一致即跳过；
//  3. adb root（含 adbd 重启后的重连）；
//  4. adb remount 解锁 system 分区；
//  5. push cert 到 /system/etc/security/cacerts/，chmod 0644 + restorecon；
//  6. 读回校验指纹。
//
// 任一步失败即返回错误，由创建路径回滚。信任库由进程启动时加载：注入发生在
// 创建路径（Android 仍在 boot 早期），后续启动的 app 进程自然加载新 CA；
// 已在运行的 framework 进程不刷新（已知限制，与 Linux 侧轮转语义一致）。
func injectEgressProxyCAAndroid(ctx context.Context, adbAddr string, certPEM []byte) error {
	wantFP := sha256.Sum256(certPEM)

	fileName, err := androidCAFileName(certPEM)
	if err != nil {
		return err
	}
	target := androidCAStoreDir + "/" + fileName

	if err := adbConnect(ctx, adbAddr); err != nil {
		return err
	}

	// ② 指纹幂等探测（读失败视为未安装，继续注入）。用 exec-out 而非
	// shell cat：exec-out 不做 PTY 换行转换，字节级保真。
	if existing, err := egressCAADBRun(ctx, adbAddr, "exec-out", "cat", target); err == nil {
		if bytes.Equal(normalizeADBReadBack([]byte(existing)), bytes.TrimSpace(certPEM)) {
			logger.L().Info(ctx, "android guest already trusts the egress proxy CA (fingerprint match), skipping injection",
				zap.String("fingerprint", hex.EncodeToString(wantFP[:])),
				zap.String("target", target),
			)

			return nil
		}
	}

	// ③④ root + remount 解锁 system 分区。
	if err := adbEnsureRoot(ctx, adbAddr); err != nil {
		return err
	}
	if out, err := egressCAADBRun(ctx, adbAddr, "remount"); err != nil {
		return fmt.Errorf("adb remount: %w", err)
	} else if strings.Contains(out, "remount failed") {
		return fmt.Errorf("adb remount failed: %s", utils.Truncate(out, 200))
	}

	// ⑤ 写入 cert（经宿主临时文件 adb push）。
	tmp, err := os.CreateTemp("", "e2b-egress-ca-*.pem")
	if err != nil {
		return fmt.Errorf("create temp CA file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(certPEM); err != nil {
		tmp.Close()

		return fmt.Errorf("write temp CA file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp CA file: %w", err)
	}
	if _, err := egressCAADBRun(ctx, adbAddr, "push", tmp.Name(), target); err != nil {
		return fmt.Errorf("adb push CA cert: %w", err)
	}
	if _, err := egressCAADBRun(ctx, adbAddr, "shell", "chmod", "0644", target); err != nil {
		return fmt.Errorf("chmod CA cert: %w", err)
	}
	// SELinux enforcing 下新文件需恢复 system_file 标签，否则域内进程读不到。
	if _, err := egressCAADBRun(ctx, adbAddr, "shell", "restorecon", target); err != nil {
		return fmt.Errorf("restorecon CA cert: %w", err)
	}

	// ⑥ 读回校验（exec-out + 换行归一化，防止 PTY 转换造成假失败）。
	wrote, err := egressCAADBRun(ctx, adbAddr, "exec-out", "cat", target)
	if err != nil {
		return fmt.Errorf("verify CA cert read-back: %w", err)
	}
	if !bytes.Equal(normalizeADBReadBack([]byte(wrote)), bytes.TrimSpace(certPEM)) {
		return fmt.Errorf("verify CA cert read-back: content mismatch")
	}

	logger.L().Info(ctx, "egress proxy CA injected into android system trust store",
		zap.String("fingerprint", hex.EncodeToString(wantFP[:])),
		zap.String("target", target),
	)

	return nil
}
