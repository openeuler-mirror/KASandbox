package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Writes to the Mooncake backend (layout in storage_mooncake.go) and the
// provider itself.

type mooncakeStorage struct {
	kv        mooncakeKV
	chunks    *mooncakeChunkCache
	bucket    string
	uploadURL string
	hmacKey   []byte
	// limiter caps how many files are uploaded at once, like the other
	// providers do (gcloud-concurrent-upload-limit); nil means no cap.
	limiter *limit.Limiter
}

var _ StorageProvider = (*mooncakeStorage)(nil)

var (
	_ Seekable     = (*mooncakeObject)(nil)
	_ Blob         = (*mooncakeObject)(nil)
	_ StreamStorer = (*mooncakeObject)(nil)
)

func newMooncakeStorage(bucket, uploadBaseURL string, hmacKey []byte, limiter *limit.Limiter) (*mooncakeStorage, error) {
	kv, err := newMooncakeClient()
	if err != nil {
		return nil, err
	}

	s := newMooncakeStorageWithKV(kv, sharedMooncakeChunkCache(), bucket, uploadBaseURL, hmacKey)
	s.limiter = limiter

	return s, nil
}

func newMooncakeStorageWithKV(kv mooncakeKV, chunks *mooncakeChunkCache, bucket, uploadBaseURL string, hmacKey []byte) *mooncakeStorage {
	return &mooncakeStorage{kv: kv, chunks: chunks, bucket: bucket, uploadURL: uploadBaseURL, hmacKey: hmacKey}
}

func (s *mooncakeStorage) key(path string) string {
	return s.bucket + "/" + path
}

func (s *mooncakeStorage) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	prefixKey := s.key(prefix)
	// Mooncake has no listing; the regex is matched against every key.
	n, err := s.kv.RemoveByRegex("^" + regexp.QuoteMeta(prefixKey) + ".*")
	invalidateMooncakeMetaPrefix(prefixKey)
	s.chunks.invalidatePrefix(prefixKey)
	if err != nil {
		return fmt.Errorf("failed to delete Mooncake objects with prefix %s: %w", prefix, err)
	}

	if n == 0 {
		logger.L().Warn(ctx, "No objects found to delete with the given prefix", zap.String("prefix", prefix), zap.String("bucket", s.bucket))
	}

	return nil
}

func (s *mooncakeStorage) UploadSignedURL(_ context.Context, path string, ttl time.Duration) (string, error) {
	if s.uploadURL == "" || s.hmacKey == nil {
		return "", errors.New("mooncake storage does not support signed URLs (no upload endpoint configured)")
	}

	expiresSec := time.Now().Add(ttl).Unix()
	token := ComputeUploadHMAC(s.hmacKey, path, expiresSec)

	return fmt.Sprintf("%s/upload?path=%s&expires=%d&token=%s",
		s.uploadURL, url.QueryEscape(path), expiresSec, url.QueryEscape(token)), nil
}

func (s *mooncakeStorage) OpenSeekable(_ context.Context, path string) (Seekable, error) {
	objType, _ := seekableObjectType(path)

	return &mooncakeObject{kv: s.kv, chunks: s.chunks, key: s.key(path), objType: objType, limiter: s.limiter}, nil
}

func (s *mooncakeStorage) OpenBlob(_ context.Context, path string) (Blob, error) {
	return &mooncakeObject{kv: s.kv, chunks: s.chunks, key: s.key(path), limiter: s.limiter}, nil
}

func (s *mooncakeStorage) GetDetails() string {
	return fmt.Sprintf("[Mooncake Storage, bucket set to %s]", s.bucket)
}

func (o *mooncakeObject) writeMeta(meta *mooncakeObjectMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	if err := o.kv.Put(o.key, data); err != nil {
		return fmt.Errorf("failed to write metadata %s: %w", o.key, err)
	}
	mooncakeMetaCache.Store(o.key, *meta)

	return nil
}

// removeExisting deletes the value under the object's key and, for a chunked
// object, its chunks. Mooncake cannot overwrite a key, but 2026.30 rewrites
// objects: metadata.json after the optimize phase, and every object when a
// pause upload is retried. The metadata goes first so readers never see an
// object whose chunks are missing; a crash in between leaves orphan chunks,
// which putChunk replaces.
func (o *mooncakeObject) removeExisting(ctx context.Context) error {
	mooncakeMetaCache.Delete(o.key)

	size, err := o.kv.GetSize(o.key)
	if errors.Is(err, ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check existing object %s: %w", o.key, err)
	}

	var meta *mooncakeObjectMeta
	if size <= mooncakeMaxMetaSize {
		if meta, err = o.readMeta(ctx); err != nil && !errors.Is(err, errNotMooncakeMeta) {
			return fmt.Errorf("failed to read existing object %s: %w", o.key, err)
		}
	}

	if err := o.kv.Remove(o.key); err != nil {
		return fmt.Errorf("failed to remove existing object %s: %w", o.key, err)
	}

	if meta != nil {
		o.removeChunks(meta.Size, meta.ChunkSize)
	}

	return nil
}

// removeChunks deletes the chunks covering [0, size); failures only leave
// orphans behind.
func (o *mooncakeObject) removeChunks(size, chunkSize int64) {
	for off := int64(0); off < size; off += chunkSize {
		if err := o.kv.Remove(o.chunkKey(off)); err != nil {
			logger.L().Warn(context.Background(), "failed to remove Mooncake chunk", zap.String("key", o.chunkKey(off)), zap.Error(err))
		}
	}
	o.chunks.invalidatePrefix(o.key + mooncakeChunkSeparator)
}

// putChunk replaces a chunk left behind by an earlier failed upload.
func (o *mooncakeObject) putChunk(offset int64, data []byte) error {
	key := o.chunkKey(offset)
	defer o.chunks.invalidate(key)

	err := o.kv.Put(key, data)
	if err == nil {
		return nil
	}

	exists, existsErr := o.kv.Exists(key)
	if existsErr != nil || !exists {
		return fmt.Errorf("failed to put chunk %s: %w", key, err)
	}
	if err := o.kv.Remove(key); err != nil {
		return fmt.Errorf("failed to replace chunk %s: %w", key, err)
	}
	if err := o.kv.Put(key, data); err != nil {
		return fmt.Errorf("failed to put chunk %s: %w", key, err)
	}

	return nil
}

// storeChunks reads r to the end and stores it as chunks, uploaded
// concurrently. It returns the number of bytes stored; on error the chunks
// already written are removed.
func (o *mooncakeObject) storeChunks(ctx context.Context, r io.Reader) (size int64, err error) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(mooncakeUploadConcurrency)

	defer func() {
		if err != nil {
			_ = g.Wait()
			o.removeChunks(size, mooncakeChunkSize)
		}
	}()

	for {
		// A failed chunk cancels gctx; g.Wait below returns its error, not
		// the cancellation (2026.30 does not retry a canceled upload).
		if gctx.Err() != nil {
			break
		}

		bufPtr := mooncakeChunkPool.Get().(*[]byte)
		n, readErr := io.ReadFull(r, *bufPtr)
		eof := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if readErr != nil && !eof {
			mooncakeChunkPool.Put(bufPtr)

			return size, fmt.Errorf("failed to read upload data: %w", readErr)
		}
		if n == 0 {
			mooncakeChunkPool.Put(bufPtr)

			break
		}

		off := size
		size += int64(n)
		g.Go(func() error {
			defer mooncakeChunkPool.Put(bufPtr)
			if err := gctx.Err(); err != nil {
				return err
			}

			return o.putChunk(off, (*bufPtr)[:n])
		})

		if eof {
			break
		}
	}

	if err := g.Wait(); err != nil {
		return size, err
	}

	return size, ctx.Err()
}

// storeReader stores r as a chunked object, metadata last. The caller holds
// the key lock.
func (o *mooncakeObject) storeReader(ctx context.Context, r io.Reader) (int64, error) {
	if err := o.removeExisting(ctx); err != nil {
		return 0, err
	}

	size, err := o.storeChunks(ctx, r)
	if err != nil {
		return size, err
	}

	if err := o.writeMeta(&mooncakeObjectMeta{Size: size, ChunkSize: mooncakeChunkSize}); err != nil {
		o.removeChunks(size, mooncakeChunkSize)

		return size, err
	}

	return size, nil
}

func (o *mooncakeObject) Put(ctx context.Context, data []byte, _ ...PutOption) error {
	unlock := lockMooncakeKey(o.key)
	defer unlock()

	// Empty data goes through the chunked layout: Mooncake cannot store an
	// empty value.
	if len(data) == 0 || len(data) > mooncakeChunkSize {
		_, err := o.storeReader(ctx, bytes.NewReader(data))

		return err
	}

	if err := o.removeExisting(ctx); err != nil {
		return err
	}

	return o.kv.Put(o.key, data)
}

// StoreReader stores an object of unknown size without buffering it whole;
// the signed upload handler uses it for layer files in the build cache.
func (o *mooncakeObject) StoreReader(ctx context.Context, r io.Reader, _ ...PutOption) error {
	unlock := lockMooncakeKey(o.key)
	defer unlock()

	_, err := o.storeReader(ctx, r)

	return err
}

func (o *mooncakeObject) StoreFile(ctx context.Context, path string, opts ...PutOption) (*FullFrameTable, [32]byte, error) {
	p := ApplyPutOptions(opts)

	// Take the upload slot before the key lock, so a queued upload does not
	// hold the lock. Without the cap every paused sandbox compresses and
	// uploads at once, each holding its compressed parts in memory.
	release, err := o.limiter.AcquireUploadSlot(ctx)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer release()

	unlock := lockMooncakeKey(o.key)
	defer unlock()

	if cfg := CompressConfigFromOpts(p); cfg.IsCompressionEnabled() {
		if err := o.removeExisting(ctx); err != nil {
			return nil, [32]byte{}, err
		}

		// Parts are cut into chunks in order (see mooncakePartUploader), so
		// more upload slots would only hold more compressed parts in memory.
		const uploadSlots = 1
		ft, checksum, err := storeFileCompressed(ctx, path, cfg, uploadSlots, p, func(metadata ObjectMetadata) (partUploader, error) {
			return newMooncakePartUploader(o, metadata), nil
		})
		if err == nil {
			t := ft.Table()
			logger.L().Debug(ctx, "Stored file to Mooncake",
				zap.String("object", o.key),
				zap.String("source", path),
				zap.Int64("size_uncompressed", t.UncompressedSize()),
				zap.Int64("size_compressed", t.CompressedSize()),
				zap.String("compression", cfg.CompressionType().String()),
				zap.Int("frames", t.NumFrames()),
			)
		}

		return ft, checksum, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer f.Close()

	size, err := o.storeReader(ctx, f)
	if err == nil {
		logger.L().Debug(ctx, "Stored file to Mooncake",
			zap.String("object", o.key),
			zap.String("source", path),
			zap.Int64("size_uncompressed", size),
			zap.String("compression", "none"),
		)
	}

	return nil, [32]byte{}, err
}

// mooncakePartUploader cuts the compressed stream into chunks at stored
// offsets, so parts are processed in order: a part waits until the ones before
// it are done. compressStream starts them in order but may run two at once
// once the input is read. The chunks of a part are uploaded concurrently.
type mooncakePartUploader struct {
	o                *mooncakeObject
	uncompressedSize *int64

	mu       sync.Mutex
	nextPart int
	turn     chan struct{} // closed and replaced when nextPart moves on
	err      error         // first failed part; later parts give up

	// Only the part whose turn it is touches these.
	pending   []byte // tail shorter than a chunk
	stored    int64  // bytes already in chunks
	completed bool
}

var _ partUploader = (*mooncakePartUploader)(nil)

func newMooncakePartUploader(o *mooncakeObject, metadata ObjectMetadata) *mooncakePartUploader {
	u := &mooncakePartUploader{o: o, nextPart: 1, turn: make(chan struct{})}
	if size, ok := metadata.UncompressedSize(); ok {
		u.uncompressedSize = &size
	}

	return u
}

func (u *mooncakePartUploader) Start(context.Context) error { return nil }

func (u *mooncakePartUploader) waitTurn(ctx context.Context, partIndex int) error {
	for {
		u.mu.Lock()
		switch {
		case u.err != nil:
			err := u.err
			u.mu.Unlock()

			return fmt.Errorf("mooncake upload of %s: an earlier part failed: %w", u.o.key, err)
		case partIndex == u.nextPart:
			u.mu.Unlock()

			return nil
		case partIndex < u.nextPart:
			u.mu.Unlock()

			return fmt.Errorf("mooncake upload of %s: part %d uploaded twice", u.o.key, partIndex)
		}
		turn := u.turn
		u.mu.Unlock()

		select {
		case <-turn:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (u *mooncakePartUploader) finishTurn(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if err != nil && u.err == nil {
		u.err = err
	}
	u.nextPart++
	close(u.turn)
	u.turn = make(chan struct{})
}

func (u *mooncakePartUploader) UploadPart(ctx context.Context, partIndex int, data ...[]byte) (err error) {
	if err := u.waitTurn(ctx, partIndex); err != nil {
		return err
	}
	defer func() { u.finishTurn(err) }()

	for _, d := range data {
		u.pending = append(u.pending, d...)
	}

	full := int64(len(u.pending)) / mooncakeChunkSize * mooncakeChunkSize
	if full == 0 {
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(mooncakeUploadConcurrency)
	for i := int64(0); i < full; i += mooncakeChunkSize {
		chunk := u.pending[i : i+mooncakeChunkSize]
		off := u.stored + i
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}

			return u.o.putChunk(off, chunk)
		})
	}
	err = g.Wait()
	u.stored += full // on failure too, so Close removes what this part did write
	u.pending = append([]byte(nil), u.pending[full:]...)

	return err
}

func (u *mooncakePartUploader) Complete(context.Context) error {
	if len(u.pending) > 0 {
		if err := u.o.putChunk(u.stored, u.pending); err != nil {
			return err
		}
		u.stored += int64(len(u.pending))
		u.pending = nil
	}

	meta := &mooncakeObjectMeta{Size: u.stored, ChunkSize: mooncakeChunkSize, UncompressedSize: u.uncompressedSize}
	if err := u.o.writeMeta(meta); err != nil {
		return err
	}

	u.completed = true

	return nil
}

// Close removes a partial upload.
func (u *mooncakePartUploader) Close() error {
	if u.completed {
		return nil
	}

	u.o.removeChunks(u.stored+int64(len(u.pending)), mooncakeChunkSize)

	return nil
}
