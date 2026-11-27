package storage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingMooncakeKV counts reads per key and, while hold is set, holds reads
// of chunk values until hold is closed or the reader's context ends.
type countingMooncakeKV struct {
	*fakeMooncakeKV

	mu      sync.Mutex
	gets    map[string]int
	hold    chan struct{}
	started chan string // gets the key of each held read
}

func newCountingMooncakeKV() *countingMooncakeKV {
	return &countingMooncakeKV{fakeMooncakeKV: newFakeMooncakeKV(), gets: map[string]int{}}
}

func (k *countingMooncakeKV) GetInto(ctx context.Context, key string, dst []byte) (int, error) {
	k.mu.Lock()
	k.gets[key]++
	hold, started := k.hold, k.started
	k.mu.Unlock()

	if hold != nil && strings.Contains(key, mooncakeChunkSeparator) {
		if started != nil {
			started <- key
		}
		select {
		case <-hold:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	return k.fakeMooncakeKV.GetInto(ctx, key, dst)
}

func (k *countingMooncakeKV) holdReads() (release func()) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.hold = make(chan struct{})
	k.started = make(chan string, 16)

	return func() { close(k.hold) }
}

func (k *countingMooncakeKV) count(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.gets[key]
}

func (k *countingMooncakeKV) chunkGets() map[string]int {
	k.mu.Lock()
	defer k.mu.Unlock()

	gets := map[string]int{}
	for key, n := range k.gets {
		if strings.Contains(key, mooncakeChunkSeparator) {
			gets[key] = n
		}
	}

	return gets
}

func newCountingMooncake(t *testing.T, chunks *mooncakeChunkCache) (*mooncakeStorage, *countingMooncakeKV) {
	t.Helper()

	kv := newCountingMooncakeKV()
	bucket := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())

	return newMooncakeStorageWithKV(kv, chunks, bucket, "", nil), kv
}

// Frames are smaller than chunks: reading every frame of a compressed object
// fetches each chunk once with the cache, several times without it.
func TestMooncakeChunkCacheFramesShareChunks(t *testing.T) {
	t.Parallel()

	for _, cached := range []bool{true, false} {
		name := map[bool]string{true: "cached", false: "uncached"}[cached]
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var chunks *mooncakeChunkCache
			if cached {
				chunks = newMooncakeChunkCache(16 * mooncakeChunkSize)
			}
			p, kv := newCountingMooncake(t, chunks)
			data := testData(t, 3*mooncakeChunkSize+777)

			obj, err := p.OpenSeekable(t.Context(), "build/rootfs.ext4.lz4")
			require.NoError(t, err)
			fft, _, err := obj.StoreFile(t.Context(), writeMooncakeTempFile(t, data),
				WithCompressConfig(CompressConfig{Enabled: true, Type: "lz4", Level: 1, MinPartSizeMB: 5}))
			require.NoError(t, err)
			ft := fft.Table()
			require.Greater(t, ft.NumFrames(), int(ft.CompressedSize()/mooncakeChunkSize)+1, "more frames than chunks")

			for i := range ft.NumFrames() {
				startU, endU, _, _ := ft.FrameAt(i)
				assert.Equal(t, data[startU:endU], readRange(t, obj, startU, endU-startU, ft), "frame %d", i)
			}

			gets := kv.chunkGets()
			require.NotEmpty(t, gets)
			most := 0
			for _, n := range gets {
				most = max(most, n)
			}
			if cached {
				assert.Equal(t, 1, most, "each chunk read once: %v", gets)
			} else {
				assert.Greater(t, most, 1, "chunks read again per frame: %v", gets)
			}
		})
	}
}

// Whole-chunk reads (uncompressed data) are not cached, but use a chunk that
// is.
func TestMooncakeChunkCacheWholeChunkReads(t *testing.T) {
	t.Parallel()

	chunks := newMooncakeChunkCache(16 * mooncakeChunkSize)
	p, kv := newCountingMooncake(t, chunks)
	data := testData(t, 2*mooncakeChunkSize+100)

	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, data))
	require.NoError(t, err)

	assert.Equal(t, data[:mooncakeChunkSize], readRange(t, obj, 0, mooncakeChunkSize, nil))
	assert.Zero(t, chunks.bytes, "a whole-chunk read is not cached")

	// A partial read caches the second chunk; a whole read of it then uses it.
	second := p.key("build/memfile") + mooncakeChunkSeparator + "4194304"
	assert.Equal(t, data[mooncakeChunkSize+10:mooncakeChunkSize+20], readRange(t, obj, mooncakeChunkSize+10, 10, nil))
	assert.Equal(t, data[mooncakeChunkSize:2*mooncakeChunkSize], readRange(t, obj, mooncakeChunkSize, mooncakeChunkSize, nil))
	assert.Equal(t, 1, kv.count(second))
}

func TestMooncakeChunkCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	kv := newCountingMooncakeKV()
	for _, key := range []string{"a#c#0", "b#c#0", "c#c#0"} {
		require.NoError(t, kv.Put(key, []byte(key)))
	}
	chunks := newMooncakeChunkCache(10) // two 5-byte values

	get := func(key string) {
		t.Helper()
		data, err := chunks.get(t.Context(), kv, key, 5)
		require.NoError(t, err)
		require.Equal(t, key, string(data))
	}
	get("a#c#0")
	get("b#c#0")
	get("a#c#0") // a is now the most recent
	get("c#c#0") // evicts b
	assert.Equal(t, int64(10), chunks.bytes)

	get("a#c#0")
	get("b#c#0")
	assert.Equal(t, 1, kv.count("a#c#0"))
	assert.Equal(t, 2, kv.count("b#c#0"))
}

func TestMooncakeChunkCacheConcurrentMissesShareRead(t *testing.T) {
	t.Parallel()

	kv := newCountingMooncakeKV()
	require.NoError(t, kv.Put("o#c#0", []byte("chunk value")))
	chunks := newMooncakeChunkCache(1 << 20)
	release := kv.holdReads()

	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Go(func() {
			data, err := chunks.get(t.Context(), kv, "o#c#0", 11)
			assert.NoError(t, err)
			results[i] = string(data)
		})
	}
	<-kv.started
	// Let the other readers reach the read in flight.
	time.Sleep(50 * time.Millisecond)
	release()
	wg.Wait()

	for _, r := range results {
		assert.Equal(t, "chunk value", r)
	}
	assert.Equal(t, 1, kv.count("o#c#0"))
}

// A reader that gives up on its context does not fail the others.
func TestMooncakeChunkCacheCanceledReader(t *testing.T) {
	t.Parallel()

	kv := newCountingMooncakeKV()
	require.NoError(t, kv.Put("o#c#0", []byte("value")))
	chunks := newMooncakeChunkCache(1 << 20)
	release := kv.holdReads()

	ctx, cancel := context.WithCancel(t.Context())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := chunks.get(ctx, kv, "o#c#0", 5)
		leaderErr <- err
	}()
	<-kv.started

	waiter := make(chan string, 1)
	go func() {
		data, err := chunks.get(t.Context(), kv, "o#c#0", 5)
		assert.NoError(t, err)
		waiter <- string(data)
	}()
	time.Sleep(50 * time.Millisecond)

	cancel()
	require.ErrorIs(t, <-leaderErr, context.Canceled)
	<-kv.started // the waiter reads again
	release()
	assert.Equal(t, "value", <-waiter)
	assert.Equal(t, 2, kv.count("o#c#0"))
}

// A chunk written or removed while a read of it is in flight is not cached.
func TestMooncakeChunkCacheInvalidateInFlight(t *testing.T) {
	t.Parallel()

	kv := newCountingMooncakeKV()
	require.NoError(t, kv.Put("o#c#0", []byte("old")))
	chunks := newMooncakeChunkCache(1 << 20)
	release := kv.holdReads()

	done := make(chan string, 1)
	go func() {
		data, err := chunks.get(t.Context(), kv, "o#c#0", 3)
		assert.NoError(t, err)
		done <- string(data)
	}()
	<-kv.started
	chunks.invalidate("o#c#0")
	release()
	assert.Equal(t, "old", <-done)
	assert.Zero(t, chunks.bytes)

	require.NoError(t, kv.Remove("o#c#0"))
	require.NoError(t, kv.Put("o#c#0", []byte("new")))
	data, err := chunks.get(t.Context(), kv, "o#c#0", 3)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

// Rewriting and deleting objects drops their cached chunks.
func TestMooncakeChunkCacheRewriteAndDelete(t *testing.T) {
	t.Parallel()

	chunks := newMooncakeChunkCache(16 * mooncakeChunkSize)
	p, _ := newCountingMooncake(t, chunks)
	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)

	first := testData(t, mooncakeChunkSize+500)
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, first))
	require.NoError(t, err)
	assert.Equal(t, first[100:400], readRange(t, obj, 100, 300, nil))
	require.Positive(t, chunks.bytes)

	second := testData(t, mooncakeChunkSize+500)
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, second))
	require.NoError(t, err)
	assert.Equal(t, second[100:400], readRange(t, obj, 100, 300, nil))

	require.NoError(t, p.DeleteObjectsWithPrefix(t.Context(), "build/"))
	assert.Zero(t, chunks.bytes)
	_, _, err = obj.OpenRangeReader(t.Context(), 100, 300, nil)
	require.ErrorIs(t, err, ErrObjectNotExist)
}

func TestMooncakeReadsWithoutChunkCache(t *testing.T) {
	t.Parallel()

	p, _ := newCountingMooncake(t, nil)
	data := testData(t, 2*mooncakeChunkSize+12345)

	plain, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	_, _, err = plain.StoreFile(t.Context(), writeMooncakeTempFile(t, data))
	require.NoError(t, err)
	assert.Equal(t, data, readRange(t, plain, 0, int64(len(data)), nil))
	off := int64(mooncakeChunkSize - 100)
	assert.Equal(t, data[off:off+300], readRange(t, plain, off, 300, nil))

	zst, err := p.OpenSeekable(t.Context(), "build/rootfs.ext4.zstd")
	require.NoError(t, err)
	fft, _, err := zst.StoreFile(t.Context(), writeMooncakeTempFile(t, data),
		WithCompressConfig(CompressConfig{Enabled: true, Type: "zstd", Level: 1, MinPartSizeMB: 5}))
	require.NoError(t, err)
	ft := fft.Table()
	for i := range ft.NumFrames() {
		startU, endU, _, _ := ft.FrameAt(i)
		assert.Equal(t, data[startU:endU], readRange(t, zst, startU, endU-startU, ft), "frame %d", i)
	}
}
