//go:build mooncake && linux && cgo

package targetstore

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/kvcache-ai/Mooncake/mooncake-store/go/mooncakestore"
)

type nativeMooncakeClient struct {
	store  *mooncakestore.Store
	buffer []byte
}

func (c *nativeMooncakeClient) Put(key string, value []byte) error {
	return c.store.Put(key, value, nil)
}

func (c *nativeMooncakeClient) Exists(key string) (bool, error) { return c.store.Exists(key) }

func (c *nativeMooncakeClient) Get(key string, limit int64) ([]byte, error) {
	size, err := c.store.GetSize(key)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > limit {
		return nil, fmt.Errorf("size %d exceeds read bound %d", size, limit)
	}
	if size == 0 {
		return []byte{}, nil
	}
	if int64(len(c.buffer)) < size {
		if len(c.buffer) > 0 {
			if err := c.store.UnregisterBuffer(uintptr(unsafe.Pointer(&c.buffer[0]))); err != nil {
				return nil, err
			}
			if err := syscall.Munmap(c.buffer); err != nil {
				return nil, err
			}
			c.buffer = nil
		}
		c.buffer, err = syscall.Mmap(-1, 0, int(max(size, mooncakeChunkSize)), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			return nil, fmt.Errorf("allocate Mooncake transfer buffer: %w", err)
		}
		if err := c.store.RegisterBuffer(uintptr(unsafe.Pointer(&c.buffer[0])), uint64(len(c.buffer))); err != nil {
			_ = syscall.Munmap(c.buffer)
			c.buffer = nil
			return nil, fmt.Errorf("register Mooncake transfer buffer: %w", err)
		}
	}
	count, err := c.store.GetInto(key, uintptr(unsafe.Pointer(&c.buffer[0])), uint64(size))
	if err != nil {
		return nil, err
	}
	if count != size {
		return nil, fmt.Errorf("short read: got %d bytes, want %d", count, size)
	}
	// The caller consumes this view before the next read; this is registered
	// mmap memory, not a Go heap slice passed to an asynchronous transport.
	return c.buffer[:int(size)], nil
}

func (c *nativeMooncakeClient) Close() {
	if c.store == nil {
		return
	}
	if len(c.buffer) > 0 {
		_ = c.store.UnregisterBuffer(uintptr(unsafe.Pointer(&c.buffer[0])))
	}
	// Keep mmap alive through Close even if deregistration failed.
	c.store.Close()
	c.store = nil
	if len(c.buffer) > 0 {
		_ = syscall.Munmap(c.buffer)
		c.buffer = nil
	}
}

func NewMooncake(ctx context.Context, namespace string) (Store, error) {
	config, err := readMooncakeConfig()
	if err != nil {
		return nil, err
	}
	open := func(ctx context.Context) (mooncakeClient, error) {
		if err := preflightMooncake(ctx, config.metadataServer, config.masterAddr); err != nil {
			return nil, err
		}
		store, err := mooncakestore.New()
		if err != nil {
			return nil, fmt.Errorf("create Mooncake store: %w", err)
		}
		// Always zero storage capacity; never call InitAll in this tool.
		if err := store.Setup(config.localHostname, config.metadataServer, 0, config.localBufferSize,
			config.protocol, config.deviceName, config.masterAddr); err != nil {
			store.Close()
			return nil, fmt.Errorf("setup Mooncake store: %w", err)
		}
		return &nativeMooncakeClient{store: store}, nil
	}
	client, err := open(ctx)
	if err != nil {
		return nil, err
	}
	result := newMooncakeStore(client, namespace)
	result.reopen = open
	return result, nil
}
