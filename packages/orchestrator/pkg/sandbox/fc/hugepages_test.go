package fc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckHugePagesPassesOn4KiBHosts(t *testing.T) {
	t.Parallel()

	assert.NoError(t, checkHugePages(true, 4*1024))
	assert.NoError(t, checkHugePages(false, 4*1024))
}

func TestCheckHugePagesRejectsRequestOnNon4KiBHosts(t *testing.T) {
	t.Parallel()

	assert.Error(t, checkHugePages(true, 64*1024))
	assert.Error(t, checkHugePages(true, 16*1024))
}

func TestCheckHugePagesAllowsBasePagesOnNon4KiBHosts(t *testing.T) {
	t.Parallel()

	assert.NoError(t, checkHugePages(false, 64*1024))
	assert.NoError(t, checkHugePages(false, 16*1024))
}
