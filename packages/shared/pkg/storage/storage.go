package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	tracer = otel.Tracer("github.com/e2b-dev/infra/packages/shared/pkg/storage")
	meter  = otel.GetMeterProvider().Meter("shared.pkg.storage")
)

var ErrObjectNotExist = errors.New("object does not exist")

type Provider string

const (
	GCPStorageProvider     Provider = "GCPBucket"
	AWSStorageProvider     Provider = "AWSBucket"
	LocalStorageProvider   Provider = "Local"
	MINIOStorageProvider   Provider = "MinioBucket"
	MooncakeStorageProvider   Provider = "MooncakeBucket"
	DefaultStorageProvider Provider = MINIOStorageProvider

	storageProviderEnv = "STORAGE_PROVIDER"

	// MemoryChunkSize must always be bigger or equal to the block size.
	MemoryChunkSize = 4 * 1024 * 1024 // 4 MB
)

type SeekableObjectType int

const (
	UnknownSeekableObjectType SeekableObjectType = iota
	MemfileObjectType
	RootFSObjectType
)

type ObjectType int

const (
	UnknownObjectType ObjectType = iota
	MemfileHeaderObjectType
	RootFSHeaderObjectType
	SnapfileObjectType
	MetadataObjectType
	BuildLayerFileObjectType
	LayerMetadataObjectType
)

type StorageProvider interface {
	DeleteObjectsWithPrefix(ctx context.Context, prefix string) error
	UploadSignedURL(ctx context.Context, path string, ttl time.Duration) (string, error)
	OpenBlob(ctx context.Context, path string, objectType ObjectType) (Blob, error)
	OpenSeekable(ctx context.Context, path string, seekableObjectType SeekableObjectType) (Seekable, error)
	GetDetails() string
}

type Blob interface {
	WriteTo(ctx context.Context, dst io.Writer) (int64, error)
	Put(ctx context.Context, data []byte) error
	Exists(ctx context.Context) (bool, error)
}

type SeekableReader interface {
	// Random slice access, off and buffer length must be aligned to block size
	ReadAt(ctx context.Context, buffer []byte, off int64) (int, error)
	Size(ctx context.Context) (int64, error)
}

// StreamingReader supports progressive reads via a streaming range reader.
type StreamingReader interface {
	OpenRangeReader(ctx context.Context, off, length int64) (io.ReadCloser, error)
}

type SeekableWriter interface {
	// Store entire file
	StoreFile(ctx context.Context, path string) error
}

// BufferRegistrar allows Seekable readers to register memory buffers with
// the underlying storage backend. This enables optimizations like zero-copy
// reads from storage directly into application memory (e.g., Mooncake's
// direct memory access). Backends that do not support buffer registration
// can simply omit this interface — callers will gracefully skip the
// optimization.
// type BufferRegistrar interface {
// 	RegisterBuffer(ptr uintptr, size uint64) error
// 	UnregisterBuffer(ptr uintptr) error
// }

// BatchReader allows Seekable readers to read multiple non-contiguous chunks
// of data in a single backend call (e.g., Mooncake's BatchGetInto). This
// avoids the overhead of launching many goroutines and making many individual
// network round-trips. Backends without batch-read support can simply omit
// this interface — callers will gracefully fall back to a parallel
// per-chunk ReadAt loop.
// type BatchReader interface {
// 	// BatchReadInto reads multiple chunks of data in a single backend call.
// 	// Each chunk is written directly into the memory region specified by
// 	// the corresponding ptr/size pair.
// 	//
// 	// offsets: the absolute object offsets (in bytes) of each chunk to read
// 	// sizes:   the size (in bytes) of each chunk to read
// 	// ptrs:    the memory pointers where each chunk should be written
// 	// Returns a slice of bytes-read counts and any error encountered.
// 	BatchReadInto(ctx context.Context, offsets []int64, sizes []int64, ptrs []uintptr) ([]int, error)
// }

type Seekable interface {
	SeekableReader
	SeekableWriter
	StreamingReader
}

func GetTemplateStorageProvider(ctx context.Context, limiter *limit.Limiter) (StorageProvider, error) {
	provider := Provider(env.GetEnv(storageProviderEnv, string(DefaultStorageProvider)))

	if provider == LocalStorageProvider {
		basePath := env.GetEnv("LOCAL_TEMPLATE_STORAGE_BASE_PATH", "/tmp/templates")

		return newFileSystemStorage(basePath), nil
	}

	bucketName := utils.RequiredEnv("TEMPLATE_BUCKET_NAME", "Bucket for storing template files")

	// cloud bucket-based storage
	switch provider {
	case AWSStorageProvider:
		return newAWSStorage(ctx, bucketName)
	case GCPStorageProvider:
		return NewGCP(ctx, bucketName, limiter)
	case MINIOStorageProvider:
		return NewMinioBucketStorageProvider(ctx, bucketName)
	case MooncakeStorageProvider:
		//dsnPath := env.GetEnv("POSTGRESQL_DSN_PATH", "")
		//return NewMooncakeStorageProvider(ctx, bucketName, dsnPath)
		return NewMooncakeStorageProvider(ctx, bucketName)
	}

	return nil, fmt.Errorf("unknown storage provider: %s", provider)
}

func GetBuildCacheStorageProvider(ctx context.Context, limiter *limit.Limiter) (StorageProvider, error) {
	provider := Provider(env.GetEnv(storageProviderEnv, string(DefaultStorageProvider)))

	if provider == LocalStorageProvider {
		basePath := env.GetEnv("LOCAL_BUILD_CACHE_STORAGE_BASE_PATH", "/tmp/build-cache")

		return newFileSystemStorage(basePath), nil
	}

	bucketName := utils.RequiredEnv("BUILD_CACHE_BUCKET_NAME", "Bucket for storing template files")

	// cloud bucket-based storage
	switch provider {
	case AWSStorageProvider:
		return newAWSStorage(ctx, bucketName)
	case GCPStorageProvider:
		return NewGCP(ctx, bucketName, limiter)
	case MINIOStorageProvider:
		return NewMinioBucketStorageProvider(ctx, bucketName)
	case MooncakeStorageProvider:
		//dsnPath := env.GetEnv("POSTGRESQL_DSN_PATH", "")
		//return NewMooncakeStorageProvider(ctx, bucketName, dsnPath)
		return NewMooncakeStorageProvider(ctx, bucketName)
	}

	return nil, fmt.Errorf("unknown storage provider: %s", provider)
}

func recordError(span trace.Span, err error) {
	if ignoreEOF(err) == nil {
		return
	}

	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// GetBlob is a convenience wrapper that wraps b.WriteTo interface to return a
// byte slice.
func GetBlob(ctx context.Context, b Blob) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := b.WriteTo(ctx, &buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
