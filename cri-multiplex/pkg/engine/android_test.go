package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestNewAndroidEngineDefaults(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{})
	if e.cfg.ArtifactsDir != "/home/fjq/cf17" {
		t.Fatalf("ArtifactsDir = %q", e.cfg.ArtifactsDir)
	}
	if e.cfg.ADBPortStart != 6520 || e.cfg.BaseInstanceNumStart != 1 {
		t.Fatalf("port/instance defaults mismatch: %+v", e.cfg)
	}
	if e.cfg.LaunchTimeout != 30*time.Second {
		t.Fatalf("LaunchTimeout = %v, want 30s", e.cfg.LaunchTimeout)
	}
	if e.cfg.CNI.NetNSPrefix != "android-" {
		t.Fatalf("Android CNI prefix = %q, want android-", e.cfg.CNI.NetNSPrefix)
	}
}

func TestAndroidAllocatePortLocked(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{ADBPortStart: 40000})

	p, err := e.allocatePortLocked("", 40000, "a")
	if err != nil || p != 40000 {
		t.Fatalf("auto allocate = (%d, %v), want 40000 nil", p, err)
	}
	p, err = e.allocatePortLocked("40002", 40000, "b")
	if err != nil || p != 40002 {
		t.Fatalf("requested allocate = (%d, %v), want 40002 nil", p, err)
	}
	if _, err := e.allocatePortLocked("40002", 40000, "c"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("conflicting port error code = %v, want ResourceExhausted", status.Code(err))
	}
	if _, err := e.allocatePortLocked("bad", 40000, "c"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid port error code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAndroidAllocateInstanceNumLocked(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{BaseInstanceNumStart: 2})

	n, err := e.allocateInstanceNumLocked("", "a")
	if err != nil || n != 2 {
		t.Fatalf("auto instance = (%d, %v), want 2 nil", n, err)
	}
	n, err = e.allocateInstanceNumLocked("4", "b")
	if err != nil || n != 4 {
		t.Fatalf("requested instance = (%d, %v), want 4 nil", n, err)
	}
	if _, err := e.allocateInstanceNumLocked("4", "c"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("conflicting instance error code = %v, want ResourceExhausted", status.Code(err))
	}
	if _, err := e.allocateInstanceNumLocked("0", "c"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid instance error code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAndroidArtifactsDirForInstance(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{ArtifactsDir: "/home/fjq/cf17", BaseInstanceNumStart: 1})
	if got := e.artifactsDirForInstance(0); got != "/home/fjq/cf17" {
		t.Fatalf("instance 0 dir = %q", got)
	}
	if got := e.artifactsDirForInstance(1); got != "/home/fjq/cf17" {
		t.Fatalf("instance 1 dir = %q", got)
	}
	if got := e.artifactsDirForInstance(2); got != "/home/fjq/cf17-2" {
		t.Fatalf("instance 2 dir = %q", got)
	}
}

func TestAndroidSkipArtifactsCopyEntry(t *testing.T) {
	cases := map[string]bool{
		".cuttlefish_config.json": true,
		"userdata.img.lock":       true,
		"cuttlefish_runtime":      true,
		"cuttlefish_runtime.1":    true,
		"boot.img":                false,
		"bin":                     false,
	}
	for name, want := range cases {
		if got := skipAndroidArtifactsCopyEntry(name); got != want {
			t.Fatalf("skipAndroidArtifactsCopyEntry(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCopyCuttlefishDirWithoutInstances(t *testing.T) {
	src := filepath.Join(t.TempDir(), "cuttlefish")
	dst := filepath.Join(t.TempDir(), "cuttlefish")
	if err := os.MkdirAll(filepath.Join(src, "instances", "cvd-1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "x"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyCuttlefishDirWithoutInstances(src, dst); err != nil {
		t.Fatalf("copyCuttlefishDirWithoutInstances: %v", err)
	}
	if dirExists(filepath.Join(dst, "instances")) {
		t.Fatal("instances directory should not be copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "bin", "x")); err != nil {
		t.Fatalf("expected copied file: %v", err)
	}
}

func TestAndroidStatusAndImageMethods(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{Enabled: true})
	statusResp, err := e.Status(context.Background(), &runtime.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !statusResp.Status.Conditions[0].Status || !statusResp.Status.Conditions[1].Status {
		t.Fatalf("unexpected status conditions: %+v", statusResp.Status.Conditions)
	}

	imgResp, err := e.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: androidDefaultImage}})
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if imgResp.Image == nil || imgResp.Image.Id != androidDefaultImage {
		t.Fatalf("ImageStatus image = %+v", imgResp.Image)
	}

	other, err := e.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: "busybox"}})
	if err != nil {
		t.Fatalf("ImageStatus other: %v", err)
	}
	if other.Image != nil {
		t.Fatalf("non-android image should return nil image, got %+v", other.Image)
	}
}

func TestAndroidUnimplementedMethods(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{Enabled: true})
	if _, err := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("ExecSync code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := e.Exec(context.Background(), &runtime.ExecRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Exec code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := e.Attach(context.Background(), &runtime.AttachRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Attach code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := e.PortForward(context.Background(), &runtime.PortForwardRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("PortForward code = %v, want Unimplemented", status.Code(err))
	}
}

func TestAndroidRecordAccessIPAndPodConfig(t *testing.T) {
	rec := &AndroidSandboxRecord{
		CRISandboxID: "pod-a",
		PodUID:       "uid-a",
		Name:         "name-a",
		Namespace:    "ns-a",
		NodeIP:       "192.0.2.10",
		PodIP:        "10.0.0.10",
		Labels:       map[string]string{"app": "android"},
		Annotations:  map[string]string{"k": "v"},
	}
	if got := rec.accessIP(); got != "10.0.0.10" {
		t.Fatalf("accessIP = %q, want PodIP", got)
	}
	rec.PodIP = ""
	if got := rec.accessIP(); got != "192.0.2.10" {
		t.Fatalf("accessIP fallback = %q, want NodeIP", got)
	}

	cfg := rec.toPodSandboxConfig()
	if cfg.Metadata.Name != "name-a" || cfg.Metadata.Namespace != "ns-a" || cfg.Metadata.Uid != "uid-a" {
		t.Fatalf("metadata mismatch: %+v", cfg.Metadata)
	}
	cfg.Labels["app"] = "changed"
	if rec.Labels["app"] != "android" {
		t.Fatal("toPodSandboxConfig should copy labels")
	}
}
