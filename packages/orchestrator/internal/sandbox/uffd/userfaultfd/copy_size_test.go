package userfaultfd

import (
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestUffdCopySize(t *testing.T) {
	hostPageSize := uintptr(block.PAGE_SIZE)

	// A region using a page size smaller than (or equal to) the host page size
	// must still be copied in full host pages, because UFFDIO_COPY requires
	// host-page alignment and length.
	if got := uffdCopySize(uintptr(header.PageSize)); got != hostPageSize {
		t.Fatalf("uffdCopySize(%d) = %d, want host page size %d", header.PageSize, got, hostPageSize)
	}

	// A hugepage region is already a multiple of the host page size, so the
	// larger region page size is used as-is.
	if got := uffdCopySize(uintptr(header.HugepageSize)); got != uintptr(header.HugepageSize) {
		t.Fatalf("uffdCopySize(%d) = %d, want %d", header.HugepageSize, got, header.HugepageSize)
	}
}
