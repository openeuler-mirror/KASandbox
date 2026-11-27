package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
)

// Mooncake is a flat key-value store whose values are limited to the 4 MiB
// registered read buffer and cannot be overwritten. Objects use the layout of
// the KASandbox Mooncake backend, so data written by it stays readable:
//
//	<bucket>/<path>           JSON metadata (mooncakeObjectMeta), written last
//	<bucket>/<path>#c#<off>   stored bytes [off, off+4 MiB)
//
// Small blobs (at most 4 MiB) may instead be a single value under
// <bucket>/<path>. Compressed objects store the compressed stream, chunked the
// same way; offsets are stored (compressed) offsets and the frame table maps
// them, as for the other backends.
const (
	mooncakeChunkSize         = MemoryChunkSize
	mooncakeMaxMetaSize       = 4096
	mooncakeUploadConcurrency = 16
	mooncakeChunkSeparator    = "#c#"
)

var errLegacyMooncakeBlobTooLarge = errors.New("single-value Mooncake blob exceeds the 4 MiB read buffer")

// mooncakeKV is the part of the Mooncake client the provider uses. The real
// client needs cgo and the mooncake build tag (storage_mooncake_client.go).
type mooncakeKV interface {
	// Put stores value under key; it fails if the key already exists.
	Put(key string, value []byte) error
	// GetInto copies the value of key into dst and returns its length. dst
	// must be at least as long as the value and at most 4 MiB.
	GetInto(ctx context.Context, key string, dst []byte) (int, error)
	// GetSize and GetInto return ErrObjectNotExist for a missing key.
	GetSize(key string) (int64, error)
	Exists(key string) (bool, error)
	// Remove succeeds if the key does not exist.
	Remove(key string) error
	RemoveByRegex(pattern string) (int64, error)
}

type mooncakeObjectMeta struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk_size"`
	// UncompressedSize is set for compressed objects only.
	UncompressedSize *int64 `json:"uncompressed_size,omitempty"`
}

func (m *mooncakeObjectMeta) valid() bool {
	return m.ChunkSize > 0 && m.ChunkSize <= mooncakeChunkSize && m.Size >= 0
}

// Metadata is cached per process: OpenRangeReader runs once per fetched chunk.
// Writes and deletes in this process invalidate it; another node rewriting
// the same key is not seen until restart.
var mooncakeMetaCache sync.Map

func invalidateMooncakeMetaPrefix(prefix string) {
	mooncakeMetaCache.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && strings.HasPrefix(k, prefix) {
			mooncakeMetaCache.Delete(key)
		}

		return true
	})
}

// Writes to one key are serialized within the process: 2026.30 writes some
// objects twice at once (the background layer upload and the optimize phase
// both write metadata.json), which S3 settles as last writer wins but which
// fails on Mooncake, where a write is remove-then-put. Readers in this process
// take the same lock, so they never see the key between the two steps.
type mooncakeKeyLock struct {
	mu   sync.Mutex
	refs int
}

var (
	mooncakeKeyLocksMu sync.Mutex
	mooncakeKeyLocks   = map[string]*mooncakeKeyLock{}
)

func lockMooncakeKey(key string) func() {
	mooncakeKeyLocksMu.Lock()
	l := mooncakeKeyLocks[key]
	if l == nil {
		l = &mooncakeKeyLock{}
		mooncakeKeyLocks[key] = l
	}
	l.refs++
	mooncakeKeyLocksMu.Unlock()

	l.mu.Lock()

	return func() {
		l.mu.Unlock()

		mooncakeKeyLocksMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(mooncakeKeyLocks, key)
		}
		mooncakeKeyLocksMu.Unlock()
	}
}

var mooncakeChunkPool = sync.Pool{
	New: func() any {
		buf := make([]byte, mooncakeChunkSize)

		return &buf
	},
}

type mooncakeObject struct {
	kv      mooncakeKV
	chunks  *mooncakeChunkCache // nil: no chunk cache
	key     string
	objType SeekableObjectType
	limiter *limit.Limiter // nil: no upload cap
}

func (o *mooncakeObject) chunkKey(offset int64) string {
	return o.key + mooncakeChunkSeparator + strconv.FormatInt(offset, 10)
}

// readMeta reads the metadata value under the object's key. It returns
// errNotMooncakeMeta if the key holds something else (a single-value blob).
func (o *mooncakeObject) readMeta(ctx context.Context) (*mooncakeObjectMeta, error) {
	buf := make([]byte, mooncakeMaxMetaSize)
	n, err := o.kv.GetInto(ctx, o.key, buf)
	if err != nil {
		return nil, err
	}

	return parseMooncakeMeta(buf[:n])
}

var errNotMooncakeMeta = errors.New("not a chunked Mooncake object")

func parseMooncakeMeta(data []byte) (*mooncakeObjectMeta, error) {
	var meta mooncakeObjectMeta
	if err := json.Unmarshal(data, &meta); err != nil || !meta.valid() {
		return nil, errNotMooncakeMeta
	}

	return &meta, nil
}

func (o *mooncakeObject) meta(ctx context.Context) (*mooncakeObjectMeta, error) {
	if cached, ok := mooncakeMetaCache.Load(o.key); ok {
		meta := cached.(mooncakeObjectMeta)

		return &meta, nil
	}

	unlock := lockMooncakeKey(o.key)
	defer unlock()

	size, err := o.kv.GetSize(o.key)
	if err != nil {
		return nil, err
	}
	if size > mooncakeMaxMetaSize {
		return nil, fmt.Errorf("mooncake object %s: %w", o.key, errNotMooncakeMeta)
	}

	meta, err := o.readMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("mooncake object %s: %w", o.key, err)
	}
	mooncakeMetaCache.Store(o.key, *meta)

	return meta, nil
}

func (o *mooncakeObject) WriteTo(ctx context.Context, dst io.Writer) (n int64, err error) {
	start := time.Now()
	defer func() { RecordReadBlob(ctx, time.Since(start), n, o.key, SourceMooncake, err) }()

	unlock := lockMooncakeKey(o.key)
	defer unlock()

	size, err := o.kv.GetSize(o.key)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		return 0, nil
	}
	if size > mooncakeChunkSize {
		return 0, fmt.Errorf("%w: %s is %d bytes", errLegacyMooncakeBlobTooLarge, o.key, size)
	}

	bufPtr := mooncakeChunkPool.Get().(*[]byte)
	defer mooncakeChunkPool.Put(bufPtr)

	read, err := o.kv.GetInto(ctx, o.key, (*bufPtr)[:size])
	if err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", o.key, err)
	}
	value := (*bufPtr)[:read]

	if size <= mooncakeMaxMetaSize {
		if meta, metaErr := parseMooncakeMeta(value); metaErr == nil {
			rc := &mooncakeChunkReader{ctx: ctx, o: o, meta: meta, start: 0, off: 0, end: meta.Size}
			defer rc.Close()

			return io.Copy(dst, rc)
		}
	}

	written, err := dst.Write(value)

	return int64(written), err
}

func (o *mooncakeObject) Exists(_ context.Context) (bool, error) {
	return o.kv.Exists(o.key)
}

func (o *mooncakeObject) Size(ctx context.Context) (_ int64, err error) {
	start := time.Now()
	defer func() { RecordReadSize(ctx, time.Since(start), o.objType, SourceMooncake, err) }()

	meta, err := o.meta(ctx)
	if err != nil {
		return 0, err
	}
	if meta.UncompressedSize != nil {
		return *meta.UncompressedSize, nil
	}

	return meta.Size, nil
}

func (o *mooncakeObject) OpenRangeReader(ctx context.Context, off, length int64, frameTable *FrameTable) (_ RangeReader, _ Source, err error) {
	start := time.Now()
	defer func() {
		RecordReadOpen(ctx, time.Since(start), o.objType, SourceMooncake, frameTable.CompressionType(), err)
	}()

	if !frameTable.IsCompressed() {
		raw, err := o.openRangeReader(ctx, off, length)
		if err != nil {
			return nil, SourceMooncake, err
		}

		return raw, SourceMooncake, nil
	}

	r, err := frameTable.LocateCompressed(off)
	if err != nil {
		return nil, SourceMooncake, fmt.Errorf("get frame for offset %d, Mooncake:%s: %w", off, o.key, err)
	}

	raw, err := o.openRangeReader(ctx, r.Offset, int64(r.Length))
	if err != nil {
		return nil, SourceMooncake, err
	}

	dec, err := NewDecompressReader(raw, frameTable.CompressionType(), SourceMooncake, o.objType)
	if err != nil {
		raw.Close(ctx)

		return nil, SourceMooncake, err
	}

	return dec, SourceMooncake, nil
}

// openRangeReader reads stored bytes [off, off+length), clipped to the
// object, across as many chunks as needed.
func (o *mooncakeObject) openRangeReader(ctx context.Context, off, length int64) (RangeReader, error) {
	if off < 0 || length < 0 {
		return nil, fmt.Errorf("invalid range %d+%d for %s", off, length, o.key)
	}

	meta, err := o.meta(ctx)
	if err != nil {
		return nil, err
	}
	if off > meta.Size {
		return nil, fmt.Errorf("range start %d beyond the end of %s (%d bytes)", off, o.key, meta.Size)
	}

	return NewRangeReader(&mooncakeChunkReader{
		ctx:   ctx,
		o:     o,
		meta:  meta,
		start: off,
		off:   off,
		end:   min(off+length, meta.Size),
	}), nil
}

// mooncakeChunkReader streams [off, end) of a chunked object.
type mooncakeChunkReader struct {
	ctx   context.Context //nolint:containedctx // io.Reader has no context argument
	o     *mooncakeObject
	meta  *mooncakeObjectMeta
	start int64
	off   int64
	end   int64

	// The chunk at bufOff, kept for reads that do not start on a chunk
	// boundary or are shorter than the chunk. With the chunk cache buf is the
	// cached value and bufPtr stays nil.
	bufPtr *[]byte
	buf    []byte
	bufOff int64
}

// chunkLen is the stored length of the chunk at chunkOff.
func (r *mooncakeChunkReader) chunkLen(chunkOff int64) int64 {
	return min(r.meta.ChunkSize, r.meta.Size-chunkOff)
}

func (r *mooncakeChunkReader) readChunk(chunkOff int64, dst []byte) error {
	want := r.chunkLen(chunkOff)
	n, err := r.o.kv.GetInto(r.ctx, r.o.chunkKey(chunkOff), dst[:want])

	return r.checkChunk(chunkOff, n, err)
}

func (r *mooncakeChunkReader) checkChunk(chunkOff int64, n int, err error) error {
	if err != nil {
		return fmt.Errorf("failed to read chunk %s: %w", r.o.chunkKey(chunkOff), err)
	}
	if want := r.chunkLen(chunkOff); int64(n) != want {
		return fmt.Errorf("chunk %s has %d bytes, want %d: %w", r.o.chunkKey(chunkOff), n, want, io.ErrUnexpectedEOF)
	}

	return nil
}

// readWholeChunk fills dst with the chunk at chunkOff, from a cached copy or a
// read already in flight if there is one, without caching it.
func (r *mooncakeChunkReader) readWholeChunk(chunkOff int64, dst []byte) error {
	if r.o.chunks != nil {
		data, ok, err := r.o.chunks.peek(r.ctx, r.o.chunkKey(chunkOff), r.chunkLen(chunkOff))
		if err != nil {
			return r.checkChunk(chunkOff, 0, err)
		}
		if ok {
			copy(dst, data)

			return nil
		}
	}

	return r.readChunk(chunkOff, dst)
}

// loadChunk makes the chunk at chunkOff the buffered one. Only a chunk the
// reader's range covers in part (a compressed frame) goes through the chunk
// cache, as the rest of it is likely read next; a fully covered chunk is read
// once into the block cache.
func (r *mooncakeChunkReader) loadChunk(chunkOff int64) error {
	if r.o.chunks != nil && (r.start > chunkOff || r.end < chunkOff+r.chunkLen(chunkOff)) {
		data, err := r.o.chunks.get(r.ctx, r.o.kv, r.o.chunkKey(chunkOff), r.chunkLen(chunkOff))
		if err := r.checkChunk(chunkOff, len(data), err); err != nil {
			return err
		}
		r.buf, r.bufOff = data, chunkOff

		return nil
	}

	if r.bufPtr == nil {
		r.bufPtr = mooncakeChunkPool.Get().(*[]byte)
	}
	if err := r.readWholeChunk(chunkOff, *r.bufPtr); err != nil {
		return err
	}
	r.buf, r.bufOff = (*r.bufPtr)[:r.chunkLen(chunkOff)], chunkOff

	return nil
}

func (r *mooncakeChunkReader) Read(p []byte) (int, error) {
	total := 0
	for total < len(p) && r.off < r.end {
		if err := r.ctx.Err(); err != nil {
			return total, err
		}

		chunkOff := r.off - r.off%r.meta.ChunkSize
		inChunk := r.off - chunkOff
		want := min(r.end, chunkOff+r.chunkLen(chunkOff)) - r.off

		// A read covering the whole chunk goes straight into p.
		if inChunk == 0 && want == r.chunkLen(chunkOff) && int64(len(p)-total) >= want {
			if err := r.readWholeChunk(chunkOff, p[total:total+int(want)]); err != nil {
				return total, err
			}
			total += int(want)
			r.off += want

			continue
		}

		if r.buf == nil || r.bufOff != chunkOff {
			if err := r.loadChunk(chunkOff); err != nil {
				r.buf = nil

				return total, err
			}
		}

		copied := copy(p[total:], r.buf[inChunk:inChunk+want])
		total += copied
		r.off += int64(copied)
	}

	if r.off >= r.end {
		return total, io.EOF
	}

	return total, nil
}

func (r *mooncakeChunkReader) Close() error {
	if r.bufPtr != nil {
		mooncakeChunkPool.Put(r.bufPtr)
		r.bufPtr = nil
		r.buf = nil
	}

	return nil
}
