//go:build mooncake && linux && cgo

package targetstore

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/kvcache-ai/Mooncake/mooncake-store/go/mooncakestore"
)

const (
	// 与 KASandbox 运行时 packages/shared/pkg/storage/storage_mooncake.go 中
	// MOONCAKE_GLOBAL_SEGMENT_SIZE / MOONCAKE_LOCAL_BUFFER_SIZE 的默认值逐字
	// 一致(1 GiB / 128 MiB)。两者是 Mooncake 客户端的资源参数,不影响写出
	// 的数据布局;实际取值优先读同名环境变量,常量只是未设置时的兜底,
	// 保证同一主机上迁移工具与运行时行为一致。
	defaultMooncakeGlobalSegmentSize = 1_073_741_824
	defaultMooncakeLocalBufferSize   = 134_217_728
)

type nativeMooncakeClient struct {
	store *mooncakestore.Store
}

func (c *nativeMooncakeClient) Put(key string, value []byte) error {
	// PutFrom 只接受通过 RegisterBuffer 注册的内存。首版沿用 E2B StoreFile 当前
	// 使用的 Put 路径，避免把 NUMA buffer 生命周期带进一次性迁移命令。
	return c.store.Put(key, value, nil)
}

func (c *nativeMooncakeClient) Exists(key string) (bool, error) {
	return c.store.Exists(key)
}

func (c *nativeMooncakeClient) Close() {
	c.store.Close()
}

// NewMooncake 使用与 E2B storage_mooncake.go 相同的 Job 环境变量和默认值。
// namespace 只负责 key 前缀，连接参数由运行该命令的 Job 环境统一提供。
func NewMooncake(ctx context.Context, namespace string) (Store, error) {
	globalSegmentSize, err := mooncakeUint64("MOONCAKE_GLOBAL_SEGMENT_SIZE", defaultMooncakeGlobalSegmentSize)
	if err != nil {
		return nil, err
	}
	localBufferSize, err := mooncakeUint64("MOONCAKE_LOCAL_BUFFER_SIZE", defaultMooncakeLocalBufferSize)
	if err != nil {
		return nil, err
	}
	mountSegmentSize, err := mooncakeUint64("MOONCAKE_MOUNT_SEGMENT_SIZE", 0)
	if err != nil {
		return nil, err
	}
	localHostname := mooncakeEnv("MOONCAKE_LOCAL_HOSTNAME", "localhost")
	metadataServer := mooncakeEnv("MOONCAKE_METADATA_SERVER", "http://localhost:8080/metadata")
	protocol := mooncakeEnv("MOONCAKE_PROTOCOL", "tcp")
	deviceName := mooncakeEnv("MOONCAKE_DEVICE_NAME", "")
	masterAddr := mooncakeEnv("MOONCAKE_MASTER_ADDR", "localhost:50051")

	// native Setup 内部的控制面连接不接受 context，端点不可达时可能长期阻塞在
	// CGO 调用中。先做一次短 TCP 预检，让地址错误和网络故障在进入 native 层前返回。
	if err := preflightMooncake(ctx, metadataServer, masterAddr); err != nil {
		return nil, err
	}

	store, err := mooncakestore.New()
	if err != nil {
		return nil, fmt.Errorf("create Mooncake store: %w", err)
	}
	if err := store.Setup(
		localHostname,
		metadataServer,
		globalSegmentSize,
		localBufferSize,
		protocol,
		deviceName,
		masterAddr,
	); err != nil {
		store.Close()
		return nil, fmt.Errorf("setup Mooncake store: %w", err)
	}
	if mountSegmentSize > 0 {
		if err := store.InitAll(protocol, deviceName, mountSegmentSize); err != nil {
			store.Close()
			return nil, fmt.Errorf("initialize Mooncake segments: %w", err)
		}
	}

	return newMooncakeStore(&nativeMooncakeClient{store: store}, namespace), nil
}

func mooncakeEnv(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func mooncakeUint64(name string, fallback uint64) (uint64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}
