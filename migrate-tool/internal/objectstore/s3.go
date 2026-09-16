package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type S3Config struct {
	Bucket    string
	Prefix    string
	Region    string
	Endpoint  string
	PathStyle bool
}

const defaultS3Region = "us-east-1"

func (c S3Config) Validate() error {
	_, err := normalizeS3Config(c)

	return err
}

type S3Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string
}

func NewS3Store(ctx context.Context, options S3Config) (*S3Store, error) {
	options, err := normalizeS3Config(options)
	if err != nil {
		return nil, err
	}

	config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(options.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(config, func(value *s3.Options) {
		// AWS S3 可使用虚拟主机寻址；MinIO 等自定义 endpoint 通常需要
		// path-style，因此由配置显式保留这一差异。
		value.UsePathStyle = options.PathStyle
		if options.Endpoint != "" {
			value.BaseEndpoint = aws.String(options.Endpoint)
		}
	})
	return &S3Store{client: client, uploader: manager.NewUploader(client), bucket: options.Bucket, prefix: options.Prefix}, nil
}

func (s *S3Store) Kind() string { return "s3" }

func (s *S3Store) Stat(ctx context.Context, key string) (Info, error) {
	physical, err := s.physicalKey(key)
	if err != nil {
		return Info{}, err
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(physical)})
	if isS3NotFound(err) {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return Info{}, fmt.Errorf("head S3 object %q: %w", key, err)
	}
	info, err := s3ObjectInfo(key, output.ContentLength, output.ETag, output.VersionId, output.LastModified)
	if err != nil {
		return Info{}, fmt.Errorf("inspect S3 object %q: %w", key, err)
	}

	return info, nil
}

func (s *S3Store) Open(ctx context.Context, key, versionID string) (io.ReadCloser, Info, error) {
	return s.open(ctx, key, versionID, "")
}

func (s *S3Store) open(ctx context.Context, key, versionID, expectedETag string) (io.ReadCloser, Info, error) {
	physical, err := s.physicalKey(key)
	if err != nil {
		return nil, Info{}, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(physical)}
	// 有不可变版本时按 VersionID 精确读取；未版本化对象在验证写入时使用
	// If-Match，避免 HEAD 与 GET 之间读到另一次覆盖。
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	if !hasImmutableVersionID(versionID) && expectedETag != "" {
		input.IfMatch = aws.String(expectedETag)
	}
	output, err := s.client.GetObject(ctx, input)
	if isS3NotFound(err) {
		return nil, Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, Info{}, fmt.Errorf("get S3 object %q: %w", key, err)
	}
	info, err := s3ObjectInfo(key, output.ContentLength, output.ETag, output.VersionId, output.LastModified)
	if err != nil {
		closeErr := output.Body.Close()

		return nil, Info{}, fmt.Errorf("inspect S3 object %q: %w", key, errors.Join(err, closeErr))
	}
	if versionID != "" && info.VersionID != versionID {
		closeErr := output.Body.Close()
		versionErr := fmt.Errorf("S3 object %q returned version %q, requested %q", key, info.VersionID, versionID)

		return nil, Info{}, errors.Join(versionErr, closeErr)
	}
	if expectedETag != "" && info.ETag != trimETag(expectedETag) {
		closeErr := output.Body.Close()
		etagErr := fmt.Errorf("S3 object %q returned ETag %q, requested %q", key, info.ETag, trimETag(expectedETag))

		return nil, Info{}, errors.Join(etagErr, closeErr)
	}

	return output.Body, info, nil
}

func (s *S3Store) Put(ctx context.Context, key string, input io.Reader, size int64, expectedSHA256 string) (Info, error) {
	physical, err := s.physicalKey(key)
	if err != nil {
		return Info{}, err
	}
	request := s3PutInput(s.bucket, physical, input, size, expectedSHA256)
	result, err := s.uploader.Upload(ctx, request)
	if err != nil {
		return Info{}, fmt.Errorf("upload S3 object %q: %w", key, err)
	}
	versionID := aws.ToString(result.VersionID)
	etag := aws.ToString(result.ETag)
	if !hasImmutableVersionID(versionID) && trimETag(etag) == "" {
		return Info{}, fmt.Errorf("upload S3 object %q returned no stable version identity", key)
	}
	// 上传成功不是迁移成功。重新下载刚写入的精确版本并计算 SHA-256，随后再
	// HEAD 一次确认验证期间没有发生版本漂移。
	reader, info, err := s.open(ctx, key, versionID, etag)
	if err != nil {
		return Info{}, fmt.Errorf("verify uploaded S3 object %q: %w", key, err)
	}
	hash := sha256.New()
	read, copyErr := io.Copy(hash, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return Info{}, fmt.Errorf("verify uploaded S3 object %q: %w", key, errors.Join(copyErr, closeErr))
	}
	if size >= 0 && read != size {
		return Info{}, fmt.Errorf("uploaded S3 object %q size mismatch: got %d, want %d", key, read, size)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if expectedSHA256 != "" && actual != expectedSHA256 {
		return Info{}, fmt.Errorf("uploaded S3 object %q SHA-256 mismatch: got %s, want %s", key, actual, expectedSHA256)
	}
	after, err := s.Stat(ctx, key)
	if err != nil {
		return Info{}, fmt.Errorf("recheck uploaded S3 object %q: %w", key, err)
	}
	if !info.SameObjectVersion(after) {
		return Info{}, fmt.Errorf("uploaded S3 object %q changed during verification", key)
	}

	return info, nil
}

func s3PutInput(bucket, key string, input io.Reader, size int64, expectedSHA256 string) *s3.PutObjectInput {
	request := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   input,
		// 目标对象冲突必须在计划阶段解决，上传阶段不允许覆盖并发写入。
		IfNoneMatch: aws.String("*"),
	}
	if expectedSHA256 != "" {
		request.Metadata = map[string]string{"template-migrate-sha256": expectedSHA256}
	}
	if size >= 0 {
		request.ContentLength = aws.Int64(size)
	}

	return request
}

func (s *S3Store) physicalKey(key string) (string, error) {
	// logical key 来自 Header 和固定产物命名，必须保持相对、规范且使用“/”。
	if err := validateObjectPath(key, false); err != nil {
		return "", fmt.Errorf("invalid object key %q: %w", key, err)
	}
	if s.prefix == "" {
		return key, nil
	}
	return s.prefix + "/" + key, nil
}

func normalizeS3Config(options S3Config) (S3Config, error) {
	if options.Bucket == "" {
		return S3Config{}, fmt.Errorf("S3 bucket is required")
	}
	if strings.TrimSpace(options.Bucket) != options.Bucket || strings.ContainsAny(options.Bucket, "/\\:") || containsControl(options.Bucket) {
		return S3Config{}, fmt.Errorf("invalid S3 bucket %q", options.Bucket)
	}
	if options.Region == "" {
		options.Region = defaultS3Region
	}
	if strings.TrimSpace(options.Region) != options.Region || containsControl(options.Region) {
		return S3Config{}, fmt.Errorf("invalid S3 region %q", options.Region)
	}
	if err := validateObjectPath(options.Prefix, true); err != nil {
		return S3Config{}, fmt.Errorf("invalid S3 prefix %q: %w", options.Prefix, err)
	}
	if options.Endpoint != "" {
		if err := validateS3Endpoint(options.Endpoint); err != nil {
			return S3Config{}, err
		}
	}

	return options, nil
}

func validateS3Endpoint(value string) error {
	if strings.TrimSpace(value) != value || containsControl(value) {
		return fmt.Errorf("invalid S3 endpoint %q", value)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse S3 endpoint: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.Opaque != "" {
		return fmt.Errorf("S3 endpoint must be an http:// or https:// URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("S3 endpoint must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || strings.ContainsRune(value, '#') {
		return fmt.Errorf("S3 endpoint must not contain a query or fragment")
	}
	if parsed.RawPath != "" || (parsed.Path != "" && (strings.ContainsRune(parsed.Path, '\\') || path.Clean(parsed.Path) != parsed.Path)) {
		return fmt.Errorf("S3 endpoint contains a non-canonical path")
	}

	return nil
}

func validateObjectPath(value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}

		return fmt.Errorf("path is empty")
	}
	if strings.ContainsRune(value, '\\') || containsControl(value) {
		return fmt.Errorf("path contains an unsupported character")
	}
	clean := path.Clean(value)
	if strings.HasPrefix(value, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return fmt.Errorf("path must be relative and canonical")
	}

	return nil
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func s3ObjectInfo(key string, contentLength *int64, etag, versionID *string, lastModified *time.Time) (Info, error) {
	if contentLength == nil || aws.ToInt64(contentLength) < 0 {
		return Info{}, fmt.Errorf("response has no valid content length")
	}
	info := Info{
		Key:       key,
		Size:      aws.ToInt64(contentLength),
		ETag:      trimETag(aws.ToString(etag)),
		VersionID: aws.ToString(versionID),
	}
	if lastModified != nil {
		info.LastModified = lastModified.UTC()
	}
	if !hasImmutableVersionID(info.VersionID) && (info.ETag == "" || info.LastModified.IsZero()) {
		return Info{}, fmt.Errorf("response has no stable version identity")
	}

	return info, nil
}

func trimETag(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}

	return value
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion":
			return true
		default:
			return false
		}
	}
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == 404
}
