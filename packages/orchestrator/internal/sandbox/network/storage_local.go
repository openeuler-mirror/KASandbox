package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type StorageLocal struct {
	config       Config
	self         processIdentity
	slotsSize    int
	foreignNs    map[string]struct{}
	acquiredNs   map[string]struct{}
	acquiredNsMu sync.Mutex
}

const netNamespacesDir = "/var/run/netns"

func NewStorageLocal(ctx context.Context, config Config) (*StorageLocal, error) {
	self, err := currentProcessIdentity()
	if err != nil {
		return nil, fmt.Errorf("error reading own process identity: %w", err)
	}

	// get namespaces that we want to always skip
	foreignNs, orphanedSlots, err := scanNamespaces(ctx, self)
	if err != nil {
		return nil, fmt.Errorf("error getting already used namespaces: %w", err)
	}

	foreignNsMap := make(map[string]struct{})
	for _, ns := range foreignNs {
		foreignNsMap[ns] = struct{}{}
		logger.L().Info(ctx, fmt.Sprintf("Found foreign namespace: %s", ns))
	}

	s := &StorageLocal{
		config:       config,
		self:         self,
		foreignNs:    foreignNsMap,
		slotsSize:    vrtSlotsSize,
		acquiredNs:   make(map[string]struct{}, vrtSlotsSize),
		acquiredNsMu: sync.Mutex{},
	}

	if len(orphanedSlots) > 0 {
		// 每个槽位的回收要跑十余次 iptables，串行约 0.4s；放到后台逐个做，
		// 不阻塞启动。回收前这些槽位留在 foreignNs 里不会被分配。
		go s.reclaimOrphanedSlots(ctx, orphanedSlots)
	}

	return s, nil
}

func (s *StorageLocal) Acquire(ctx context.Context) (*Slot, error) {
	spanCtx, span := tracer.Start(ctx, "network-namespace-acquire")
	defer span.End()

	acquireTimeoutCtx, acquireCancel := context.WithTimeout(spanCtx, time.Millisecond*500)
	defer acquireCancel()

	s.acquiredNsMu.Lock()
	defer s.acquiredNsMu.Unlock()

	// we skip the first slot because it's the host slot
	slotIdx := 1

	for {
		select {
		case <-acquireTimeoutCtx.Done():
			return nil, fmt.Errorf("failed to acquire IP slot: timeout")
		default:
			if len(s.acquiredNs) > s.slotsSize {
				return nil, fmt.Errorf("failed to acquire IP slot: no empty slots found")
			}

			slotIdx++
			slotName := getSlotName(slotIdx)

			// skip the slot if it's already in use by foreign program
			if _, found := s.foreignNs[slotName]; found {
				continue
			}

			// skip the slot if it's already acquired
			if _, found := s.acquiredNs[slotName]; found {
				continue
			}

			// check if the slot can be acquired
			available, err := isNamespaceAvailable(slotName)
			if err != nil {
				return nil, fmt.Errorf("error checking if namespace is available: %w", err)
			}

			if !available {
				s.foreignNs[slotName] = struct{}{}
				logger.L().Debug(ctx, "Skipping slot because not available", zap.String("slot", slotName))

				continue
			}

			s.acquiredNs[slotName] = struct{}{}
			slotKey := getLocalKey(slotIdx)

			// 归属标记写失败不影响本次分配，只是该槽位在本进程异常退出后无法被下一代自动回收
			if err := writeSlotOwner(slotName, s.self); err != nil {
				logger.L().Warn(ctx, "Failed to write slot owner marker", zap.String("slot", slotName), zap.Error(err))
			}

			return NewSlot(slotKey, slotIdx, s.config)
		}
	}
}

func (s *StorageLocal) Release(ips *Slot) error {
	s.acquiredNsMu.Lock()
	defer s.acquiredNsMu.Unlock()

	slotName := getSlotName(ips.Idx)
	delete(s.acquiredNs, slotName)

	if err := removeSlotOwner(slotName); err != nil {
		logger.L().Warn(context.Background(), "Failed to remove slot owner marker", zap.String("slot", slotName), zap.Error(err))
	}

	return nil
}

// reclaimOrphanedSlots 回收上一代编排器遗留、owner 已不存在的槽位：删除其
// iptables 规则、路由、veth 与 netns，成功后从 foreignNs 移出供分配。
// 上一代进程已死，其沙箱本就处于无人管理状态，回收不会影响任何在管的沙箱。
func (s *StorageLocal) reclaimOrphanedSlots(ctx context.Context, orphaned []int) {
	logger.L().Info(ctx, "Reclaiming orphaned network slots left by a previous orchestrator instance", zap.Int("count", len(orphaned)))

	reclaimed := 0
	for _, idx := range orphaned {
		if ctx.Err() != nil {
			return
		}

		slotName := getSlotName(idx)

		slot, err := NewSlot(getLocalKey(idx), idx, s.config)
		if err != nil {
			logger.L().Warn(ctx, "Cannot rebuild orphaned slot for reclaim, leaving it as foreign", zap.String("slot", slotName), zap.Error(err))

			continue
		}

		// 部分规则可能早已不存在，RemoveNetwork 会把这些错误一并返回；
		// 只要 netns 最终被卸掉就视为回收成功
		rmErr := slot.RemoveNetwork()

		if err := removeSlotOwner(slotName); err != nil {
			logger.L().Warn(ctx, "Failed to remove slot owner marker", zap.String("slot", slotName), zap.Error(err))
		}

		if _, statErr := os.Stat(filepath.Join(netNamespacesDir, slotName)); statErr == nil {
			logger.L().Warn(ctx, "Orphaned slot still present after reclaim, leaving it as foreign", zap.String("slot", slotName), zap.Error(rmErr))

			continue
		}

		if rmErr != nil {
			logger.L().Debug(ctx, "Reclaimed orphaned slot with partial cleanup errors", zap.String("slot", slotName), zap.Error(rmErr))
		}

		s.acquiredNsMu.Lock()
		delete(s.foreignNs, slotName)
		s.acquiredNsMu.Unlock()

		reclaimed++
	}

	logger.L().Info(ctx, "Finished reclaiming orphaned network slots", zap.Int("reclaimed", reclaimed), zap.Int("total", len(orphaned)))
}

func isNamespaceAvailable(name string) (bool, error) {
	nsPath := filepath.Join(netNamespacesDir, name)
	_, err := os.Stat(nsPath)

	if os.IsNotExist(err) {
		// Namespace does not exist, so it's available
		return true, nil
	} else if err != nil {
		// Some other error
		return false, err
	}

	// 路径存在不代表是有效 namespace：异常退出可能留下普通文件或失效挂载点。
	// 失效残留清理后槽位即可复用，而不是在本进程生命周期内永久跳过。
	valid, err := isValidNetNSMount(nsPath)
	if err != nil {
		return false, fmt.Errorf("error validating namespace mount: %w", err)
	}

	if valid {
		// File exists and is a live namespace mount, so it's in use.
		return false, nil
	}

	if rmErr := deleteNamedNamespace(name); rmErr != nil {
		return false, fmt.Errorf("error removing stale namespace leftover %s: %w", name, rmErr)
	}

	return true, nil
}

// isValidNetNSMount 校验路径是有效的 netns 挂载点。
// 判定方式与 mountpoint(1) 一致：挂载点（nsfs）的设备号与父目录（tmpfs）不同；
// 编排器异常退出后残留的普通文件与父目录同设备，可据此区分。
// 注意：仍被挂载着的 netns 在此判定下是「有效」的，即便已没有任何进程使用；
// 这类上一代遗留的槽位由归属标记（slot_owner.go）识别并在启动时回收。
func isValidNetNSMount(path string) (bool, error) {
	// nsfs 与 /run 的 tmpfs 是不同的设备号；普通残留文件与父目录同设备
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false, err
	}

	var parentSt unix.Stat_t
	if err := unix.Stat(filepath.Dir(path), &parentSt); err != nil {
		return false, err
	}

	return st.Dev != parentSt.Dev, nil
}

// isSlotNamespaceName 判断是否为本组件管理的槽位命名（ns-<数字>）。
// 其他命名（如 host、cni-*）一律视为外部程序的 namespace，不做任何清理。
func isSlotNamespaceName(name string) bool {
	_, err := parseSlotIndex(name)

	return err == nil
}

func parseSlotIndex(name string) (int, error) {
	rest, ok := strings.CutPrefix(name, "ns-")
	if !ok {
		return 0, fmt.Errorf("not a slot namespace name: %q", name)
	}

	return strconv.Atoi(rest)
}

// scanNamespaces 扫描 netns 目录，返回需要跳过的 foreign namespace 列表，以及
// 上一代编排器遗留、可回收的槽位下标。
//
// 对本组件命名（ns-N）的有效 netns 挂载，按归属标记判定：
//   - 没有标记：可能是其他工具或旧版本编排器创建，保守视为 foreign
//   - 标记的 owner 进程仍存活：另一个编排器实例在用，视为 foreign
//   - 标记的 owner 已不存在：上一代遗留，加入回收列表
func scanNamespaces(ctx context.Context, self processIdentity) (foreign []string, orphaned []int, err error) {
	files, err := os.ReadDir(netNamespacesDir)
	if err != nil {
		// Folder does not exist, so we can assume no namespaces are in use
		if os.IsNotExist(err) {
			return nil, nil, nil
		}

		return nil, nil, fmt.Errorf("error reading netns directory: %w", err)
	}

	liveSlots := make(map[string]struct{})

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		name := file.Name()
		if name == "host" {
			continue
		}

		if !isSlotNamespaceName(name) {
			foreign = append(foreign, name)

			continue
		}

		// 本组件槽位命名的条目：校验有效性；异常退出遗留的死文件/失效挂载点
		// 直接清理，不标记为 foreign（否则槽位在进程生命周期内被永久跳过）
		valid, validErr := isValidNetNSMount(filepath.Join(netNamespacesDir, name))
		if validErr != nil {
			// 保守处理：校验失败仍视为 foreign，避免误删在用 namespace
			logger.L().Warn(ctx, "Cannot validate namespace, treating as foreign", zap.String("namespace", name), zap.Error(validErr))
			foreign = append(foreign, name)

			continue
		}

		if !valid {
			logger.L().Info(ctx, "Removing stale namespace leftover", zap.String("namespace", name))
			if rmErr := deleteNamedNamespace(name); rmErr != nil {
				logger.L().Warn(ctx, "Failed to remove stale namespace leftover", zap.String("namespace", name), zap.Error(rmErr))
			}
			if rmErr := removeSlotOwner(name); rmErr != nil {
				logger.L().Warn(ctx, "Failed to remove slot owner marker", zap.String("slot", name), zap.Error(rmErr))
			}

			continue
		}

		liveSlots[name] = struct{}{}

		owner, found, ownerErr := readSlotOwner(name)
		if ownerErr != nil {
			logger.L().Warn(ctx, "Cannot read slot owner marker, treating namespace as foreign", zap.String("namespace", name), zap.Error(ownerErr))
			foreign = append(foreign, name)

			continue
		}

		if !found || owner == self || owner.alive() {
			foreign = append(foreign, name)

			continue
		}

		idx, _ := parseSlotIndex(name)
		orphaned = append(orphaned, idx)
	}

	// 有标记但 netns 已不存在（分配后尚未建网就退出，或已被清理）的孤儿标记，owner 已死则清掉
	if markers, readErr := os.ReadDir(slotOwnerDir); readErr == nil {
		for _, m := range markers {
			name := m.Name()
			if _, live := liveSlots[name]; live || !isSlotNamespaceName(name) {
				continue
			}
			owner, found, ownerErr := readSlotOwner(name)
			if ownerErr != nil || !found || owner.alive() {
				continue
			}
			if rmErr := removeSlotOwner(name); rmErr != nil {
				logger.L().Warn(ctx, "Failed to remove stale slot owner marker", zap.String("slot", name), zap.Error(rmErr))
			}
		}
	}

	return foreign, orphaned, nil
}

func getSlotName(slotIdx int) string {
	slotIdxStr := strconv.Itoa(slotIdx)

	return fmt.Sprintf("ns-%s", slotIdxStr)
}

func getLocalKey(slotIdx int) string {
	return strconv.Itoa(slotIdx)
}
