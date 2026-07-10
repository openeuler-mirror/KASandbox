package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
)

const (
	minioOperationTimeout = 50 * time.Second
	minioWriteTimeout     = 300 * time.Second
	minioReadTimeout      = 150 * time.Second
	minioBufferSize       = 2 << 21
	useSSL                = false
)

var errObjectNotExist = errors.New("object does not exist")

type MinioBucketStorageProvider struct {
	client     *minio.Client
	bucketName string
}

var _ StorageProvider = (*MinioBucketStorageProvider)(nil)

type minioObject struct {
	client     *minio.Client
	path       string
	bucketName string
}

var (
	_ Seekable = (*minioObject)(nil)
	_ Blob     = (*minioObject)(nil)
)

// NewMinioBucketStorageProvider 初始化minio存储桶
func NewMinioBucketStorageProvider(_ context.Context, bucketName string) (*MinioBucketStorageProvider, error) {
	minioEndpoint := env.GetEnv("MINIO_ENDPOINT", "127.0.0.1:9000")
	accessKey := env.GetEnv("MINIO_ACCESS_KEY", "minioadmin")
	secretKey := env.GetEnv("MINIO_SECRET_KEY", "minioadmin")
	client, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL, // true=HTTPS, false=HTTP
	})
	if err != nil {
		return nil, err
	}

	return &MinioBucketStorageProvider{
		client:     client,
		bucketName: bucketName,
	}, nil
}

// DeleteObjectsWithPrefix 删除存储桶中指定前缀的对象
func (m *MinioBucketStorageProvider) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	ctx, cancel := context.WithTimeout(ctx, minioOperationTimeout)
	defer cancel()

	objectCh := m.client.ListObjects(ctx, m.bucketName, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})

	var objectsToDelete []minio.ObjectInfo

	for object := range objectCh {
		if object.Err != nil {
			return fmt.Errorf("DeleteObjectsWithPrefix Error: %v", object.Err)
		}
		objectsToDelete = append(objectsToDelete, object)
	}

	// minio delete operation requires at least one object to delete.
	if len(objectsToDelete) == 0 {
		return nil
	}

	deleteCh := make(chan minio.ObjectInfo, len(objectsToDelete))
	go func() {
		defer close(deleteCh)
		for _, object := range objectsToDelete {
			deleteCh <- object
		}
	}()

	errorCh := m.client.RemoveObjects(ctx, m.bucketName, deleteCh, minio.RemoveObjectsOptions{})

	var deleteErrors []string
	for e := range errorCh {
		if e.Err != nil {
			deleteErrors = append(deleteErrors, fmt.Sprintf("delete %s failed: %v", e.ObjectName, e.Err))
		}
	}

	if len(deleteErrors) > 0 {
		return fmt.Errorf("failed to delete: %v", deleteErrors)
	}

	return nil
}

func (m *MinioBucketStorageProvider) GetDetails() string {
	return fmt.Sprintf("[MINIO Storage, bucket set to %s]", m.bucketName)
}

func (m *MinioBucketStorageProvider) UploadSignedURL(ctx context.Context, path string, ttl time.Duration) (string, error) {
	signedURL, err := m.client.PresignedPutObject(
		ctx,
		m.bucketName,
		path,
		ttl,
	)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}

	return signedURL.String(), nil
}

func (m *MinioBucketStorageProvider) OpenSeekable(_ context.Context, path string, _ SeekableObjectType) (Seekable, error) {
	return &minioObject{
		client:     m.client,
		bucketName: m.bucketName,
		path:       path,
	}, nil
}

func (m *MinioBucketStorageProvider) OpenBlob(_ context.Context, path string, _ ObjectType) (Blob, error) {
	return &minioObject{
		client:     m.client,
		bucketName: m.bucketName,
		path:       path,
	}, nil
}
func (m *minioObject) WriteTo(ctx context.Context, dst io.Writer) (int64, error) {
	var lastErr error
	for i := 0; i < 3; i++ {
		n, err := m.writeToOnce(ctx, dst)
		if err == nil {
			return n, nil
		}
		lastErr = err
		if i < 2 {
			fmt.Fprintf(os.Stderr, "[MINIO DEBUG] Retry %d after error: %v\n", i+1, err)
			time.Sleep(2000 * time.Millisecond)
		}
	}
	return 0, lastErr
}

func (m *minioObject) writeToOnce(ctx context.Context, dst io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, minioReadTimeout)
	defer cancel()

	resp, err := m.client.GetObject(ctx, m.bucketName, m.path, minio.GetObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get object (bucket=%s, path=%s): %w",
			m.bucketName, m.path, err)
	}

	buff := make([]byte, minioBufferSize)
	n, err := io.CopyBuffer(dst, resp, buff)
	if err != nil {
		return n, fmt.Errorf("failed to copy MinIO object (bucket=%s, path=%s): %w",
			m.bucketName, m.path, err)
	}
	return n, nil
}

func (m *minioObject) StoreFile(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, minioWriteTimeout)
	defer cancel()

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	_, err = m.client.PutObject(
		ctx,
		m.bucketName,
		m.path,
		file,
		stat.Size(),
		minio.PutObjectOptions{},
	)
	return err
}

func (m *minioObject) Put(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, minioWriteTimeout)
	defer cancel()

	_, err := m.client.PutObject(
		ctx,
		m.bucketName,
		m.path,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{},
	)
	return err
}

func (m *minioObject) OpenRangeReader(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	end := off + length - 1
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, end); err != nil {
		return nil, fmt.Errorf("invalid range: %w", err)
	}

	obj, err := m.client.GetObject(ctx, m.bucketName, m.path, opts)
	if err != nil {
		respErr := minio.ToErrorResponse(err)
		if respErr.Code == "NoSuchKey" || respErr.Code == "NoSuchBucket" {
			return nil, errObjectNotExist
		}
		return nil, fmt.Errorf("failed to create MinIO range reader for %q: %w", m.path, err)
	}

	return obj, nil
}

func (m *minioObject) ReadAt(ctx context.Context, buff []byte, off int64) (n int, err error) {
	ctx, cancel := context.WithTimeout(ctx, minioReadTimeout)
	defer cancel()

	end := off + int64(len(buff)) - 1
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, end); err != nil {
		return 0, fmt.Errorf("invalid range: %w", err)
	}

	obj, err := m.client.GetObject(ctx, m.bucketName, m.path, opts)
	if err != nil {
		respErr := minio.ToErrorResponse(err)
		if respErr.Code == "NoSuchKey" || respErr.Code == "NoSuchBucket" {
			return 0, errObjectNotExist
		}
		return 0, fmt.Errorf("failed to get object: %w", err)
	}
	defer obj.Close()

	n, err = io.ReadFull(obj, buff)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

func (m *minioObject) Size(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, minioOperationTimeout)
	defer cancel()

	resp, err := m.client.StatObject(ctx, m.bucketName, m.path, minio.StatObjectOptions{})
	if err != nil {
		respErr := minio.ToErrorResponse(err)
		if respErr.Code == "NoSuchKey" || respErr.Code == "NoSuchBucket" {
			return 0, errObjectNotExist
		}
		return 0, fmt.Errorf("failed to stat object: %w", err)
	}

	return resp.Size, nil
}

func (m *minioObject) Exists(ctx context.Context) (bool, error) {
	_, err := m.Size(ctx)
	return err == nil, ignoreNotExist(err)
}

func (m *minioObject) Delete(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, minioOperationTimeout)
	defer cancel()

	err := m.client.RemoveObject(ctx, m.bucketName, m.path, minio.RemoveObjectOptions{})
	return err
}

func ignoreNotExist(err error) error {
	if errors.Is(err, errObjectNotExist) {
		return nil
	}
	return err
}

