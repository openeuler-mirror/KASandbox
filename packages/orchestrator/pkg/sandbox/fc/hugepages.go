package fc

import (
	"fmt"
	"os"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// CheckHugePages rejects a request for Firecracker's 2 MiB hugepage mode on a
// host that cannot back it.
//
// Only 4 KiB base-page hosts can back Firecracker's 2 MiB guest memory
// mappings; on any other base-page size (e.g. 64 KiB ARM64 kernels) the VM
// fails to start. Whether hugepages are used is decided by the caller, so the
// request is rejected instead of being served with host base pages. On 4 KiB
// hosts every request passes, exactly as before this check existed.
func CheckHugePages(requested bool) error {
	return checkHugePages(requested, os.Getpagesize())
}

func checkHugePages(requested bool, hostPageSize int) error {
	if requested && hostPageSize != header.PageSize {
		return fmt.Errorf("2 MiB hugepages requested, but the host base-page size (%d bytes) cannot back them", hostPageSize)
	}

	return nil
}
