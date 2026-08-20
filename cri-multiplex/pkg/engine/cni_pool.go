package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netns"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// cniPoolPodConfig 是预热时传给 CNI ADD 的占位 PodSandboxConfig。
// bridge/host-local 等插件只把 K8S_* args 当元数据，不影响网络配置本身；
// 关键点是 Add 与后续 Del 必须使用同一个 ContainerID（这里即 warmID），
// host-local IPAM 才能正确释放地址。
var cniPoolPodConfig = &runtime.PodSandboxConfig{
	Metadata: &runtime.PodSandboxMetadata{
		Name:      "cni-pool",
		Namespace: "cri-multiplex",
		Uid:       "cni-pool",
	},
}

// CNI 预热池：后台协程串行执行完整的 CNI ADD（netns 创建 + veth 配对 + IPAM
// 分配），把成品 CNIRecord 放入 ready 队列；RunPodSandbox 命中时直接取用，
// 避开并发创建时 CNI 插件链（host-local IPAM 文件锁、netlink/mount 串行化、
// udev 事件风暴）导致的近线性膨胀（实测 100 并发 p50≈65s、200 并发 p50≈200s）。
//
// 与 orphan reconciler 的关系：池中 netns 从预热开始一直保持 pending 标记，
// 直到被 RunPodSandbox 取用（随其 defer 解除）或在 Close 时释放，因此
// scanOrphanNetNS 不会把池中空闲 netns 当孤儿删除；进程异常退出时这些
// netns 失去 pending 保护，由下次启动的 reconciler 回收。

func (e *grpcE2BEngine) startCNIPool(size int) {
	e.cniPoolReady = make(chan *CNIRecord, size)
	e.cniPoolStop = make(chan struct{})
	e.cniPoolWG.Add(1)
	go e.cniPoolWarmLoop(size)
	log.Printf("[GrpcE2BEngine] CNI pool started: target size=%d", size)
}

func (e *grpcE2BEngine) cniPoolWarmLoop(size int) {
	defer e.cniPoolWG.Done()
	paused := false
	for {
		select {
		case <-e.cniPoolStop:
			return
		default:
		}
		// 有创建请求在途时暂停预热：warm 与 direct CNI ADD 共享 CNI 插件链的
		// 串行资源，预热会拖慢真正在创建的沙箱。轮询等待在途请求清零。
		if atomic.LoadInt64(&e.inflightRunPod) > 0 {
			if !paused {
				log.Printf("[GrpcE2BEngine] CNI pool warm paused: RunPodSandbox in flight")
				paused = true
			}
			select {
			case <-e.cniPoolStop:
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		if paused {
			log.Printf("[GrpcE2BEngine] CNI pool warm resumed")
			paused = false
		}
		if len(e.cniPoolReady) >= size {
			select {
			case <-e.cniPoolStop:
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		rec, err := e.warmCNIPoolEntry()
		if err != nil {
			log.Printf("[GrpcE2BEngine] WARNING: CNI pool warm failed: %v", err)
			select {
			case <-e.cniPoolStop:
				return
			case <-time.After(time.Second):
				continue
			}
		}
		select {
		case e.cniPoolReady <- rec:
			log.Printf("[GrpcE2BEngine] CNI pool warm: netns=%s podIP=%s ready=%d/%d",
				rec.NetNSName, rec.PodIP, len(e.cniPoolReady), size)
		case <-e.cniPoolStop:
			// 关停与预热完成撞车：释放刚预热好的 entry，避免泄漏
			e.unmarkPendingNetNS(rec.NetNSName)
			if err := e.cniManager.Del(context.Background(), rec, cniPoolPodConfig); err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: CNI pool drain on stop failed netns=%s: %v", rec.NetNSName, err)
			}
			return
		}
	}
}

// warmCNIPoolEntry 预热一个池化 entry。pending 标记从这里开始持有，
// 直到 entry 被取用或释放时才解除（见 acquireCNIFromPool / stopCNIPool）。
//
// warmID 约定为 "pool" + 8 位 hex（≤12 字符）：shortID 对 ≤12 字符的 ID 原样
// 返回，因此 netns 名 = 前缀 + warmID，warmID 可以从 netns 名完整还原。
// 进程重启后池内容（内存 channel）即失效，启动时的 cleanupStaleCNIPool
// 依靠这个可逆命名对上一轮遗留 entry 做带正确 ContainerID 的 CNI DEL，
// host-local IPAM 才能正确释放地址。
func (e *grpcE2BEngine) warmCNIPoolEntry() (*CNIRecord, error) {
	seq := atomic.AddInt64(&e.cniPoolSeq, 1)
	warmID := fmt.Sprintf("pool%08x", uint32(time.Now().UnixNano())^uint32(seq))
	e.markPendingNetNS(e.cniManager.NetNSName(warmID))
	rec, err := e.cniManager.Add(context.Background(), warmID, cniPoolPodConfig)
	if err != nil {
		e.unmarkPendingNetNS(e.cniManager.NetNSName(warmID))
		return nil, err
	}
	return rec, nil
}

// acquireCNIFromPool 非阻塞地从池中取一个预热好的 entry；池空或未启用返回 nil。
// 取出的 entry 仍带 pending 标记，由调用方（RunPodSandbox）在使用完毕后解除。
func (e *grpcE2BEngine) acquireCNIFromPool() *CNIRecord {
	if e.cniPoolReady == nil {
		return nil
	}
	select {
	case rec := <-e.cniPoolReady:
		return rec
	default:
		return nil
	}
}

func (e *grpcE2BEngine) stopCNIPool() {
	if e.cniPoolStop == nil {
		return
	}
	close(e.cniPoolStop)
	e.cniPoolWG.Wait()
	// 排空池中未被取用的 entry
	for {
		select {
		case rec := <-e.cniPoolReady:
			e.unmarkPendingNetNS(rec.NetNSName)
			if err := e.cniManager.Del(context.Background(), rec, cniPoolPodConfig); err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: CNI pool drain failed netns=%s: %v", rec.NetNSName, err)
			}
		default:
			return
		}
	}
}

// cleanupStaleCNIPool 在启动时清理上一轮进程遗留的预热池 entry。
// 池内容只在内存中，进程重启后无法复用；无论本轮是否开启池化都要清理
// （上一轮可能开过）。在 cniManager 初始化后、warm 协程与 orphan
// reconciler 启动前同步调用。
//
// 新命名（"pool"+8hex，netns 名 = 前缀+warmID）可还原 warmID，走完整
// CNI DEL（释放 host-local IPAM + 删 veth + 删 netns）；旧命名
// （"pool-<nano>-<seq>" 经 shortID 哈希截断）无法还原 ContainerID，
// 退化为仅删除 netns——与 orphan reconciler 对孤儿 netns 的处理一致，
// 对应 IPAM 记录可能残留。
func (e *grpcE2BEngine) cleanupStaleCNIPool() {
	prefix := e.cniConfig.NetNSPrefix
	if prefix == "" {
		prefix = "e2b-"
	}
	entries, err := os.ReadDir(e.cniConfig.NetNSDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[GrpcE2BEngine] WARNING: scan stale CNI pool netns: %v", err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		suffix, ok := strings.CutPrefix(name, prefix+"pool")
		if !ok {
			continue
		}
		if isHex8(suffix) {
			// 新命名：warmID 可还原，完整 CNI DEL
			rec := &CNIRecord{
				SandboxID: "pool" + suffix,
				NetNSName: name,
				NetNSPath: filepath.Join(e.cniConfig.NetNSDir, name),
			}
			if err := e.cniManager.Del(context.Background(), rec, cniPoolPodConfig); err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: stale CNI pool entry cleanup failed netns=%s: %v", name, err)
				continue
			}
			log.Printf("[GrpcE2BEngine] stale CNI pool entry cleaned: netns=%s", name)
			continue
		}
		// 旧命名：warmID 不可还原，仅删 netns
		if err := netns.DeleteNamed(name); err != nil && !isCleanupNotFound(err) && !os.IsNotExist(err) {
			log.Printf("[GrpcE2BEngine] WARNING: stale CNI pool netns delete failed netns=%s: %v", name, err)
			continue
		}
		log.Printf("[GrpcE2BEngine] stale CNI pool netns removed (legacy naming, IPAM record may leak): netns=%s", name)
	}
}

// isHex8 判断 s 是否为 8 位小写 hex（新命名 warmID 的 suffix）。
func isHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
