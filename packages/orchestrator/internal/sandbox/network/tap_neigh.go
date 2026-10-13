package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// tap 设备的 ARP 快速重传。
//
// Firecracker 打开 tap 设备后载波恢复，但发送队列要等内核 linkwatch 工作队列
// 异步激活（实测滞后 7~26ms，并发创建越多越长）。编排器在 resume 之后几毫秒就向
// envd 发 /init，netns 内指向客户机的邻居项此时尚未解析，首个 ARP 请求撞上未激活
// 的队列被丢弃（kfree_skb reason=QDISC_DROP）。内核默认 retrans_time_ms=1000，
// 于是这次沙箱创建白等约 1s：100 并发创建时约每 6 批出现一次，每次 1~28 个沙箱。
// 应用层重试帮不上忙——后续 SYN 只会排在同一个未解析的邻居项后面。
//
// 把 tap 设备的重传间隔调短后，丢掉首个 ARP 只多等十几毫秒。探测次数同步放大，
// 使「多久无应答才判 FAILED」的总窗口与内核默认的 3 次 × 1s 大致相同，不改变
// 客户机卡顿时的容忍度。只写该 netns 内 tap 设备自己的参数，不动全局默认值
// （全局默认值会被内核同步到宿主机现有网卡，包括物理网卡）。

// procSysNetIPv4Neigh 下的内容按调用线程所在的 netns 解析，
// 调用方必须已 LockOSThread 并切入目标 netns。
const procSysNetIPv4Neigh = "/proc/sys/net/ipv4/neigh"

type neighSysctl struct {
	name  string
	value string
}

// tapNeighSysctls 返回要写入的邻居参数；任一配置项非正数表示保持内核默认值。
// 先放大探测次数再缩短间隔，中途不会出现「短间隔 × 3 次」的过短窗口。
func tapNeighSysctls(retransTimeMs, probes int) []neighSysctl {
	if retransTimeMs <= 0 || probes <= 0 {
		return nil
	}

	return []neighSysctl{
		{name: "mcast_solicit", value: strconv.Itoa(probes)},
		{name: "ucast_solicit", value: strconv.Itoa(probes)},
		{name: "retrans_time_ms", value: strconv.Itoa(retransTimeMs)},
	}
}

// applyNeighSysctls 把 sysctls 写入 dir；任何一步失败都把已写入的项恢复为写入前
// 的值，避免留下「只改了一半」的组合（例如 190 次 × 1s 会把判失败窗口拉长到
// 三分钟）。
func applyNeighSysctls(dir string, sysctls []neighSysctl, write func(path, value string) error) error {
	previous := make([]neighSysctl, 0, len(sysctls))

	for _, sysctl := range sysctls {
		path := filepath.Join(dir, sysctl.name)

		current, err := os.ReadFile(path)
		if err == nil {
			err = write(path, sysctl.value)
		}

		if err != nil {
			errs := []error{fmt.Errorf("error setting %s to %s: %w", path, sysctl.value, err)}

			for _, restore := range previous {
				restoreErr := write(filepath.Join(dir, restore.name), restore.value)
				if restoreErr != nil {
					errs = append(errs, fmt.Errorf("error restoring %s to %s: %w", restore.name, restore.value, restoreErr))
				}
			}

			return errors.Join(errs...)
		}

		previous = append(previous, neighSysctl{name: sysctl.name, value: strings.TrimSpace(string(current))})
	}

	return nil
}

// tuneTapNeighbor 调短当前 netns 内 tap 设备的 ARP 重传间隔。失败只告警：
// 内核默认值仍然可用，代价是回到偶发的 1s 启动延迟。
func (s *Slot) tuneTapNeighbor(ctx context.Context) {
	sysctls := tapNeighSysctls(s.config.TapNeighRetransTimeMs, s.config.TapNeighProbes)
	if len(sysctls) == 0 {
		return
	}

	err := applyNeighSysctls(filepath.Join(procSysNetIPv4Neigh, s.TapName()), sysctls, func(path, value string) error {
		return os.WriteFile(path, []byte(value), 0o644)
	})
	if err != nil {
		logger.L().Warn(ctx, "failed to tune ARP retransmission on the sandbox tap device, kernel defaults stay in effect",
			zap.String("namespace", s.NamespaceID()),
			zap.Error(err))
	}
}
