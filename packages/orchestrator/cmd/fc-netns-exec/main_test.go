//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const prepareVMChildEnv = "FC_NETNS_EXEC_TEST_PREPARE_VM_CHILD"

func TestPrepareVMValidate(t *testing.T) {
	t.Run("missing flags", func(t *testing.T) {
		p := &prepareVMArgs{}
		if err := p.validate(); err == nil {
			t.Fatal("expected error for empty args")
		}
	})

	t.Run("complete flags", func(t *testing.T) {
		p := &prepareVMArgs{
			sandboxDir: "/fc-vm",
			rootfsSrc:  "/dev/nbd0",
			rootfsName: "rootfs.ext4",
			kernelSrc:  "/fc-kernels/vmlinux",
			kernelDir:  "kernel",
			kernelName: "vmlinux.bin",
		}
		if err := p.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestPrepareVMRun 在独立 mount namespace 中执行完整准备链，验证 tmpfs 挂载
// 与两条符号链接的布局与 V2 startScript 一致。需要创建 mount ns 的权限，
// 不具备时跳过。
func TestPrepareVMRun(t *testing.T) {
	switch os.Getenv(prepareVMChildEnv) {
	case "1":
		runPrepareVMChild(t)

		return
	case "probe":
		return
	}

	// 先跑一个空子进程探测 mount ns 权限，避免污染本测试进程的命名空间
	probe := exec.Command("/proc/self/exe", "-test.run=TestPrepareVMRun")
	probe.Env = append(os.Environ(), prepareVMChildEnv+"=probe")
	probe.SysProcAttr = &syscall.SysProcAttr{Unshareflags: unix.CLONE_NEWNS}
	if err := probe.Run(); err != nil {
		t.Skipf("cannot create mount namespace: %v", err)
	}

	cmd := exec.Command("/proc/self/exe", "-test.run=TestPrepareVMRun")
	cmd.Env = append(os.Environ(), prepareVMChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: unix.CLONE_NEWNS}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed: %v", err)
	}
}

func runPrepareVMChild(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandbox")

	rootfsSrc := filepath.Join(base, "rootfs.ext4")
	kernelSrc := filepath.Join(base, "vmlinux")
	for _, f := range []string{rootfsSrc, kernelSrc} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := &prepareVMArgs{
		sandboxDir: sandboxDir,
		rootfsSrc:  rootfsSrc,
		rootfsName: "rootfs.ext4",
		kernelSrc:  kernelSrc,
		kernelDir:  "kernel",
		kernelName: "vmlinux.bin",
	}
	if err := p.run(); err != nil {
		t.Fatalf("prepare-vm run: %v", err)
	}
	// 卸载 tmpfs，否则 t.TempDir 的 RemoveAll 会因挂载点繁忙失败
	defer func() {
		if err := unix.Unmount(sandboxDir, 0); err != nil {
			t.Fatalf("unmount sandbox dir: %v", err)
		}
	}()

	// tmpfs 已挂载在 sandboxDir 上
	var st unix.Statfs_t
	if err := unix.Statfs(sandboxDir, &st); err != nil {
		t.Fatal(err)
	}
	if st.Type != unix.TMPFS_MAGIC {
		t.Fatalf("sandbox dir is not tmpfs, f_type=%#x", st.Type)
	}

	// rootfs 链接位于 tmpfs 根
	rootfsDst := filepath.Join(sandboxDir, "rootfs.ext4")
	if target, err := os.Readlink(rootfsDst); err != nil || target != rootfsSrc {
		t.Fatalf("rootfs link: target=%q err=%v", target, err)
	}

	// kernel 目录创建于 tmpfs 之内，链接落位
	kernelDst := filepath.Join(sandboxDir, "kernel", "vmlinux.bin")
	if target, err := os.Readlink(kernelDst); err != nil || target != kernelSrc {
		t.Fatalf("kernel link: target=%q err=%v", target, err)
	}
}
