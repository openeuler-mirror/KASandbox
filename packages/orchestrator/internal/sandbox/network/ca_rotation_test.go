package network

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件用例操作进程内全局状态（currentProxyCA 快照 / proxyCARefs 引用计数），
// 一律不 parallel——Go 保证非 parallel 用例不与任何 parallel 用例并发执行。

// resetProxyCAState 清空进程内快照与引用计数，使用例互不污染。
func resetProxyCAState(t *testing.T) {
	t.Helper()

	currentProxyCA.Store(nil)
	proxyCAMu.Lock()
	clear(proxyCARefs)
	proxyCAMu.Unlock()
}

func readRotateStatus(t *testing.T, confdir string) proxyCARotateStatus {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(confdir, proxyCARotateStatusFile))
	require.NoError(t, err)
	var status proxyCARotateStatus
	require.NoError(t, json.Unmarshal(data, &status))

	return status
}

// derFingerprint 计算 PEM cert 的 DER SHA-256 指纹（与实现及
// openssl x509 -fingerprint -sha256 同口径）。
func derFingerprint(t *testing.T, certPEM []byte) string {
	t.Helper()

	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	fp := sha256.Sum256(cert.Raw)

	return hex.EncodeToString(fp[:])
}

// listGenDirs 返回 confdir 下全部 gen- 前缀的真实目录名（排序后）。
func listGenDirs(t *testing.T, confdir string) []string {
	t.Helper()

	entries, err := os.ReadDir(confdir)
	require.NoError(t, err)
	var gens []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), proxyCAGenPrefix) {
			gens = append(gens, e.Name())
		}
	}
	sort.Strings(gens)

	return gens
}

// writeExpiredProxyCA 往 dir 写入一套已过期的 CA 材料（测试用 1024-bit key
// 提速，有效性校验不看密钥强度）。
func writeExpiredProxyCA(t *testing.T, dir string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: proxyCACN},
		NotBefore:             now.Add(-48 * time.Hour),
		NotAfter:              now.Add(-24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	require.NoError(t, os.WriteFile(filepath.Join(dir, proxyCACombinedFile), append(certPEM, keyPEM...), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, proxyCACertFile), certPEM, 0o644))
}

func TestCurrentProxyCANilWhenNotLoaded(t *testing.T) {
	resetProxyCAState(t)

	assert.Nil(t, CurrentProxyCA(), "CA_AUTO 关闭/尚未加载时快照必须为 nil")
	assert.Nil(t, AcquireProxyCA(), "nil 快照时不计数")
}

func TestEnsureProxyCALoadsSnapshot(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))

	snap := CurrentProxyCA()
	require.NotNil(t, snap, "ensure 成功后必须加载进程内快照")
	assert.True(t, strings.HasPrefix(snap.Gen, proxyCAGenPrefix))
	assert.Equal(t, filepath.Join(confdir, snap.Gen), snap.GenDir)

	cert, err := os.ReadFile(filepath.Join(snap.GenDir, proxyCACertFile))
	require.NoError(t, err)
	assert.Equal(t, cert, snap.CertPEM)
	// 指纹是 DER 口径（与 openssl x509 -fingerprint -sha256 一致），不是
	// PEM 文本字节哈希。
	assert.Equal(t, derFingerprint(t, cert), snap.Fingerprint)
	pemFP := sha256.Sum256(cert)
	assert.NotEqual(t, hex.EncodeToString(pemFP[:]), snap.Fingerprint)
}

func TestEnsureProxyCAAdoptsLegacyLayout(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	// 造 §7.4.1 老布局扁平散文件。
	require.NoError(t, generateProxyCA(confdir))
	legacyCert, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	legacyFP := derFingerprint(t, legacyCert)

	require.NoError(t, ensureProxyCA(confdir))

	// 收编进 gen 目录，内容逐字节不变（移动而非重生成，存量信任关系不变）。
	genDir := proxyCACurrentGenDir(t, confdir)
	require.NotEqual(t, confdir, genDir)
	adoptedCert, err := os.ReadFile(filepath.Join(genDir, proxyCACertFile))
	require.NoError(t, err)
	assert.Equal(t, legacyCert, adoptedCert, "收编不得重新生成 CA")

	// 顶层散文件原位替换为经 current 中转的相对 symlink（回退兼容）。
	for _, name := range []string{proxyCACombinedFile, proxyCACertFile} {
		fi, err := os.Lstat(filepath.Join(confdir, name))
		require.NoError(t, err)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0, "顶层 %s 收编后必须是 symlink", name)
		target, err := os.Readlink(filepath.Join(confdir, name))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(proxyCACurrentLink, name), target)
	}
	// 经顶层路径（跟随 symlink）读到的内容与 gen 内一致。
	viaTop, err := os.ReadFile(filepath.Join(confdir, proxyCACertFile))
	require.NoError(t, err)
	assert.Equal(t, adoptedCert, viaTop)

	snap := CurrentProxyCA()
	require.NotNil(t, snap)
	assert.Equal(t, filepath.Base(genDir), snap.Gen)
	assert.Equal(t, legacyCert, snap.CertPEM)
	assert.Equal(t, legacyFP, snap.Fingerprint, "收编指纹必须不变")

	// 收编后幂等：再跑一次不重新生成、指纹不变。
	require.NoError(t, ensureProxyCA(confdir))
	assert.Equal(t, legacyFP, CurrentProxyCA().Fingerprint)
	assert.Equal(t, genDir, proxyCACurrentGenDir(t, confdir))
}

func TestEnsureProxyCAAdoptsLegacyHalfState(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, generateProxyCA(confdir))
	combined, err := os.ReadFile(filepath.Join(confdir, proxyCACombinedFile))
	require.NoError(t, err)
	// 半文件：只有合并 PEM，纯 cert 缺失。
	require.NoError(t, os.Remove(filepath.Join(confdir, proxyCACertFile)))

	require.NoError(t, ensureProxyCA(confdir))

	genDir := proxyCACurrentGenDir(t, confdir)
	require.NotEqual(t, confdir, genDir)
	cert, err := os.ReadFile(filepath.Join(genDir, proxyCACertFile))
	require.NoError(t, err)
	assert.Equal(t, proxyCACertBlock(combined), cert, "半文件收编：cert 从合并 PEM 重导出补全")
	adoptedCombined, err := os.ReadFile(filepath.Join(genDir, proxyCACombinedFile))
	require.NoError(t, err)
	assert.Equal(t, combined, adoptedCombined, "收编不重生成 key")
}

func TestEnsureProxyCAExpiredRegenerates(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	genDir := proxyCACurrentGenDir(t, confdir)
	oldFP := CurrentProxyCA().Fingerprint

	// 把 current 指向的代际替换为已过期材料。
	writeExpiredProxyCA(t, genDir)

	require.NoError(t, ensureProxyCA(confdir))

	newGenDir := proxyCACurrentGenDir(t, confdir)
	assert.NotEqual(t, genDir, newGenDir, "过期必须生成新代际")
	assert.NotEqual(t, oldFP, CurrentProxyCA().Fingerprint)
	cert := parseProxyCACert(t, confdir)
	assert.True(t, time.Now().Before(cert.NotAfter), "新 CA 必须在有效期内")
}

func TestRotateProxyCASuccess(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	before := CurrentProxyCA()
	require.NotNil(t, before)

	require.NoError(t, RotateProxyCA(confdir))

	after := CurrentProxyCA()
	require.NotNil(t, after)
	assert.NotEqual(t, before.Fingerprint, after.Fingerprint, "轮转后快照必须切到新 CA")
	assert.NotEqual(t, before.Gen, after.Gen)
	assert.Equal(t, after.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)),
		"current symlink 必须指向新代际")

	// rotate-status.json：0644 + 完整字段。
	info, err := os.Stat(filepath.Join(confdir, proxyCARotateStatusFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	status := readRotateStatus(t, confdir)
	assert.True(t, status.Success)
	assert.Empty(t, status.Error)
	assert.False(t, status.Rollback)
	assert.False(t, status.Time.IsZero())
	assert.Equal(t, after.Gen, status.Generation)
	assert.Equal(t, before.Fingerprint, status.OldFingerprint)
	assert.Equal(t, after.Fingerprint, status.NewFingerprint)

	// 旧代际被记为 previous 并保留（误轮转的回滚余地）。
	assert.Equal(t, before.Gen, previousProxyCAGen(confdir), "被替换的旧代际必须记为 previous")
	_, err = os.Stat(before.GenDir)
	assert.NoError(t, err, "previous 代际必须保留")
}

func TestRotateProxyCAGCsOldestGeneration(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	gen1 := CurrentProxyCA()

	require.NoError(t, RotateProxyCA(confdir)) // gen2 current，previous=gen1
	gen2 := CurrentProxyCA()
	require.NoError(t, RotateProxyCA(confdir)) // gen3 current，previous=gen2，GC 回收 gen1
	gen3 := CurrentProxyCA()

	_, err := os.Stat(gen1.GenDir)
	assert.True(t, os.IsNotExist(err), "既非 current 又非 previous 的 gen1 被回收")
	_, err = os.Stat(gen2.GenDir)
	assert.NoError(t, err, "previous（gen2）保留")
	assert.Equal(t, gen3.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)))
	assert.Equal(t, gen2.Gen, previousProxyCAGen(confdir))
}

func TestEnsureProxyCADoesNotGC(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	current := CurrentProxyCA()
	require.NotNil(t, current)

	// 造一个无引用的游离旧代际（非 current、无 previous 指向，GC 理应回收）。
	staleGen := "gen-20000101-000000"
	require.NoError(t, generateProxyCA(filepath.Join(confdir, staleGen)))

	// ensureProxyCA 幂等路径不执行 GC（create-build 也走此路径，没有 daemon
	// 的引用计数上下文，误删风险）。
	require.NoError(t, ensureProxyCA(confdir))
	assert.Contains(t, listGenDirs(t, confdir), staleGen, "ensureProxyCA 不得回收旧代际")

	// 显式 GC（daemon 启动路径 / 轮转成功后）才回收。
	GCProxyCAGenerations(confdir)
	assert.NotContains(t, listGenDirs(t, confdir), staleGen)
	assert.Contains(t, listGenDirs(t, confdir), current.Gen, "current 代际不受影响")
}

func TestRotateProxyCARefCountPinsGeneration(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	gen1 := AcquireProxyCA()
	require.NotNil(t, gen1)

	require.NoError(t, RotateProxyCA(confdir)) // gen2（gen1 成为最近旧代际，保留）
	require.NoError(t, RotateProxyCA(confdir)) // gen3：GC 时 gen1 仍被引用

	_, err := os.Stat(gen1.GenDir)
	assert.NoError(t, err, "引用计数持有期间不得回收")

	ReleaseProxyCA(gen1.Gen)
	require.NoError(t, RotateProxyCA(confdir)) // gen4：GC 回收引用清零的 gen1

	_, err = os.Stat(gen1.GenDir)
	assert.True(t, os.IsNotExist(err), "引用清零后旧代际被回收")
}

func TestRotateProxyCARollback(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	gen1 := CurrentProxyCA()
	require.NoError(t, RotateProxyCA(confdir))
	gen2 := CurrentProxyCA()

	// 回滚变体：ROTATE 内容为已存在代际目录名 → current 指回，不生成新 CA。
	status, err := rotateProxyCA(confdir, gen1.Gen)
	require.NoError(t, err)
	assert.True(t, status.Rollback)
	after := CurrentProxyCA()
	require.NotNil(t, after)
	assert.Equal(t, gen1.Fingerprint, after.Fingerprint, "回滚后快照必须是旧代际 CA")
	assert.Equal(t, gen1.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)))

	// gen2 成为 previous 被保留。
	assert.Equal(t, gen2.Gen, previousProxyCAGen(confdir))
	_, err = os.Stat(gen2.GenDir)
	assert.NoError(t, err)

	// 回滚目标必须收敛为 base 名（不允许越界路径）："../gen-x" 等价于 gen-x。
	_, err = rotateProxyCA(confdir, "../"+gen2.Gen)
	require.NoError(t, err)
	assert.Equal(t, gen2.Fingerprint, CurrentProxyCA().Fingerprint)

	// 无效回滚目标：报错且 current/快照不动，status 记失败。
	_, err = rotateProxyCA(confdir, "gen-19700101-000000")
	require.Error(t, err)
	assert.Equal(t, gen2.Fingerprint, CurrentProxyCA().Fingerprint)
	assert.Equal(t, gen2.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)))
	failStatus := readRotateStatus(t, confdir)
	assert.False(t, failStatus.Success)
	assert.True(t, failStatus.Rollback)
	assert.NotEmpty(t, failStatus.Error)
}

func TestRotateProxyCAFailureKeepsCurrentState(t *testing.T) {
	resetProxyCAState(t)

	base := t.TempDir()
	confdir := filepath.Join(base, "confdir")
	require.NoError(t, os.MkdirAll(confdir, 0o755))
	require.NoError(t, ensureProxyCA(confdir))
	before := CurrentProxyCA()
	require.NotNil(t, before)

	// 制造失败：confdir 变成「文件而非目录」（root 下 chmod 0 仍可写，不可靠）。
	aside := filepath.Join(base, "confdir-aside")
	require.NoError(t, os.Rename(confdir, aside))
	require.NoError(t, os.WriteFile(confdir, []byte("not a dir"), 0o644))

	require.Error(t, RotateProxyCA(confdir))
	require.Same(t, before, CurrentProxyCA(), "轮转失败不得触碰进程内快照")

	// 复原后一切照旧：current 仍指向原代际，可正常再轮转。
	require.NoError(t, os.Remove(confdir))
	require.NoError(t, os.Rename(aside, confdir))
	assert.Equal(t, before.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)))
	require.NoError(t, RotateProxyCA(confdir))
	assert.NotEqual(t, before.Fingerprint, CurrentProxyCA().Fingerprint)
}

func TestRotateProxyCAPreviousLinkTracksReplaced(t *testing.T) {
	// A→B→回滚A→轮转C：GC 保留 A（previous）与 C（current），删除 B（无引用）。
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir)) // A current，无 previous
	genA := CurrentProxyCA()
	assert.Empty(t, previousProxyCAGen(confdir), "首次生成无 previous")

	require.NoError(t, RotateProxyCA(confdir)) // B current，previous=A
	genB := CurrentProxyCA()
	assert.Equal(t, genA.Gen, previousProxyCAGen(confdir))

	_, err := rotateProxyCA(confdir, genA.Gen) // 回滚：A current，previous=B
	require.NoError(t, err)
	assert.Equal(t, genB.Gen, previousProxyCAGen(confdir))

	require.NoError(t, RotateProxyCA(confdir)) // C current，previous=A，GC 删 B
	genC := CurrentProxyCA()
	assert.Equal(t, genA.Gen, previousProxyCAGen(confdir))

	gens := listGenDirs(t, confdir)
	assert.Contains(t, gens, genA.Gen, "previous 保留")
	assert.Contains(t, gens, genC.Gen, "current 保留")
	assert.NotContains(t, gens, genB.Gen, "既非 current 又非 previous 且无引用 → 回收")
}

func TestGCProxyCAGenerationsPreviousEdgeCases(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir)) // A
	genA := CurrentProxyCA()
	require.NoError(t, RotateProxyCA(confdir)) // B current，previous=A
	genB := CurrentProxyCA()

	// current==previous：previous 指向 current 自身，去重且不超量保留——A 变为
	// 既非 current 又非 previous，被回收。
	previousLink := filepath.Join(confdir, proxyCAPreviousLink)
	require.NoError(t, os.Remove(previousLink))
	require.NoError(t, os.Symlink(genB.Gen, previousLink))
	GCProxyCAGenerations(confdir)
	assert.Contains(t, listGenDirs(t, confdir), genB.Gen, "current==previous 时 current 不受影响")
	assert.NotContains(t, listGenDirs(t, confdir), genA.Gen)

	// previous 悬空（目标已被删）：GC 容忍，不报错、不误删 current。
	require.NoError(t, os.Remove(previousLink))
	require.NoError(t, os.Symlink("gen-19990101-000000", previousLink))
	GCProxyCAGenerations(confdir)
	assert.Contains(t, listGenDirs(t, confdir), genB.Gen)
}

func TestRotateProxyCARollbackTargetValidation(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir)) // A
	genA := CurrentProxyCA()
	require.NoError(t, RotateProxyCA(confdir)) // B current，previous=A
	before := CurrentProxyCA()

	// 非法回滚目标：current（symlink 且无 gen- 前缀）、不存在的 gen、gen- 前缀
	// 的普通文件、symlink 形态的 gen 名——全部拒绝，且 current/快照/GC 不动。
	fakeFile := "gen-20000101-000000"
	require.NoError(t, os.WriteFile(filepath.Join(confdir, fakeFile), []byte("not a dir"), 0o644))
	fakeLink := "gen-20010101-000000"
	require.NoError(t, os.Symlink(genA.Gen, filepath.Join(confdir, fakeLink)))

	for _, target := range []string{proxyCACurrentLink, "gen-19700101-000000", fakeFile, fakeLink} {
		_, err := rotateProxyCA(confdir, target)
		require.Error(t, err, "target %q must be rejected", target)
		require.Same(t, before, CurrentProxyCA(), "target %q: 快照不得动", target)
		assert.Equal(t, before.Gen, filepath.Base(proxyCACurrentGenDir(t, confdir)),
			"target %q: current 不得动", target)

		status := readRotateStatus(t, confdir)
		assert.False(t, status.Success)
		assert.True(t, status.Rollback)
		assert.NotEmpty(t, status.Error)
	}

	// GC 未执行：A（previous）与 B（current）都在。
	gens := listGenDirs(t, confdir)
	assert.Contains(t, gens, genA.Gen)
	assert.Contains(t, gens, before.Gen)
}

func TestRotateProxyCAFailureCleansUpNewGen(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	before := CurrentProxyCA()
	require.NotNil(t, before)

	// 制造切换失败：current 替换为（空）目录——Readlink 目录报 EINVAL，
	// 发生在新 gen 目录已建好并写满材料之后。
	currentPath := filepath.Join(confdir, proxyCACurrentLink)
	require.NoError(t, os.Remove(currentPath))
	require.NoError(t, os.Mkdir(currentPath, 0o755))

	require.Error(t, RotateProxyCA(confdir))
	require.Same(t, before, CurrentProxyCA(), "轮转失败不得触碰进程内快照")

	// 本次新建的 gen 目录已被清理：只剩原代际。
	assert.Equal(t, []string{before.Gen}, listGenDirs(t, confdir),
		"失败残留的新 gen 目录必须清掉")
	// current 切换失败时 previous 不动（此处从未存在）。
	assert.Empty(t, previousProxyCAGen(confdir))

	// 修复 current 后再次轮转成功：GC 保留真正的前一个代际（previous=A），
	// 而不是任何残留目录。
	require.NoError(t, os.Remove(currentPath))
	require.NoError(t, os.Symlink(before.Gen, currentPath))
	require.NoError(t, RotateProxyCA(confdir))
	assert.Equal(t, before.Gen, previousProxyCAGen(confdir))
	gens := listGenDirs(t, confdir)
	assert.Contains(t, gens, before.Gen)
	assert.Contains(t, gens, CurrentProxyCA().Gen)
	assert.Len(t, gens, 2)
}

func TestStartProxyCARotationWatcher(t *testing.T) {
	resetProxyCAState(t)

	confdir := t.TempDir()
	require.NoError(t, ensureProxyCA(confdir))
	gen1 := CurrentProxyCA()
	require.NotNil(t, gen1)

	oldInterval := proxyCARotatePollInterval
	proxyCARotatePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { proxyCARotatePollInterval = oldInterval })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartProxyCARotationWatcher(ctx, confdir)

	// 空触发文件 = 正常轮转。
	require.NoError(t, os.WriteFile(filepath.Join(confdir, proxyCARotateTriggerFile), nil, 0o644))
	require.Eventually(t, func() bool {
		snap := CurrentProxyCA()
		return snap != nil && snap.Fingerprint != gen1.Fingerprint
	}, 5*time.Second, 10*time.Millisecond)
	_, err := os.Stat(filepath.Join(confdir, proxyCARotateTriggerFile))
	assert.True(t, os.IsNotExist(err), "触发文件执行后被删除")

	// 触发文件内容为已存在代际名 = 回滚到该代际。
	require.NoError(t, os.WriteFile(filepath.Join(confdir, proxyCARotateTriggerFile), []byte(gen1.Gen+"\n"), 0o644))
	require.Eventually(t, func() bool {
		snap := CurrentProxyCA()
		return snap != nil && snap.Fingerprint == gen1.Fingerprint
	}, 5*time.Second, 10*time.Millisecond)

	// 快照刷新先于 status 落盘，status 断言需等待本轮回滚的记录。
	var status proxyCARotateStatus
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(confdir, proxyCARotateStatusFile))
		if err != nil {
			return false
		}
		if err := json.Unmarshal(data, &status); err != nil {
			return false
		}

		return status.Success && status.Rollback && status.Generation == gen1.Gen
	}, 5*time.Second, 10*time.Millisecond)
}
