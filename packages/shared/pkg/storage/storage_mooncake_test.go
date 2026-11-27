package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
)

// fakeMooncakeKV mimics the Mooncake semantics the provider relies on: no
// overwrite, no empty values, reads limited to 4 MiB.
type fakeMooncakeKV struct {
	mu      sync.Mutex
	data    map[string][]byte
	failPut func(key string) bool
}

func newFakeMooncakeKV() *fakeMooncakeKV {
	return &fakeMooncakeKV{data: map[string][]byte{}}
}

func (f *fakeMooncakeKV) Put(key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failPut != nil && f.failPut(key) {
		return errors.New("injected put failure")
	}
	if _, ok := f.data[key]; ok {
		return errors.New("object already exists")
	}
	if len(value) == 0 {
		return errors.New("empty value")
	}
	f.data[key] = bytes.Clone(value)

	return nil
}

func (f *fakeMooncakeKV) GetInto(_ context.Context, key string, dst []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(dst) == 0 || len(dst) > mooncakeChunkSize {
		return 0, fmt.Errorf("buffer size %d out of range", len(dst))
	}
	v, ok := f.data[key]
	if !ok {
		return 0, ErrObjectNotExist
	}
	if len(v) > len(dst) {
		return 0, fmt.Errorf("value of %d bytes does not fit %d", len(v), len(dst))
	}

	return copy(dst, v), nil
}

func (f *fakeMooncakeKV) GetSize(key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.data[key]
	if !ok {
		return 0, ErrObjectNotExist
	}

	return int64(len(v)), nil
}

func (f *fakeMooncakeKV) Exists(key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.data[key]

	return ok, nil
}

func (f *fakeMooncakeKV) Remove(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.data, key)

	return nil
}

func (f *fakeMooncakeKV) RemoveByRegex(pattern string) (int64, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var n int64
	for k := range f.data {
		if re.MatchString(k) {
			delete(f.data, k)
			n++
		}
	}

	return n, nil
}

func (f *fakeMooncakeKV) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Sorted(maps.Keys(f.data))
}

func (f *fakeMooncakeKV) value(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.data[key]
}

// newTestMooncake returns a provider on a fresh fake; the bucket is unique
// per test because the metadata cache is process-wide.
func newTestMooncake(t *testing.T) (*mooncakeStorage, *fakeMooncakeKV) {
	t.Helper()

	kv := newFakeMooncakeKV()
	bucket := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())

	return newMooncakeStorageWithKV(kv, newMooncakeChunkCache(64*mooncakeChunkSize), bucket, "", nil), kv
}

func writeMooncakeTempFile(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	return path
}

// testData is half random, half repeated text, so it compresses partly.
func testData(t *testing.T, size int) []byte {
	t.Helper()

	data := make([]byte, size)
	_, err := rand.Read(data[:size/2])
	require.NoError(t, err)
	for i := size / 2; i < size; i++ {
		data[i] = "mooncake chunked storage "[i%25]
	}

	return data
}

func readRange(t *testing.T, s Seekable, off, length int64, ft *FrameTable) []byte {
	t.Helper()

	r, _, err := s.OpenRangeReader(t.Context(), off, length, ft)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	_, err = r.Close(t.Context())
	require.NoError(t, err)

	return data
}

func TestMooncakeStoreFileUncompressed(t *testing.T) {
	t.Parallel()

	sizes := []int{1, mooncakeChunkSize - 1, mooncakeChunkSize, mooncakeChunkSize + 1, 2*mooncakeChunkSize + 12345}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()

			p, kv := newTestMooncake(t)
			data := testData(t, size)

			obj, err := p.OpenSeekable(t.Context(), "build/memfile")
			require.NoError(t, err)
			ft, _, err := obj.StoreFile(t.Context(), writeMooncakeTempFile(t, data))
			require.NoError(t, err)
			assert.Nil(t, ft)

			gotSize, err := obj.Size(t.Context())
			require.NoError(t, err)
			assert.Equal(t, int64(size), gotSize)

			// Same layout as the KASandbox backend.
			var meta mooncakeObjectMeta
			require.NoError(t, json.Unmarshal(kv.value(p.key("build/memfile")), &meta))
			assert.Equal(t, mooncakeObjectMeta{Size: int64(size), ChunkSize: mooncakeChunkSize}, meta)
			assert.Len(t, kv.keys(), 1+(size+mooncakeChunkSize-1)/mooncakeChunkSize)

			// Whole object, a range across a chunk boundary, a range past the end.
			assert.Equal(t, data, readRange(t, obj, 0, int64(size), nil))
			if size > mooncakeChunkSize {
				off := int64(mooncakeChunkSize - 100)
				end := min(off+300, int64(size))
				assert.Equal(t, data[off:end], readRange(t, obj, off, 300, nil))
			}
			assert.Equal(t, data[size-1:], readRange(t, obj, int64(size-1), 10, nil))
		})
	}
}

func TestMooncakeStoreFileCompressed(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"lz4", "zstd"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()

			p, kv := newTestMooncake(t)
			// More than one 5 MiB part, so parts are cut into chunks with a
			// carried-over tail.
			data := testData(t, 3*mooncakeChunkSize+777)
			cfg := CompressConfig{Enabled: true, Type: typ, Level: 1, MinPartSizeMB: 5}

			obj, err := p.OpenSeekable(t.Context(), "build/rootfs.ext4"+parseCompressionType(typ).Suffix())
			require.NoError(t, err)
			fft, _, err := obj.StoreFile(t.Context(), writeMooncakeTempFile(t, data), WithCompressConfig(cfg))
			require.NoError(t, err)
			ft := fft.Table()
			require.True(t, ft.IsCompressed())

			gotSize, err := obj.Size(t.Context())
			require.NoError(t, err)
			assert.Equal(t, int64(len(data)), gotSize, "Size reports the uncompressed size")

			var meta mooncakeObjectMeta
			require.NoError(t, json.Unmarshal(kv.value(p.key("build/rootfs.ext4"+parseCompressionType(typ).Suffix())), &meta))
			assert.Equal(t, ft.CompressedSize(), meta.Size)

			for i := range ft.NumFrames() {
				startU, endU, _, _ := ft.FrameAt(i)
				assert.Equal(t, data[startU:endU], readRange(t, obj, startU, endU-startU, ft), "frame %d", i)
			}
		})
	}
}

func TestMooncakeCompressedEmptyFile(t *testing.T) {
	t.Parallel()

	p, _ := newTestMooncake(t)
	obj, err := p.OpenSeekable(t.Context(), "build/memfile.lz4")
	require.NoError(t, err)

	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, nil), WithCompressConfig(CompressConfig{Enabled: true, Type: "lz4"}))
	require.NoError(t, err)

	size, err := obj.Size(t.Context())
	require.NoError(t, err)
	assert.Zero(t, size)
}

func TestMooncakeOverwrite(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)

	// A pause upload retry stores the same object again, here smaller.
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, testData(t, 3*mooncakeChunkSize)))
	require.NoError(t, err)
	second := testData(t, mooncakeChunkSize+10)
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, second))
	require.NoError(t, err)

	assert.Equal(t, second, readRange(t, obj, 0, int64(len(second)), nil))
	assert.Len(t, kv.keys(), 3, "chunks of the first upload are gone")

	// metadata.json is written again after the optimize phase.
	blob, err := p.OpenBlob(t.Context(), "build/metadata.json")
	require.NoError(t, err)
	require.NoError(t, blob.Put(t.Context(), []byte(`{"version":1}`)))
	require.NoError(t, blob.Put(t.Context(), []byte(`{"version":2}`)))
	got, err := GetBlob(t.Context(), blob)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":2}`, string(got))
}

// compressStream may run two parts at once after the input is read; the
// chunk offsets still follow the part order.
func TestMooncakePartUploaderOrdersParts(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	o := &mooncakeObject{kv: kv, key: p.key("build/memfile.lz4")}
	u := newMooncakePartUploader(o, ObjectMetadata{})
	require.NoError(t, u.Start(t.Context()))

	parts := [][]byte{testData(t, mooncakeChunkSize+3), testData(t, 2*mooncakeChunkSize-7), testData(t, 100)}
	var g errgroup.Group
	for i := len(parts) - 1; i >= 0; i-- { // later parts start first
		g.Go(func() error { return u.UploadPart(t.Context(), i+1, parts[i]) })
	}
	require.NoError(t, g.Wait())
	require.NoError(t, u.Complete(t.Context()))

	want := bytes.Join(parts, nil)
	assert.Equal(t, want, readRange(t, o, 0, int64(len(want)), nil))
}

func TestMooncakePartUploaderFailedPartStopsLaterParts(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	kv.failPut = func(key string) bool { return strings.HasSuffix(key, "#c#0") }
	o := &mooncakeObject{kv: kv, key: p.key("build/memfile.lz4")}
	u := newMooncakePartUploader(o, ObjectMetadata{})
	require.NoError(t, u.Start(t.Context()))

	second := make(chan error, 1)
	go func() { second <- u.UploadPart(t.Context(), 2, testData(t, mooncakeChunkSize)) }()
	require.Error(t, u.UploadPart(t.Context(), 1, testData(t, mooncakeChunkSize)))
	require.Error(t, <-second, "a later part gives up instead of waiting forever")

	require.NoError(t, u.Close())
	assert.Empty(t, kv.keys())
}

// The layer upload and the optimize phase write metadata.json at the same
// time; one of the writes wins, neither fails.
func TestMooncakeConcurrentWritesToOneKey(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	values := [][]byte{[]byte(`{"prefetch":false}`), []byte(`{"prefetch":true}`), testData(t, mooncakeChunkSize+9)}

	var g errgroup.Group
	for range 10 {
		for _, v := range values {
			g.Go(func() error {
				blob, err := p.OpenBlob(t.Context(), "build/metadata.json")
				if err != nil {
					return err
				}

				return blob.Put(t.Context(), v)
			})
		}
	}
	require.NoError(t, g.Wait())

	blob, err := p.OpenBlob(t.Context(), "build/metadata.json")
	require.NoError(t, err)
	got, err := GetBlob(t.Context(), blob)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(values, func(v []byte) bool { return bytes.Equal(v, got) }))
	if len(got) <= mooncakeChunkSize {
		assert.Len(t, kv.keys(), 1, "no chunks left from the large value")
	}
}

func TestMooncakeReplacesOrphanChunk(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	// Left behind by an upload that died before writing the metadata.
	require.NoError(t, kv.Put(p.key("build/memfile")+"#c#0", []byte("stale")))

	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	data := testData(t, 1000)
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, data))
	require.NoError(t, err)

	assert.Equal(t, data, readRange(t, obj, 0, 1000, nil))
}

func TestMooncakeFailedUploadLeavesNothing(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	// The second chunk: compressed or not, there is more than one.
	failing := p.key("build/memfile") + "#c#" + strconv.Itoa(mooncakeChunkSize)
	kv.failPut = func(key string) bool { return key == failing }

	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	// The put error comes back, not the cancellation of the other chunks:
	// 2026.30 does not retry a canceled upload.
	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, testData(t, 4*mooncakeChunkSize)))
	require.ErrorContains(t, err, "injected put failure")
	assert.Empty(t, kv.keys())

	_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, testData(t, 4*mooncakeChunkSize)),
		WithCompressConfig(CompressConfig{Enabled: true, Type: "lz4", MinPartSizeMB: 5}))
	require.ErrorContains(t, err, "injected put failure")
	assert.Empty(t, kv.keys())
}

func TestMooncakeReadsKASandboxObjects(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	data := testData(t, mooncakeChunkSize+5)
	key := p.key("build/memfile")
	require.NoError(t, kv.Put(key, []byte(`{"size":4194309,"chunk_size":4194304}`)))
	require.NoError(t, kv.Put(key+"#c#0", data[:mooncakeChunkSize]))
	require.NoError(t, kv.Put(key+"#c#4194304", data[mooncakeChunkSize:]))

	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	size, err := obj.Size(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	assert.Equal(t, data, readRange(t, obj, 0, size, nil))
}

func TestMooncakeShortChunkFails(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	key := p.key("build/memfile")
	require.NoError(t, kv.Put(key, []byte(`{"size":100,"chunk_size":4194304}`)))
	require.NoError(t, kv.Put(key+"#c#0", make([]byte, 60)))

	obj, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	r, _, err := obj.OpenRangeReader(t.Context(), 0, 100, nil)
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestMooncakeBlob(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)

	for name, data := range map[string][]byte{
		"small":  []byte("header bytes"),
		"empty":  {},
		"large":  testData(t, mooncakeChunkSize+1),
		"stream": testData(t, 2*mooncakeChunkSize),
	} {
		blob, err := p.OpenBlob(t.Context(), "build/"+name)
		require.NoError(t, err)

		if name == "stream" {
			require.NoError(t, blob.(StreamStorer).StoreReader(t.Context(), bytes.NewReader(data)))
		} else {
			require.NoError(t, blob.Put(t.Context(), data))
		}

		exists, err := blob.Exists(t.Context())
		require.NoError(t, err)
		assert.True(t, exists, name)

		got, err := GetBlob(t.Context(), blob)
		require.NoError(t, err)
		assert.Equal(t, len(data), len(got), name)
		assert.True(t, bytes.Equal(data, got), name)
	}

	// A small blob is a single value; Put after a large one removes its chunks.
	blob, err := p.OpenBlob(t.Context(), "build/large")
	require.NoError(t, err)
	require.NoError(t, blob.Put(t.Context(), []byte("now small")))
	for _, k := range kv.keys() {
		assert.NotContains(t, k, "build/large#c#")
	}
}

func TestMooncakeMissingObject(t *testing.T) {
	t.Parallel()

	p, _ := newTestMooncake(t)

	obj, err := p.OpenSeekable(t.Context(), "build/missing")
	require.NoError(t, err)
	_, err = obj.Size(t.Context())
	require.ErrorIs(t, err, ErrObjectNotExist)
	_, _, err = obj.OpenRangeReader(t.Context(), 0, 10, nil)
	require.ErrorIs(t, err, ErrObjectNotExist)

	blob, err := p.OpenBlob(t.Context(), "build/missing")
	require.NoError(t, err)
	exists, err := blob.Exists(t.Context())
	require.NoError(t, err)
	assert.False(t, exists)
	_, err = GetBlob(t.Context(), blob)
	require.ErrorIs(t, err, ErrObjectNotExist)
}

func TestMooncakeDeleteObjectsWithPrefix(t *testing.T) {
	t.Parallel()

	p, kv := newTestMooncake(t)
	for _, path := range []string{"build-a/memfile", "build-a/rootfs.ext4", "build-ab/memfile"} {
		obj, err := p.OpenSeekable(t.Context(), path)
		require.NoError(t, err)
		_, _, err = obj.StoreFile(t.Context(), writeMooncakeTempFile(t, testData(t, 100)))
		require.NoError(t, err)
	}

	require.NoError(t, p.DeleteObjectsWithPrefix(t.Context(), "build-a/"))
	assert.Equal(t, []string{p.key("build-ab/memfile"), p.key("build-ab/memfile") + "#c#0"}, kv.keys())

	// The cached metadata of a deleted object is dropped too.
	obj, err := p.OpenSeekable(t.Context(), "build-a/memfile")
	require.NoError(t, err)
	_, err = obj.Size(t.Context())
	require.ErrorIs(t, err, ErrObjectNotExist)
}

func TestMooncakeUploadSignedURL(t *testing.T) {
	t.Parallel()

	p, _ := newTestMooncake(t)
	_, err := p.UploadSignedURL(t.Context(), "files/layer.tar", time.Minute)
	require.Error(t, err, "no upload endpoint configured")

	key := []byte("0123456789abcdef0123456789abcdef")
	p = newMooncakeStorageWithKV(newFakeMooncakeKV(), nil, "bucket", "http://10.0.0.1:5008", key)
	signed, err := p.UploadSignedURL(t.Context(), "files/layer.tar", time.Minute)
	require.NoError(t, err)

	u, err := url.Parse(signed)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:5008", u.Host)
	assert.Equal(t, "/upload", u.Path)
	expires, err := strconv.ParseInt(u.Query().Get("expires"), 10, 64)
	require.NoError(t, err)
	assert.True(t, ValidateUploadToken(key, u.Query().Get("path"), expires, u.Query().Get("token")))
}

// StoreFile takes an upload slot like the other providers: with every slot
// held it waits, and goes on once one is released.
func TestMooncakeStoreFileWaitsForUploadSlot(t *testing.T) {
	t.Parallel()

	ff, err := featureflags.NewClient()
	require.NoError(t, err)
	limiter, err := limit.New(t.Context(), ff)
	require.NoError(t, err)
	t.Cleanup(func() { _ = limiter.Close(context.Background()) })

	p, _ := newTestMooncake(t)
	p.limiter = limiter

	var releases []func()
	for range featureflags.StorageConcurrentUploadLimit.Fallback() {
		release, err := limiter.AcquireUploadSlot(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}

	path := filepath.Join(t.TempDir(), "memfile")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	seekable, err := p.OpenSeekable(t.Context(), "build/memfile")
	require.NoError(t, err)
	obj := seekable.(*mooncakeObject)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err = obj.StoreFile(ctx, path)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	exists, err := obj.Exists(t.Context())
	require.NoError(t, err)
	require.False(t, exists, "StoreFile wrote without an upload slot")

	releases[0]()
	_, _, err = obj.StoreFile(t.Context(), path)
	require.NoError(t, err)
	for _, release := range releases[1:] {
		release()
	}
}
