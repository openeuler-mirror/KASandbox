// Package hostkernel reports facts about the kernel the current process
// runs on. Linux 4.19 specific workarounds check IsLinux419 so that other
// kernels keep the upstream behavior.
package hostkernel

import (
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// IsLinux419 reports whether the host kernel is Linux 4.19 (e.g. openEuler
// 20.03 LTS). The result is computed once and cached.
func IsLinux419() bool {
	return isLinux419()
}

var isLinux419 = sync.OnceValue(func() bool {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return false
	}

	return isLinux419Release(unix.ByteSliceToString(uts.Release[:]))
})

// isLinux419Release reports whether a kernel release string, as printed by
// `uname -r`, belongs to Linux 4.19.
func isLinux419Release(release string) bool {
	return release == "4.19" || strings.HasPrefix(release, "4.19.")
}
