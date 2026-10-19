package sandbox

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionInfo_HasHugePages(t *testing.T) {
	// When the environment variable is not set, huge pages are decided by the
	// firecracker version.
	t.Run("falls back to version when env var is not set", func(t *testing.T) {
		removeEnv(t, useHugePagesEnvVar)

		info, err := NewVersionInfo("v1.7.0-release_1234567")
		require.NoError(t, err)
		assert.True(t, info.HasHugePages())

		info, err = NewVersionInfo("v1.6.0-release_1234567")
		require.NoError(t, err)
		assert.False(t, info.HasHugePages())
	})

	t.Run("env var set to true enables huge pages regardless of version", func(t *testing.T) {
		t.Setenv(useHugePagesEnvVar, "true")

		for _, version := range []string{"v1.7.0-release_1234567", "v1.6.0-release_1234567", "v1.0.0"} {
			info, err := NewVersionInfo(version)
			require.NoError(t, err)
			assert.True(t, info.HasHugePages())
		}
	})

	t.Run("env var set to false disables huge pages regardless of version", func(t *testing.T) {
		t.Setenv(useHugePagesEnvVar, "false")

		for _, version := range []string{"v1.7.0-release_1234567", "v1.8.0-release_1234567"} {
			info, err := NewVersionInfo(version)
			require.NoError(t, err)
			assert.False(t, info.HasHugePages())
		}
	})

	t.Run("env var accepts common boolean values", func(t *testing.T) {
		for _, value := range []string{"1", "t", "TRUE", "True"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv(useHugePagesEnvVar, value)

				info, err := NewVersionInfo("v1.0.0")
				require.NoError(t, err)
				assert.True(t, info.HasHugePages())
			})
		}

		for _, value := range []string{"0", "f", "FALSE", "False"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv(useHugePagesEnvVar, value)

				info, err := NewVersionInfo("v1.7.0-release_1234567")
				require.NoError(t, err)
				assert.False(t, info.HasHugePages())
			})
		}
	})

	t.Run("falls back to version when env var is invalid", func(t *testing.T) {
		t.Setenv(useHugePagesEnvVar, "not-a-bool")

		info, err := NewVersionInfo("v1.7.0-release_1234567")
		require.NoError(t, err)
		assert.True(t, info.HasHugePages())

		info, err = NewVersionInfo("v1.6.0-release_1234567")
		require.NoError(t, err)
		assert.False(t, info.HasHugePages())
	})
}

// removeEnv unsets the environment variable for the duration of the test and
// restores the previous value afterwards.
func removeEnv(t *testing.T, key string) {
	t.Helper()

	prevValue, ok := os.LookupEnv(key)

	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("cannot unset environment variable: %v", err)
	}

	if ok {
		t.Cleanup(func() {
			os.Setenv(key, prevValue) //nolint:usetesting // restoring a pre-existing value
		})
	} else {
		t.Cleanup(func() {
			os.Unsetenv(key)
		})
	}
}
