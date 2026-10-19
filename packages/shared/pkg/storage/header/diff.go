package header

import (
	"bytes"
	"fmt"

	"go.opentelemetry.io/otel"
)

const (
	PageSize        = 2 << 11
	HugepageSize    = 2 << 20
	RootfsBlockSize = 2 << 11
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/shared/pkg/storage/header")

var (
	EmptyHugePage = make([]byte, HugepageSize)
	EmptyBlock    = make([]byte, RootfsBlockSize)
)

func IsEmptyBlock(block []byte, blockSize int64) (bool, error) {
	// Standard block sizes keep the original code path unchanged.
	switch blockSize {
	case HugepageSize:
		return bytes.Equal(block, EmptyHugePage), nil
	case RootfsBlockSize:
		return bytes.Equal(block, EmptyBlock), nil
	}

	// Other block sizes only occur on hosts whose base page differs from
	// 4 KiB (e.g. 64 KiB ARM64 kernels such as openEuler 4.19).
	if blockSize <= 0 {
		return false, fmt.Errorf("block size must be positive: %d", blockSize)
	}
	if int64(len(block)) != blockSize {
		return false, fmt.Errorf("block length does not match block size: %d != %d", len(block), blockSize)
	}

	for _, value := range block {
		if value != 0 {
			return false, nil
		}
	}

	return true, nil
}
