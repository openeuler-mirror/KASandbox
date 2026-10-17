package header

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsEmptyBlockSupportsHostPageSizes(t *testing.T) {
	t.Parallel()

	for _, blockSize := range []int64{PageSize, 64 * 1024, HugepageSize} {
		empty, err := IsEmptyBlock(make([]byte, blockSize), blockSize)
		require.NoError(t, err)
		assert.True(t, empty)

		block := make([]byte, blockSize)
		block[len(block)-1] = 1
		empty, err = IsEmptyBlock(block, blockSize)
		require.NoError(t, err)
		assert.False(t, empty)
	}
}

func TestIsEmptyBlockRejectsInvalidSize(t *testing.T) {
	t.Parallel()

	_, err := IsEmptyBlock(make([]byte, PageSize), 64*1024)
	require.Error(t, err)
}
