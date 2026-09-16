package objectstore

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func TestS3ConfigValidate(t *testing.T) {
	t.Parallel()

	valid := []S3Config{
		{Bucket: "templates"},
		{Bucket: "templates", Prefix: "team/templates", Region: "cn-north-1"},
		{Bucket: "templates", Endpoint: "http://127.0.0.1:9000", PathStyle: true},
		{Bucket: "templates", Endpoint: "https://storage.example.test/gateway"},
		{Bucket: "templates", Endpoint: "https://storage.example.test/team%20gateway"},
	}
	for _, config := range valid {
		if err := config.Validate(); err != nil {
			t.Errorf("S3Config.Validate(%+v) returned error: %v", config, err)
		}
	}

	invalid := []S3Config{
		{},
		{Bucket: " templates"},
		{Bucket: "team/templates"},
		{Bucket: "templates:9000"},
		{Bucket: "templates", Region: " us-east-1"},
		{Bucket: "templates", Prefix: "/absolute"},
		{Bucket: "templates", Prefix: "team/../other"},
		{Bucket: "templates", Prefix: "team//templates"},
		{Bucket: "templates", Prefix: "team/templates/"},
		{Bucket: "templates", Prefix: "team\\templates"},
		{Bucket: "templates", Endpoint: "minio:9000"},
		{Bucket: "templates", Endpoint: "ftp://minio:9000"},
		{Bucket: "templates", Endpoint: "http://:9000"},
		{Bucket: "templates", Endpoint: "http://user:secret@minio:9000"},
		{Bucket: "templates", Endpoint: "http://minio:9000?tenant=a"},
		{Bucket: "templates", Endpoint: "http://minio:9000#"},
		{Bucket: "templates", Endpoint: "http://minio:9000/#fragment"},
		{Bucket: "templates", Endpoint: "http://minio:9000/a/../b"},
		{Bucket: "templates", Endpoint: "http://minio:9000/a%2Fb"},
	}
	for _, config := range invalid {
		if err := config.Validate(); err == nil {
			t.Errorf("S3Config.Validate(%+v) succeeded, want error", config)
		}
	}
}

func TestS3PutInputUsesConditionalCreate(t *testing.T) {
	t.Parallel()

	body := strings.NewReader("fixture")
	request := s3PutInput("templates", "prefix/build/rootfs.ext4", body, 7, "digest")
	if aws.ToString(request.Bucket) != "templates" || aws.ToString(request.Key) != "prefix/build/rootfs.ext4" {
		t.Fatalf("unexpected request target: bucket=%q key=%q", aws.ToString(request.Bucket), aws.ToString(request.Key))
	}
	if request.Body != body || aws.ToInt64(request.ContentLength) != 7 {
		t.Fatalf("unexpected request body or length: body=%v length=%d", request.Body, aws.ToInt64(request.ContentLength))
	}
	if aws.ToString(request.IfNoneMatch) != "*" {
		t.Fatalf("IfNoneMatch = %q, want *", aws.ToString(request.IfNoneMatch))
	}
	if request.Metadata["template-migrate-sha256"] != "digest" {
		t.Fatalf("unexpected checksum metadata: %+v", request.Metadata)
	}
}

func TestS3StorePhysicalKey(t *testing.T) {
	t.Parallel()

	store := &S3Store{prefix: "tenant/templates"}
	key, err := store.physicalKey("00112233-4455-6677-8899-aabbccddeeff/rootfs.ext4")
	if err != nil {
		t.Fatal(err)
	}
	const expected = "tenant/templates/00112233-4455-6677-8899-aabbccddeeff/rootfs.ext4"
	if key != expected {
		t.Fatalf("physical key = %q, want %q", key, expected)
	}

	invalid := []string{"", ".", "..", "../outside", "/absolute", "a/../b", "a//b", "a/./b", "a/", "a\\b", "a\x00b"}
	for _, value := range invalid {
		if _, err := store.physicalKey(value); err == nil {
			t.Errorf("physicalKey(%q) succeeded, want error", value)
		}
	}
}

func TestS3ObjectInfoRequiresStableIdentity(t *testing.T) {
	t.Parallel()

	size := int64(10)
	modified := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	info, err := s3ObjectInfo("build/memfile", &size, aws.String(`"etag"`), nil, &modified)
	if err != nil {
		t.Fatal(err)
	}
	if info.ETag != "etag" || info.LastModified.Location() != time.UTC {
		t.Fatalf("unexpected normalized info: %+v", info)
	}

	if _, err := s3ObjectInfo("build/memfile", &size, nil, aws.String("version-1"), nil); err != nil {
		t.Fatalf("immutable VersionID should be sufficient: %v", err)
	}
	if _, err := s3ObjectInfo("build/memfile", &size, aws.String(`"etag"`), aws.String("null"), nil); err == nil {
		t.Fatal("literal null VersionID without Last-Modified succeeded, want error")
	}
	if _, err := s3ObjectInfo("build/memfile", &size, nil, nil, &modified); err == nil {
		t.Fatal("unversioned identity without ETag succeeded, want error")
	}
	if _, err := s3ObjectInfo("build/memfile", nil, aws.String(`"etag"`), nil, &modified); err == nil {
		t.Fatal("identity without content length succeeded, want error")
	}
}

func TestInfoSameObjectVersion(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.August, 3, 4, 0, 0, 0, time.UTC)
	base := Info{Key: "build/memfile", Size: 10, ETag: "etag", LastModified: modified}
	tests := []struct {
		name  string
		left  Info
		right Info
		want  bool
	}{
		{name: "unversioned same", left: base, right: base, want: true},
		{name: "etag changed", left: base, right: withETag(base, "other")},
		{name: "time changed", left: base, right: withTime(base, modified.Add(time.Second))},
		{name: "missing identity", left: Info{Key: base.Key, Size: base.Size}, right: Info{Key: base.Key, Size: base.Size}},
		{name: "immutable version same", left: withVersion(base, "v1"), right: withVersion(withETag(base, "other"), "v1"), want: true},
		{name: "immutable version changed", left: withVersion(base, "v1"), right: withVersion(base, "v2")},
		{name: "null version same", left: withVersion(base, "null"), right: withVersion(base, "null"), want: true},
		{name: "null version time changed", left: withVersion(base, "null"), right: withVersion(withTime(base, modified.Add(time.Second)), "null")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.left.SameObjectVersion(test.right); got != test.want {
				t.Fatalf("SameObjectVersion() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestIsS3NotFound(t *testing.T) {
	t.Parallel()

	responseError := func(status int, cause error) error {
		return &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      cause,
		}
	}
	apiError := func(code string) error {
		return &smithy.GenericAPIError{Code: code, Message: "fixture"}
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "no such key", err: apiError("NoSuchKey"), want: true},
		{name: "no such version", err: apiError("NoSuchVersion"), want: true},
		{name: "generic not found", err: apiError("NotFound"), want: true},
		{name: "no such bucket", err: apiError("NoSuchBucket")},
		{name: "no such bucket with HTTP 404", err: responseError(http.StatusNotFound, apiError("NoSuchBucket"))},
		{name: "unmodeled HTTP 404", err: responseError(http.StatusNotFound, errors.New("fixture")), want: true},
		{name: "unmodeled HTTP 403", err: responseError(http.StatusForbidden, errors.New("fixture"))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isS3NotFound(test.err); got != test.want {
				t.Fatalf("isS3NotFound() = %t, want %t", got, test.want)
			}
		})
	}
}

func withETag(info Info, etag string) Info {
	info.ETag = etag

	return info
}

func withTime(info Info, modified time.Time) Info {
	info.LastModified = modified

	return info
}

func withVersion(info Info, version string) Info {
	info.VersionID = version

	return info
}
