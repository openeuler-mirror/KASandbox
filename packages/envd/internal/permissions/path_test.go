package permissions

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetSubpathsStopsAtVolumeRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Clean(filepath.VolumeName(filepath.Join("D:", "envd")) + string(filepath.Separator))
	if root == string(filepath.Separator) {
		root = string(filepath.Separator)
	}

	path := filepath.Join(root, "envd", "envd-windows", "test", "tmp")
	subpaths := getSubpaths(path)

	assert.NotEmpty(t, subpaths)
	assert.Equal(t, path, subpaths[len(subpaths)-1])
}
