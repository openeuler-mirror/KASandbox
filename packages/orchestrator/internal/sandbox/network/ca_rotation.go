package network

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	// proxyCACurrentLink 是指向当前代际目录的 symlink 名（tmp symlink + rename
	// 原子切换，§7.5.1）。
	proxyCACurrentLink = "current"
	// proxyCAPreviousLink 是指向上一次被替换代际的 symlink 名（误轮转的回滚
	// 余地，GC 保留对象之一）。与 current 无法原子同改：先切 current，成功后
	// 再更新 previous；current 切换失败时 previous 不动。
	proxyCAPreviousLink = "previous"
	// proxyCAGenPrefix 是代际目录名前缀。gen-<UTC时间戳>（同秒冲突追加序号）
	// 的命名只为唯一性服务，GC 不依赖其字典序。
	proxyCAGenPrefix = "gen-"
	// proxyCARotateTriggerFile 是 confdir 内的轮转触发文件名（§7.5.2）。
	proxyCARotateTriggerFile = "ROTATE"
	// proxyCARotateStatusFile 记录最近一次轮转结果，供运维回读确认。
	proxyCARotateStatusFile = "rotate-status.json"
)

// proxyCARotatePollInterval 是触发文件轮询间隔（UT 可缩短）。
var proxyCARotatePollInterval = 2 * time.Second

// ProxyCASnapshot 是进程内当前 CA 代际的原子快照（§7.5.1）。沙箱创建路径经
// AcquireProxyCA 取一次代际引用后，spawn 的 confdir 与 guest 注入的 cert 恒为
// 同一代际，轮转可在任意时刻安全发生（§7.5.4）。
type ProxyCASnapshot struct {
	Gen     string // 代际目录名（gen-<UTC时间戳>）
	GenDir  string // 代际目录完整路径（spawn confdir 用，不经 current 二次寻址）
	CertPEM []byte // 纯 cert PEM（guest 注入输入）
	// Fingerprint 是 cert DER 字节的 hex sha256，与
	// openssl x509 -fingerprint -sha256 输出一致（日志/rotate-status.json
	// 对账口径）。注意与 guest 注入幂等检查用的 PEM 字节哈希不同（那边只需
	// 内部自洽）。
	Fingerprint string
}

var (
	// proxyCAMu 串行化启动路径（ensureProxyCA）与轮转协程（RotateProxyCA）
	// （§7.5.2 并发约束：单进程内锁，不引入文件锁）；引用计数表共用此锁。
	proxyCAMu sync.Mutex
	// currentProxyCA 是当前代际快照；CA_AUTO 关闭或尚未加载时为 nil。
	currentProxyCA atomic.Pointer[ProxyCASnapshot]
	// proxyCARefs 是 per-gen 进程内引用计数（沙箱生命周期持有，销毁时释放）。
	proxyCARefs = map[string]int{}
)

// CurrentProxyCA 返回当前 CA 代际快照；CA_AUTO 关闭或尚未加载时返回 nil。
func CurrentProxyCA() *ProxyCASnapshot {
	return currentProxyCA.Load()
}

// AcquireProxyCA 返回当前快照并持有该代际的一份引用（引用计数 +1，阻止 GC
// 回收对应目录）；快照为 nil（CA_AUTO 关闭/未加载）时不计数。调用方必须在
// 沙箱销毁时 ReleaseProxyCA。
func AcquireProxyCA() *ProxyCASnapshot {
	snap := currentProxyCA.Load()
	if snap == nil {
		return nil
	}

	proxyCAMu.Lock()
	proxyCARefs[snap.Gen]++
	proxyCAMu.Unlock()

	return snap
}

// ReleaseProxyCA 释放 AcquireProxyCA 持有的代际引用。
func ReleaseProxyCA(gen string) {
	proxyCAMu.Lock()
	defer proxyCAMu.Unlock()

	if proxyCARefs[gen] > 1 {
		proxyCARefs[gen]--
	} else {
		delete(proxyCARefs, gen)
	}
}

type proxyCAGenState int

const (
	proxyCAGenInvalid proxyCAGenState = iota // 缺失/损坏/过期
	proxyCAGenHalf                           // 合并 PEM 有效、纯 cert 缺失/损坏（可重导出补全）
	proxyCAGenValid                          // 两文件齐备、可解析、未过期
)

// currentProxyCAGen 解析 current symlink 指向的代际目录名；symlink 不存在返回
// 空串。目标一律取 base 名（拒绝越界路径），指向缺失/损坏代际由 loadProxyCAGen
// 判定兜底。
func currentProxyCAGen(confdir string) (string, error) {
	target, err := os.Readlink(filepath.Join(confdir, proxyCACurrentLink))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}

		return "", err
	}

	return filepath.Base(target), nil
}

// previousProxyCAGen 解析 previous symlink 指向的代际目录名；不存在/读失败/
// 悬空（目标已被删除）一律返回空串（视为无 previous，GC 容忍）。
func previousProxyCAGen(confdir string) string {
	target, err := os.Readlink(filepath.Join(confdir, proxyCAPreviousLink))
	if err != nil {
		return ""
	}

	return filepath.Base(target)
}

// proxyCACertFingerprint 返回 PEM cert 的 DER 字节 SHA-256 指纹（hex），与
// openssl x509 -fingerprint -sha256 同口径。
func proxyCACertFingerprint(certPEM []byte) (string, bool) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false
	}
	fp := sha256.Sum256(cert.Raw)

	return hex.EncodeToString(fp[:]), true
}

// loadProxyCAGen 加载并校验一个代际目录，返回快照（valid 时字段完整，half 时
// 只有 Gen/GenDir）与状态。读文件失败按「缺失」处理（真实故障由后续写入路径
// fail-fast 暴露）。
func loadProxyCAGen(confdir, gen string) (*ProxyCASnapshot, proxyCAGenState) {
	genDir := filepath.Join(confdir, gen)

	combinedPEM, err := os.ReadFile(filepath.Join(genDir, proxyCACombinedFile))
	if err != nil || !proxyCACombinedValid(combinedPEM) {
		return nil, proxyCAGenInvalid
	}

	certPEM, err := os.ReadFile(filepath.Join(genDir, proxyCACertFile))
	if err != nil || !proxyCACertValid(certPEM) {
		return &ProxyCASnapshot{Gen: gen, GenDir: genDir}, proxyCAGenHalf
	}

	fp, ok := proxyCACertFingerprint(certPEM)
	if !ok {
		return &ProxyCASnapshot{Gen: gen, GenDir: genDir}, proxyCAGenHalf
	}

	return &ProxyCASnapshot{
		Gen:         gen,
		GenDir:      genDir,
		CertPEM:     certPEM,
		Fingerprint: fp,
	}, proxyCAGenValid
}

// newProxyCAGenName 生成不与现存条目冲突的代际目录名。时间戳 + 同秒序号仅为
// 唯一性服务，GC 不依赖名字顺序（保留对象由 current/previous symlink 与引用
// 计数决定）。
func newProxyCAGenName(confdir string) string {
	base := proxyCAGenPrefix + time.Now().UTC().Format("20060102-150405")
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(confdir, name)); err != nil {
			// 不存在（或 confdir 本身异常，由后续写入路径 fail-fast）。
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// atomicSymlink 在 confdir 内先建 tmp symlink 再 rename 到 linkName（原子
// 替换，可覆盖已存在的 symlink/普通文件）。
func atomicSymlink(confdir, linkName, target string) error {
	link := filepath.Join(confdir, linkName)
	tmp := filepath.Join(confdir, fmt.Sprintf(".%s.tmp-%d", linkName, time.Now().UnixNano()))
	defer os.Remove(tmp)

	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create tmp symlink for %s: %w", linkName, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		return fmt.Errorf("rename tmp symlink to %s: %w", linkName, err)
	}

	return nil
}

// switchCurrentProxyCA 经 tmp symlink + rename 把 current 原子切到 gen。
func switchCurrentProxyCA(confdir, gen string) error {
	return atomicSymlink(confdir, proxyCACurrentLink, gen)
}

// adoptLegacyProxyCA 把 §7.4.1 扁平布局的散文件收编进新代际目录并切换 current
// （调用方持 proxyCAMu）。先把内容原子写进代际目录、切 symlink、最后把顶层
// 散文件原位替换为指向 current/<同名文件> 的相对 symlink（回退兼容：CA_AUTO
// 关闭或回退旧版二进制时经顶层路径仍能读到当前 CA，不会触发重生成；symlink
// 经 current 中转，后续轮转自动跟踪）。内容逐字节保留，不重新生成，存量沙箱
// 信任关系不变。
func adoptLegacyProxyCA(confdir string, combinedPEM []byte) error {
	gen := newProxyCAGenName(confdir)
	genDir := filepath.Join(confdir, gen)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return fmt.Errorf("create proxy CA generation dir %s: %w", genDir, err)
	}
	if err := writeFileAtomic(filepath.Join(genDir, proxyCACombinedFile), combinedPEM, 0o600); err != nil {
		return err
	}

	certPath := filepath.Join(confdir, proxyCACertFile)
	if certPEM, err := os.ReadFile(certPath); err == nil && proxyCACertValid(certPEM) {
		if err := writeFileAtomic(filepath.Join(genDir, proxyCACertFile), certPEM, 0o644); err != nil {
			return err
		}
	} else if err := writeFileAtomic(filepath.Join(genDir, proxyCACertFile), proxyCACertBlock(combinedPEM), 0o644); err != nil {
		// 半文件：纯 cert 散文件缺失/损坏，从合并 PEM 重导出补全。
		return err
	}

	if err := switchCurrentProxyCA(confdir, gen); err != nil {
		return err
	}
	// 顶层散文件原位替换为经 current 中转的相对 symlink（best-effort：失败
	// 留下原散文件，内容仍然有效，不影响收编本身）。
	for _, name := range []string{proxyCACombinedFile, proxyCACertFile} {
		if err := atomicSymlink(confdir, name, filepath.Join(proxyCACurrentLink, name)); err != nil {
			logger.L().Warn(context.Background(), "failed to replace legacy proxy CA file with symlink",
				zap.String("confdir", confdir), zap.String("file", name), zap.Error(err))
		}
	}

	snap, state := loadProxyCAGen(confdir, gen)
	if state != proxyCAGenValid {
		return fmt.Errorf("adopted proxy CA generation %s failed validation", gen)
	}
	currentProxyCA.Store(snap)

	return nil
}

// renewProxyCA 生成新代际并切换 current、刷新进程内快照（调用方持
// proxyCAMu）。symlink 切换是最后一个可失败步骤：失败时 current 与快照都不动，
// 且本次新建的 gen 目录被清理（残留坏目录会干扰后续 GC）。current 切换成功后
// 把被替换的旧代际记为 previous。
func renewProxyCA(confdir string) (*ProxyCASnapshot, error) {
	gen := newProxyCAGenName(confdir)
	genDir := filepath.Join(confdir, gen)
	// fresh 记录该目录是否本次新建（名字刚由 newProxyCAGenName 选出，正常必为
	// 真；保守判定防止误删别人已存在的目录）。
	_, statErr := os.Lstat(genDir)
	fresh := statErr != nil
	cleanup := func() {
		if fresh {
			_ = os.RemoveAll(genDir)
		}
	}

	if err := generateProxyCA(genDir); err != nil {
		cleanup()

		return nil, err
	}

	snap, state := loadProxyCAGen(confdir, gen)
	if state != proxyCAGenValid {
		cleanup()

		return nil, fmt.Errorf("freshly generated proxy CA generation %s failed validation", gen)
	}

	oldGen, err := currentProxyCAGen(confdir)
	if err != nil {
		cleanup()

		return nil, fmt.Errorf("resolve current proxy CA generation before switch: %w", err)
	}
	if err := switchCurrentProxyCA(confdir, gen); err != nil {
		cleanup()

		return nil, err
	}
	// previous 在 current 切换成功后才更新（两 symlink 无法原子同改）；更新
	// 失败仅记日志，不影响轮转结果。
	if oldGen != "" && oldGen != gen {
		if err := atomicSymlink(confdir, proxyCAPreviousLink, oldGen); err != nil {
			logger.L().Warn(context.Background(), "failed to update previous proxy CA generation link",
				zap.String("confdir", confdir), zap.String("previous", oldGen), zap.Error(err))
		}
	}

	currentProxyCA.Store(snap)

	return snap, nil
}

// GCProxyCAGenerations 回收旧代际目录（§7.5.5）：保留 current、previous 与
// 进程内引用计数 >0 的代际，其余整目录删除。只在两处显式调用：daemon 启动
// （main.go，重启后引用清零）与 rotateProxyCA 切换成功后。create-build 等无
// 引用计数上下文的工具路径不得调用。
func GCProxyCAGenerations(confdir string) {
	proxyCAMu.Lock()
	defer proxyCAMu.Unlock()

	gcProxyCAGens(confdir)
}

// gcProxyCAGens 是 GC 本体（调用方持 proxyCAMu）。previous 悬空（目标已被删）
// 不影响回收。失败仅记日志——回收是后台义务，不影响 CA 可用性。
func gcProxyCAGens(confdir string) {
	current, err := currentProxyCAGen(confdir)
	if err != nil {
		return
	}
	previous := previousProxyCAGen(confdir)

	entries, err := os.ReadDir(confdir)
	if err != nil {
		return
	}

	for _, e := range entries {
		gen := e.Name()
		if !e.IsDir() || !strings.HasPrefix(gen, proxyCAGenPrefix) {
			continue
		}
		// current/previous 相同天然去重（相等判断命中任一即保留）。
		if gen == current || gen == previous || proxyCARefs[gen] > 0 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(confdir, gen)); err != nil {
			logger.L().Warn(context.Background(), "failed to GC old proxy CA generation",
				zap.String("gen", gen), zap.Error(err))
			continue
		}
		delete(proxyCARefs, gen)
	}
}

// proxyCARotateStatus 是 rotate-status.json 的内容（最近一次轮转结果）。
type proxyCARotateStatus struct {
	Time           time.Time `json:"time"`
	Generation     string    `json:"generation"`
	OldFingerprint string    `json:"old_fingerprint,omitempty"`
	NewFingerprint string    `json:"new_fingerprint,omitempty"`
	Success        bool      `json:"success"`
	Rollback       bool      `json:"rollback,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// RotateProxyCA 执行一次运行时 CA 轮转（§7.5.3，供 SIGHUP handler 等调用方
// 使用）：生成新代际 → tmp+rename 写两个 PEM → 原子切 current → 更新
// previous → 刷新进程内快照 → GC 旧代际 → 写 rotate-status.json。任一步
// 失败：current 与快照保持旧值不动，status 记失败——在跑流量与新建沙箱
// 不受影响。
func RotateProxyCA(confdir string) error {
	_, err := rotateProxyCA(confdir, "")

	return err
}

// rotateProxyCA 是轮转序列本体；rollbackGen 非空时为回滚变体（§7.5.5）：校验
// 该既有代际有效后把 current 指回它并刷新快照，不生成新 CA。回滚同样只影响
// 新建沙箱。
func rotateProxyCA(confdir, rollbackGen string) (*proxyCARotateStatus, error) {
	proxyCAMu.Lock()
	defer proxyCAMu.Unlock()

	status := &proxyCARotateStatus{Time: time.Now().UTC(), Rollback: rollbackGen != ""}
	if old := currentProxyCA.Load(); old != nil {
		status.OldFingerprint = old.Fingerprint
	}

	if rollbackGen != "" {
		// 回滚目标校验：必须是 confdir 下真实存在的 gen- 前缀目录——拒绝
		// symlink（写入 current 会造成自环，并误导 GC 误删全部真代际私钥）。
		// 任一不满足：不切换、不 GC，status 记失败。
		gen := filepath.Base(rollbackGen)
		if !strings.HasPrefix(gen, proxyCAGenPrefix) {
			return failProxyCARotation(confdir, status,
				fmt.Errorf("rollback target %q is not a generation name", rollbackGen))
		}
		if st, err := os.Lstat(filepath.Join(confdir, gen)); err != nil || !st.IsDir() {
			return failProxyCARotation(confdir, status,
				fmt.Errorf("rollback target generation %q is missing or not a real directory", gen))
		}

		snap, state := loadProxyCAGen(confdir, gen)
		if state == proxyCAGenHalf {
			// 半文件代际：补全纯 cert 后再指回。
			combinedPEM, err := os.ReadFile(filepath.Join(snap.GenDir, proxyCACombinedFile))
			if err != nil {
				return failProxyCARotation(confdir, status, err)
			}
			if err := writeFileAtomic(filepath.Join(snap.GenDir, proxyCACertFile), proxyCACertBlock(combinedPEM), 0o644); err != nil {
				return failProxyCARotation(confdir, status, err)
			}
			snap, state = loadProxyCAGen(confdir, gen)
		}
		if state != proxyCAGenValid {
			return failProxyCARotation(confdir, status,
				fmt.Errorf("rollback target generation %q is corrupt or expired", gen))
		}

		oldGen, err := currentProxyCAGen(confdir)
		if err != nil {
			return failProxyCARotation(confdir, status,
				fmt.Errorf("resolve current proxy CA generation before rollback: %w", err))
		}
		if err := switchCurrentProxyCA(confdir, gen); err != nil {
			return failProxyCARotation(confdir, status, err)
		}
		// 与 renewProxyCA 同序：current 成功后才更新 previous（失败仅记日志）。
		if oldGen != "" && oldGen != gen {
			if err := atomicSymlink(confdir, proxyCAPreviousLink, oldGen); err != nil {
				logger.L().Warn(context.Background(), "failed to update previous proxy CA generation link",
					zap.String("confdir", confdir), zap.String("previous", oldGen), zap.Error(err))
			}
		}
		currentProxyCA.Store(snap)
		gcProxyCAGens(confdir)

		status.Generation = gen
		status.NewFingerprint = snap.Fingerprint
		status.Success = true
		writeProxyCARotateStatus(confdir, status)

		return status, nil
	}

	snap, err := renewProxyCA(confdir)
	if err != nil {
		return failProxyCARotation(confdir, status, err)
	}
	gcProxyCAGens(confdir)

	status.Generation = snap.Gen
	status.NewFingerprint = snap.Fingerprint
	status.Success = true
	writeProxyCARotateStatus(confdir, status)

	return status, nil
}

// failProxyCARotation 记录失败 status 并返回原始错误（current 与快照不动）。
func failProxyCARotation(confdir string, status *proxyCARotateStatus, err error) (*proxyCARotateStatus, error) {
	status.Error = err.Error()
	writeProxyCARotateStatus(confdir, status)

	return status, err
}

// writeProxyCARotateStatus 原子写 rotate-status.json（0644）；写失败仅记日志
// （轮转状态本身已落定，status 只是运维观测面）。
func writeProxyCARotateStatus(confdir string, status *proxyCARotateStatus) {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(confdir, proxyCARotateStatusFile), data, 0o644); err != nil {
		logger.L().Warn(context.Background(), "failed to write proxy CA rotate status",
			zap.String("confdir", confdir), zap.Error(err))
	}
}

// StartProxyCARotationWatcher 启动触发文件轮询协程（§7.5.2 主触发方式）：每
// 2s 检查 confdir/ROTATE，存在则读取内容（空 = 正常轮转；非空且为已存在代际
// 目录名 = 回滚到该代际）、删除文件并执行轮转，结果打日志（新旧指纹，与
// guest 内 openssl x509 -fingerprint 输出对账）。ctx 取消即退出。
func StartProxyCARotationWatcher(ctx context.Context, confdir string) {
	go func() {
		ticker := time.NewTicker(proxyCARotatePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			triggerPath := filepath.Join(confdir, proxyCARotateTriggerFile)
			data, err := os.ReadFile(triggerPath)
			if err != nil {
				continue
			}
			// 先删触发文件再执行：轮转崩溃不会反复触发。
			if err := os.Remove(triggerPath); err != nil {
				logger.L().Warn(ctx, "failed to remove proxy CA rotate trigger file",
					zap.String("path", triggerPath), zap.Error(err))
				continue
			}

			target := strings.TrimSpace(string(data))
			logger.L().Info(ctx, "proxy CA rotation triggered",
				zap.String("confdir", confdir), zap.String("rollback_gen", target))

			status, err := rotateProxyCA(confdir, target)
			if err != nil {
				logger.L().Error(ctx, "proxy CA rotation failed", zap.Error(err))
				continue
			}
			logger.L().Info(ctx, "proxy CA rotated",
				zap.String("gen", status.Generation),
				zap.String("old_fingerprint", status.OldFingerprint),
				zap.String("new_fingerprint", status.NewFingerprint),
				zap.Bool("rollback", status.Rollback),
			)
		}
	}()
}
