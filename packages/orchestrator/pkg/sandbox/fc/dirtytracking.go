package fc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// KVM 的 arm64 硬件脏状态跟踪能力号。x/sys/unix 里没有这两个常量，而且
// KVM_CHECK_EXTENSION 的参数是按值传的能力号，所以这里都写出来。
const (
	kvmCapArmHWDirtyStateTrack = 502
	kvmCheckExtension          = 0xAE03 // _IO(KVMIO, 0x03)
)

// trackDirtyPagesEnabled decides whether every microVM this orchestrator
// starts arms dirty page tracking. That is what lets Firecracker produce Diff
// snapshots, and therefore what makes an incremental checkpoint incremental:
// with it off every checkpoint silently copies all of guest memory instead,
// which on a 2 GiB sandbox is the difference between 40 MiB and 2 GiB written,
// and between tens of milliseconds and a second and a half.
//
// FC_TRACK_DIRTY_PAGES forces the answer either way. Left unset it follows the
// hardware, because the cost does:
//
//   - where the CPU tracks dirty state itself (arm64 HDBSS, KVM capability
//     502) arming it is close to free, so it defaults on and a host that can
//     do incremental checkpoints does them without anyone having to know;
//   - where it does not, the kernel write-protects every clean page and takes
//     a VM exit on its first write, a pure loss for sandboxes that never
//     checkpoint, so it defaults off.
var trackDirtyPagesEnabled, trackDirtyPagesReason = resolveTrackDirtyPages()

func resolveTrackDirtyPages() (bool, string) {
	if raw, ok := os.LookupEnv("FC_TRACK_DIRTY_PAGES"); ok {
		return raw == "true", fmt.Sprintf("FC_TRACK_DIRTY_PAGES=%q", raw)
	}

	if hardwareDirtyTracking() {
		return true, "hardware dirty state tracking present (KVM capability 502)"
	}

	return false, "no hardware dirty state tracking; software tracking costs a VM exit per clean page"
}

// hardwareDirtyTracking asks KVM whether this host tracks guest dirty state in
// hardware. KVM_CHECK_EXTENSION only queries: no VM is created and nothing is
// enabled, so this is safe to run at startup anywhere, including on a host
// with no /dev/kvm at all.
func hardwareDirtyTracking() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()

	ret, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), kvmCheckExtension, kvmCapArmHWDirtyStateTrack)

	return errno == 0 && ret > 0
}

// TrackDirtyPagesEnabled reports whether VMs on this orchestrator are started
// with dirty page tracking armed, which is what makes incremental (Diff)
// snapshots possible.
func TrackDirtyPagesEnabled() bool { return trackDirtyPagesEnabled }

// TrackDirtyPagesReason explains how TrackDirtyPagesEnabled got its value. A
// deployment that quietly lost incremental checkpoints should be able to say
// why from its logs, instead of only being slow.
func TrackDirtyPagesReason() string { return trackDirtyPagesReason }
