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

// mitmproxy confdir 代际化布局（§7.5.1）：
//
//	confdir/
//	├── current -> gen-<UTC时间戳>/    # tmp symlink + rename 原子切换
//	├── previous -> gen-<UTC时间戳>/   # 上一次被替换的代际（回滚余地 + GC 保留）
//	├── mitmproxy-ca.pem -> current/mitmproxy-ca.pem            # 回退兼容 symlink
//	├── mitmproxy-ca-cert.pem -> current/mitmproxy-ca-cert.pem  #（仅老布局收编后建）
//	└── gen-<UTC时间戳>/
//	    ├── mitmproxy-ca.pem          # cert+key 合并（0600，mitmproxy confdir
//	    │                             # 约定格式），代理进程签发落叶证书的输入
//	    └── mitmproxy-ca-cert.pem     # 纯 cert（0644），guest 注入的输入
//
// CA_AUTO=false 的手工布局仍是 confdir 顶层同名散文件（ProxyCACertPath 不变）。
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

// ensureProxyCA 保证 confdir 内存在可用的代理 CA 材料并加载进程内代际快照
// （SANDBOX_PROXY_CA_AUTO=true 时由 ParseConfig 调用）。幂等：current 指向的
// 代际两文件齐备、可解析且 cert 未过期即加载快照跳过（不重新生成）；current
// 缺失但顶层存在 §7.4.1 老布局散文件时收编为 gen-<ts>/（内容逐字节保留，
// 不重新生成，存量沙箱信任关系不变）；代际内半文件状态可补全（cert 缺失时
// 从合并 PEM 重导出）；全空/损坏/过期生成新代际并切换 current。任何写入
// 失败 fail-fast（可预见的 MITM 全断比启动失败更难排查）。
//
// 并发前提：单实例 orchestrator，本函数（启动路径）与 RotateProxyCA（轮转
// 协程）共用 proxyCAMu 串行化，无多进程竞争，不需要文件锁；tmp+rename 原子
// 写仅为防进程崩溃留下半文件。
//
// 本函数不做旧代际 GC：create-build 工具也走本路径，它没有 daemon 的引用计数
// 上下文，共用 confdir 时会误删 daemon 正在引用的代际。GC 只在两处显式发生：
// daemon 启动时 main.go 调 GCProxyCAGenerations（彼时安全的原因：重启后进程内
// 引用清零，而在跑代理只在 spawn 时读一次 CA 入内存、之后不读盘——孤儿
// mitmproxy 不在 hostservice.KillOrphanedProcesses 的清理范围内，它只杀
// Android 辅助进程，但同样不读盘），以及 rotateProxyCA 切换成功后。
func ensureProxyCA(confdir string) error {
	proxyCAMu.Lock()
	defer proxyCAMu.Unlock()

	gen, err := currentProxyCAGen(confdir)
	if err != nil {
		return fmt.Errorf("resolve current proxy CA generation: %w", err)
	}

	if gen != "" {
		snap, state := loadProxyCAGen(confdir, gen)
		switch state {
		case proxyCAGenValid:
			// 幂等跳过：重启/滚动不重生成。
			currentProxyCA.Store(snap)

			return nil
		case proxyCAGenHalf:
			// 半文件：合并 PEM 在、纯 cert 缺失/损坏——cert 从合并 PEM 重导出。
			combinedPEM, err := os.ReadFile(filepath.Join(snap.GenDir, proxyCACombinedFile))
			if err != nil {
				return fmt.Errorf("read proxy CA combined PEM from %s: %w", snap.GenDir, err)
			}
			if err := writeFileAtomic(filepath.Join(snap.GenDir, proxyCACertFile), proxyCACertBlock(combinedPEM), 0o644); err != nil {
				return err
			}
			snap, _ = loadProxyCAGen(confdir, gen)
			currentProxyCA.Store(snap)

			return nil
		}
		// 损坏/过期：落到下方生成新代际（current 原子切换，旧代际由 previous
		// symlink 与显式 GC 管理）。
	} else {
		// current 缺失：顶层老布局（§7.4.1 扁平散文件）收编进新代际目录。
		combinedPath := filepath.Join(confdir, proxyCACombinedFile)
		if combinedPEM, err := os.ReadFile(combinedPath); err == nil && proxyCACombinedValid(combinedPEM) {
			return adoptLegacyProxyCA(confdir, combinedPEM)
		}
	}

	// 全空、损坏或过期——生成新代际并切换 current。
	_, err = renewProxyCA(confdir)

	return err
}

// generateProxyCA 生成 RSA 2048 自签根 CA 并原子写入 dir 的两个约定文件
// （dir 为代际目录，或 CA_AUTO=false 手工布局下直接是 confdir）。
func generateProxyCA(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create proxy CA dir %s: %w", dir, err)
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

	if err := writeFileAtomic(filepath.Join(dir, proxyCACombinedFile), combinedPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, proxyCACertFile), certPEM, 0o644); err != nil {
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
