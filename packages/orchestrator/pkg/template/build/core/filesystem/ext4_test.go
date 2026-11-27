//go:build linux

package filesystem

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Make requires a block size >= inodesRatio; 4 KiB is what the rootfs uses.
const makeBlockSize = 4096

// TestMakeDirIndex pins both sides of the build-ext4-dir-index switch: nothing
// else exercises the enabled path until the flag is turned on.
func TestMakeDirIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    MakeOptions
		indexed bool
	}{
		{name: "enabled keeps the mkfs base feature", opts: MakeOptions{DirIndex: true}, indexed: true},
		{name: "disabled strips the index", opts: MakeOptions{DirIndex: false}, indexed: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requireTools(t, "mkfs.ext4", "dumpe2fs")

			rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
			require.NoError(t, Make(t.Context(), rootfsPath, 64, makeBlockSize, tc.opts))

			features := filesystemFeatures(t, rootfsPath)
			assert.Equal(t, tc.indexed, slices.Contains(features, "dir_index"),
				"dir_index in mkfs.ext4 feature set %v", features)
		})
	}
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()

	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("requires %s to inspect an ext4 filesystem: %v", tool, err)
		}
	}
}

// filesystemFeatures returns the feature names dumpe2fs reports for the image.
func filesystemFeatures(t *testing.T, rootfsPath string) []string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "dumpe2fs", "-h", rootfsPath)
	// The parser below relies on dumpe2fs's English field names.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.Output()
	require.NoError(t, err, "dumpe2fs -h %s", rootfsPath)

	const prefix = "Filesystem features:"
	for line := range strings.Lines(string(output)) {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.Fields(after)
		}
	}

	t.Fatalf("no %q line in dumpe2fs output:\n%s", prefix, output)

	return nil
}

func TestParseFreeBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{
			name: "sums group counters instead of stale superblock",
			input: `Free blocks:              50000
 Group  0: block bitmap at 1, inode bitmap at 2, inode table at 3
           28629 free blocks, 100 free inodes, 1 used directory
 Group  1: block bitmap at 4, inode bitmap at 5, inode table at 6
           28639 free blocks, 100 free inodes, 1 used directory
`,
			expected: 57268,
		},
		{
			name: "supports singular block counter",
			input: ` Group  0: block bitmap at 1, inode bitmap at 2, inode table at 3
           1 free block, 1 free inode, 1 used directory
`,
			expected: 1,
		},
		{
			name: "rejects incomplete group output",
			input: ` Group  0: block bitmap at 1, inode bitmap at 2, inode table at 3
           10 free blocks, 100 free inodes, 1 used directory
 Group  1: block bitmap at 4, inode bitmap at 5, inode table at 6
`,
			wantErr: true,
		},
		{
			name:    "rejects global counter without block groups",
			input:   "Free blocks:              50000\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := parseFreeBlocks(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestParseReservedBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{
			name:     "standard debugfs output",
			input:    "Block count:              131072\nReserved block count:     6553\nFree blocks:              120000\n",
			expected: 6553,
		},
		{
			name:     "zero reserved blocks",
			input:    "Reserved block count:     0\n",
			expected: 0,
		},
		{
			name:    "missing reserved blocks",
			input:   "Block count:              131072\nFree blocks:              120000\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := parseReservedBlocks(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// TestLegacyOverlayMountOptions pins the checks of the mount(2) fallback: the kernel keeps one page of mount
// data (the terminating NUL included) and reads ':' and ',' as separators.
func TestLegacyOverlayMountOptions(t *testing.T) {
	t.Parallel()

	page := os.Getpagesize()
	fits := "/" + strings.Repeat("a", page-len("lowerdir=")-2)

	tests := []struct {
		name    string
		layers  []string
		want    string
		wantErr string
	}{
		{name: "layers in order", layers: []string{"/l/top", "/l/base"}, want: "lowerdir=/l/top:/l/base"},
		{name: "exactly one page", layers: []string{fits}, want: "lowerdir=" + fits},
		{name: "one byte over a page", layers: []string{fits + "a"}, wantErr: "exceed page size"},
		{name: "many layers over a page", layers: slices.Repeat([]string{"/l/" + strings.Repeat("x", 60)}, page/60), wantErr: "exceed page size"},
		{name: "no layers", layers: nil, wantErr: "at least one lower layer"},
		{name: "empty layer", layers: []string{"/l/top", ""}, wantErr: "cannot be empty"},
		{name: "colon in a layer", layers: []string{"/l/a:b"}, wantErr: "separator"},
		{name: "comma in a layer", layers: []string{"/l/a,b"}, wantErr: "separator"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := legacyOverlayMountOptions(tc.layers)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, got)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got)+1, page)
		})
	}
}
