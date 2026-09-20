package network

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// mitmproxy confdir 布局（§7.4.1，与 §7.3 手动产物同构）：
//   - proxyCACertFile 纯 cert（0644），guest 注入的输入；
//   - proxyCACombinedFile cert+key 合并（0600，mitmproxy confdir 约定格式），
//     代理进程签发落叶证书的输入。
const (
	proxyCACertFile     = "mitmproxy-ca-cert.pem"
	proxyCACombinedFile = "mitmproxy-ca.pem"

	// proxyCACN 自动生成 CA 的 CommonName（与 §7.3 手动 CA 的 CN 区分开，
	// 便于 guest 内 openssl 输出直接辨认来源）。
	proxyCACN = "e2b-egress-auto-ca"

	// proxyCAValidityYears 自动生成 CA 的有效期（年）。
	proxyCAValidityYears = 10
)

// ProxyCACertPath 返回 confdir 内纯 cert 文件的路径（guest 注入的输入）。
func ProxyCACertPath(confdir string) string {
	return filepath.Join(confdir, proxyCACertFile)
}

// ensureProxyCA 保证 confdir 内存在可用的代理 CA 材料（SANDBOX_PROXY_CA_AUTO=true
// 时由 ParseConfig 调用）。幂等：两文件均存在、可解析且 cert 未过期即跳过；
// 半文件状态可补全（cert 文件缺失时从合并 PEM 重导出）；其余缺失/损坏/过期
// 一律重新生成。任何写入失败 fail-fast（可预见的 MITM 全断比启动失败更难排查）。
//
// 单实例前提：一个节点只有一个 orchestrator 实例，本函数只运行在 ParseConfig
// 启动路径（单线程），无多进程竞争，不需要文件锁；tmp+rename 原子写仅为防
// 进程崩溃留下半文件。不自动轮换：轮换 = 运维删除 confdir 文件 + 滚动重启
// （存量沙箱信任的是旧 CA，重生成会导致存量 MITM 全断）。
func ensureProxyCA(confdir string) error {
	combinedPath := filepath.Join(confdir, proxyCACombinedFile)
	certPath := filepath.Join(confdir, proxyCACertFile)

	combinedPEM, combinedErr := os.ReadFile(combinedPath)
	combinedOK := combinedErr == nil && proxyCACombinedValid(combinedPEM)
	certPEM, certErr := os.ReadFile(certPath)
	certOK := certErr == nil && proxyCACertValid(certPEM)

	switch {
	case combinedOK && certOK:
		// 幂等跳过：重启/滚动不重生成。
		return nil
	case combinedOK:
		// 半文件：合并 PEM 在、纯 cert 缺失/损坏——cert 可从合并 PEM 重导出。
		return writeFileAtomic(certPath, proxyCACertBlock(combinedPEM), 0o644)
	default:
		// 全缺、损坏、过期、或只有纯 cert（key 不可恢复）——重新生成整套。
		return generateProxyCA(confdir)
	}
}

// generateProxyCA 生成 RSA 2048 自签根 CA 并原子写入 confdir 的两个约定文件。
func generateProxyCA(confdir string) error {
	if err := os.MkdirAll(confdir, 0o755); err != nil {
		return fmt.Errorf("create proxy CA confdir %s: %w", confdir, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate proxy CA key: %w", err)
	}

	// 随机 128-bit 序列号。
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate proxy CA serial: %w", err)
	}

	now := time.Now()
	// SubjectKeyId 显式采用 RFC 5280 §4.2.1.2 method 1（SHA-1 over PKCS#1
	// DER），与 openssl / mitmproxy 从 issuer 公钥推导 AuthorityKeyIdentifier
	// 的口径一致。Go 1.25+ 对 CA 默认按 RFC 7093 用 SHA-256 截断生成 SKID，
	// 会导致 mitmproxy 伪造落叶证书的 AKID（method 1）与 CA SKID 不匹配，
	// openssl3 客户端严格校验时报
	// "authority and subject key identifier mismatch"（e2e 实测）。
	skid := sha1.Sum(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: proxyCACN},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(proxyCAValidityYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          skid[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create proxy CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	combinedPEM := append(certPEM, keyPEM...)

	if err := writeFileAtomic(filepath.Join(confdir, proxyCACombinedFile), combinedPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(confdir, proxyCACertFile), certPEM, 0o644); err != nil {
		return err
	}

	return nil
}

// proxyCACertValid 报告 PEM 数据是否是可解析且未过期的 X509 证书。
func proxyCACertValid(data []byte) bool {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)

	return err == nil && time.Now().Before(cert.NotAfter)
}

// proxyCACombinedValid 报告合并 PEM 是否同时持有可解析的 cert（未过期）与私钥。
func proxyCACombinedValid(data []byte) bool {
	if !proxyCACertValid(data) {
		return false
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
				return true
			}
		case "PRIVATE KEY":
			if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				return true
			}
		}
	}
}

// proxyCACertBlock 从合并 PEM 中提取纯 cert 段（cert 重导出）。
func proxyCACertBlock(data []byte) []byte {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			return pem.EncodeToMemory(block)
		}
	}
}

// writeFileAtomic 经同目录 tmp 文件 + rename 原子写入，进程崩溃不留半文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()

		return fmt.Errorf("chmod tmp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()

		return fmt.Errorf("write tmp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename tmp file to %s: %w", path, err)
	}

	return nil
}
