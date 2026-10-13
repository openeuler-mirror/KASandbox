package network

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTapNeighSysctls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		retransTimeMs int
		probes        int
		want          []neighSysctl
	}{
		{
			name:          "probe counts are raised before the interval is shortened",
			retransTimeMs: 10,
			probes:        190,
			want: []neighSysctl{
				{name: "mcast_solicit", value: "190"},
				{name: "ucast_solicit", value: "190"},
				{name: "retrans_time_ms", value: "10"},
			},
		},
		{
			name:          "zero interval keeps kernel defaults",
			retransTimeMs: 0,
			probes:        190,
		},
		{
			name:          "zero probes keeps kernel defaults",
			retransTimeMs: 10,
			probes:        0,
		},
		{
			name:          "negative values keep kernel defaults",
			retransTimeMs: -1,
			probes:        -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tapNeighSysctls(tt.retransTimeMs, tt.probes))
		})
	}
}

// kernelDefaults 在临时目录里模拟 /proc/sys/net/ipv4/neigh/<dev> 的初始内容。
func kernelDefaults(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for name, value := range map[string]string{
		"mcast_solicit":   "3\n",
		"ucast_solicit":   "3\n",
		"retrans_time_ms": "1000\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644))
	}

	return dir
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

func readSysctl(t *testing.T, dir, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)

	return string(data)
}

func TestApplyNeighSysctls(t *testing.T) {
	t.Parallel()

	dir := kernelDefaults(t)

	require.NoError(t, applyNeighSysctls(dir, tapNeighSysctls(10, 190), writeFile))

	assert.Equal(t, "190", readSysctl(t, dir, "mcast_solicit"))
	assert.Equal(t, "190", readSysctl(t, dir, "ucast_solicit"))
	assert.Equal(t, "10", readSysctl(t, dir, "retrans_time_ms"))
}

func TestApplyNeighSysctlsRestoresOnFailure(t *testing.T) {
	t.Parallel()

	dir := kernelDefaults(t)
	errReadOnly := errors.New("read-only file system")

	err := applyNeighSysctls(dir, tapNeighSysctls(10, 190), func(path, value string) error {
		// 模拟最后一步失败：探测次数已放大，间隔却没能缩短
		if filepath.Base(path) == "retrans_time_ms" {
			return errReadOnly
		}

		return writeFile(path, value)
	})

	require.ErrorIs(t, err, errReadOnly)
	assert.Equal(t, "3", readSysctl(t, dir, "mcast_solicit"))
	assert.Equal(t, "3", readSysctl(t, dir, "ucast_solicit"))
	assert.Equal(t, "1000\n", readSysctl(t, dir, "retrans_time_ms"))
}

func TestApplyNeighSysctlsMissingDevice(t *testing.T) {
	t.Parallel()

	err := applyNeighSysctls(filepath.Join(t.TempDir(), "tap0"), tapNeighSysctls(10, 190), writeFile)

	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestParseConfigTapNeighDefaults(t *testing.T) {
	t.Parallel()

	config, err := ParseConfig()
	require.NoError(t, err)

	assert.Equal(t, 10, config.TapNeighRetransTimeMs)
	assert.Equal(t, 190, config.TapNeighProbes)
}
