package storage

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Mooncake returns a value only whole, and a compressed frame (2 MiB
// uncompressed, about 1 MiB stored) is much smaller than a chunk, so without a
// cache every frame read fetches its 4 MiB chunk again: on cold reads the
// chunks of a compressed object were fetched four to six times. The cache
// keeps recently read chunks in memory, shared by all objects of the process,
// and lets concurrent misses of one chunk share a single read.
//
// Entries are never modified. Writes and deletes of chunks in this process
// invalidate them after the change, which also keeps reads in flight from
// being cached; like the metadata cache, a rewrite by another node is not seen
// until restart.
const defaultMooncakeChunkCacheMB = 1024

type mooncakeChunkCache struct {
	maxBytes int64

	mu       sync.Mutex
	bytes    int64
	lru      *list.List // front is the most recently used; values are *mooncakeCachedChunk
	entries  map[string]*list.Element
	inflight map[string]*mooncakeChunkRead
}

type mooncakeCachedChunk struct {
	key  string
	data []byte
}

type mooncakeChunkRead struct {
	done  chan struct{}
	data  []byte
	err   error
	stale bool // invalidated while in flight: not cached
}

// newMooncakeChunkCache returns a cache of at most maxBytes; nil (no cache)
// if maxBytes is not positive.
func newMooncakeChunkCache(maxBytes int64) *mooncakeChunkCache {
	if maxBytes <= 0 {
		return nil
	}

	return &mooncakeChunkCache{
		maxBytes: maxBytes,
		lru:      list.New(),
		entries:  map[string]*list.Element{},
		inflight: map[string]*mooncakeChunkRead{},
	}
}

var (
	sharedMooncakeChunkCacheOnce sync.Once
	sharedMooncakeChunkCacheInst *mooncakeChunkCache
)

// sharedMooncakeChunkCache is the cache of all Mooncake providers of the
// process, sized by MOONCAKE_CHUNK_CACHE_MB (0 turns it off).
func sharedMooncakeChunkCache() *mooncakeChunkCache {
	sharedMooncakeChunkCacheOnce.Do(func() {
		mb, err := env.GetEnvAsInt("MOONCAKE_CHUNK_CACHE_MB", defaultMooncakeChunkCacheMB)
		if err != nil || mb < 0 {
			logger.L().Warn(context.Background(), "invalid MOONCAKE_CHUNK_CACHE_MB, using the default",
				zap.Int("default_mb", defaultMooncakeChunkCacheMB), zap.Error(err))
			mb = defaultMooncakeChunkCacheMB
		}
		sharedMooncakeChunkCacheInst = newMooncakeChunkCache(int64(mb) << 20)
		logger.L().Info(context.Background(), "Mooncake chunk cache", zap.Int("size_mb", mb))
	})

	return sharedMooncakeChunkCacheInst
}

// get returns the value of key, at most size bytes, from the cache or read
// through kv. Only values of exactly size bytes are kept; the caller checks
// the length. The returned slice must not be modified.
func (c *mooncakeChunkCache) get(ctx context.Context, kv mooncakeKV, key string, size int64) ([]byte, error) {
	for {
		data, read, leader := c.lookup(key, size, true)
		if data != nil {
			return data, nil
		}

		if leader {
			buf := make([]byte, size)
			n, err := kv.GetInto(ctx, key, buf)
			c.finish(key, read, size, buf[:n], err)

			return buf[:n], err
		}

		select {
		case <-read.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if read.err == nil {
			return read.data, nil
		}
		// The reader gave up on its own context: read again unless this
		// caller is done too.
		if isContextErr(read.err) && ctx.Err() == nil {
			continue
		}

		return nil, read.err
	}
}

// peek returns key if it is cached with the given size, or waits for a read of
// it already in flight. It reports false if neither; the caller then reads it
// itself without caching it.
func (c *mooncakeChunkCache) peek(ctx context.Context, key string, size int64) ([]byte, bool, error) {
	data, read, _ := c.lookup(key, size, false)
	if data != nil {
		return data, true, nil
	}
	if read == nil {
		return nil, false, nil
	}

	select {
	case <-read.done:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	if read.err == nil && int64(len(read.data)) == size {
		return read.data, true, nil
	}

	return nil, false, nil
}

// lookup returns the cached value, or the read in flight. With start set and
// neither present it registers a new read and reports the caller as its
// leader, which must call finish.
func (c *mooncakeChunkCache) lookup(key string, size int64, start bool) ([]byte, *mooncakeChunkRead, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		chunk := el.Value.(*mooncakeCachedChunk)
		if int64(len(chunk.data)) == size {
			c.lru.MoveToFront(el)

			return chunk.data, nil, false
		}
		c.removeLocked(el)
	}

	if read, ok := c.inflight[key]; ok {
		return nil, read, false
	}
	if !start {
		return nil, nil, false
	}

	read := &mooncakeChunkRead{done: make(chan struct{})}
	c.inflight[key] = read

	return nil, read, true
}

func (c *mooncakeChunkCache) finish(key string, read *mooncakeChunkRead, size int64, data []byte, err error) {
	c.mu.Lock()
	delete(c.inflight, key)
	if err == nil && !read.stale && int64(len(data)) == size && size <= c.maxBytes {
		c.addLocked(key, data)
	}
	c.mu.Unlock()

	read.data, read.err = data, err
	close(read.done)
}

func (c *mooncakeChunkCache) addLocked(key string, data []byte) {
	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
	c.entries[key] = c.lru.PushFront(&mooncakeCachedChunk{key: key, data: data})
	c.bytes += int64(len(data))

	for c.bytes > c.maxBytes {
		c.removeLocked(c.lru.Back())
	}
}

func (c *mooncakeChunkCache) removeLocked(el *list.Element) {
	chunk := c.lru.Remove(el).(*mooncakeCachedChunk)
	delete(c.entries, chunk.key)
	c.bytes -= int64(len(chunk.data))
}

// invalidate drops key and keeps a read of it in flight from being cached.
// Call it after the key was written or removed.
func (c *mooncakeChunkCache) invalidate(key string) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
	if read, ok := c.inflight[key]; ok {
		read.stale = true
	}
}

// invalidatePrefix is invalidate for every key starting with prefix.
func (c *mooncakeChunkCache) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if strings.HasPrefix(el.Value.(*mooncakeCachedChunk).key, prefix) {
			c.removeLocked(el)
		}
		el = next
	}
	for key, read := range c.inflight {
		if strings.HasPrefix(key, prefix) {
			read.stale = true
		}
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
