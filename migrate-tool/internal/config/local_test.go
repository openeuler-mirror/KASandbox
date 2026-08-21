package config

import (
	"net/url"
	"path/filepath"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
)

func TestParseS3(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("TM_S3_ENDPOINT", "")

	endpointQuery := url.Values{
		"endpoint":   {"http://minio.internal:9000"},
		"path_style": {"false"},
		"region":     {"cn-north-1"},
	}.Encode()
	tests := []struct {
		name     string
		endpoint string
		want     objectstore.S3Config
	}{
		{
			name:     "AWS defaults",
			endpoint: "s3://templates",
			want:     objectstore.S3Config{Bucket: "templates", Region: "us-east-1"},
		},
		{
			name:     "prefix",
			endpoint: "s3://templates/team/cache?region=eu-west-1",
			want:     objectstore.S3Config{Bucket: "templates", Prefix: "team/cache", Region: "eu-west-1"},
		},
		{
			name:     "equivalent empty syntax",
			endpoint: "s3://templates/?",
			want:     objectstore.S3Config{Bucket: "templates", Region: "us-east-1"},
		},
		{
			name:     "escaped slash is a prefix separator",
			endpoint: "s3://templates/team%2Fcache",
			want:     objectstore.S3Config{Bucket: "templates", Prefix: "team/cache", Region: "us-east-1"},
		},
		{
			name:     "escaped prefix character",
			endpoint: "s3://templates/team%20cache",
			want:     objectstore.S3Config{Bucket: "templates", Prefix: "team cache", Region: "us-east-1"},
		},
		{
			name:     "custom endpoint overrides path-style default",
			endpoint: "s3://templates?" + endpointQuery,
			want: objectstore.S3Config{Bucket: "templates", Region: "cn-north-1",
				Endpoint: "http://minio.internal:9000", PathStyle: false},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseS3(test.endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("parseS3() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseMooncakeNamespace(t *testing.T) {
	namespace, err := parseMooncake("mooncake://templates-test")
	if err != nil {
		t.Fatal(err)
	}
	if namespace != "templates-test" {
		t.Fatalf("parseMooncake() = %q, want templates-test", namespace)
	}
}

func TestParseMooncakeRejectsMissingOrAmbiguousNamespace(t *testing.T) {
	invalid := []string{
		"mooncake://",
		"mooncake://templates/path",
		"mooncake://templates?endpoint=other",
		"mooncake://templates:50051",
		"mooncake://10.0.0.5:50051",
	}
	for _, endpoint := range invalid {
		if _, err := parseMooncake(endpoint); err == nil {
			t.Errorf("parseMooncake(%q) succeeded, want error", endpoint)
		}
	}
}

func TestLocalPathDecodesFileURIOnce(t *testing.T) {
	got, err := localPath("file:///tmp/literal%252Fname")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.FromSlash("/tmp/literal%2Fname")
	if got != want {
		t.Fatalf("localPath() = %q, want %q", got, want)
	}
}

func TestLocalPathRejectsMalformedSchemes(t *testing.T) {
	invalid := []string{"mooncake:/templates", "s3:/bucket/prefix", "postgresql:db"}
	for _, endpoint := range invalid {
		if _, err := localPath(endpoint); err == nil {
			t.Errorf("localPath(%q) succeeded, want error", endpoint)
		}
	}
	// Windows 盘符和含冒号的普通路径不受影响。
	for _, endpoint := range []string{`D:\templates`, "D:/templates", "/tmp/a:b"} {
		if got, err := localPath(endpoint); err != nil || got != endpoint {
			t.Errorf("localPath(%q) = %q, %v; want passthrough", endpoint, got, err)
		}
	}
}

func TestParseS3EnvironmentDefaults(t *testing.T) {
	t.Setenv("AWS_REGION", "ap-southeast-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	t.Setenv("TM_S3_ENDPOINT", "https://minio.internal:9443")

	got, err := parseS3("s3://templates/prefix")
	if err != nil {
		t.Fatal(err)
	}
	want := objectstore.S3Config{Bucket: "templates", Prefix: "prefix", Region: "ap-southeast-1",
		Endpoint: "https://minio.internal:9443", PathStyle: true}
	if got != want {
		t.Fatalf("parseS3() = %+v, want %+v", got, want)
	}
}

func TestParseS3RejectsAmbiguousURI(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("TM_S3_ENDPOINT", "")

	invalid := []string{
		"http://templates",
		"s3://",
		"s3:///prefix",
		"s3://user:secret@templates",
		"s3://templates:9000",
		"s3://templates#",
		"s3://templates/prefix#fragment",
		"s3://templates/a/../b",
		"s3://templates/a//b",
		"s3://templates/a%5Cb",
		"s3://templates?path_style=maybe",
		"s3://templates?region=",
		"s3://templates?endpoint=",
		"s3://templates?endpoint=ftp%3A%2F%2Fminio%3A9000",
		"s3://templates?endpoint=http%3A%2F%2Fuser%3Asecret%40minio%3A9000",
		"s3://templates?unknown=value",
		"s3://templates?region=a&region=b",
		"s3://templates?region=a;b",
	}
	for _, endpoint := range invalid {
		if _, err := parseS3(endpoint); err == nil {
			t.Errorf("parseS3(%q) succeeded, want error", endpoint)
		}
	}
}
