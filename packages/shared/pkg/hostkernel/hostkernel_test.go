package hostkernel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsLinux419Release(t *testing.T) {
	t.Parallel()

	tests := []struct {
		release string
		want    bool
	}{
		{"4.19", true},
		{"4.19.0", true},
		{"4.19.90-2112.8.0.0131.oe1.aarch64", true},
		{"4.19.90-89.11.v2401.ky10.aarch64", true},
		{"4.190", false},
		{"4.1", false},
		{"14.19.1", false},
		{"5.10.0-60.18.0.50.oe2203.aarch64", false},
		{"6.6.0-145.0.13.139.oe2403.aarch64", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.release, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, isLinux419Release(tt.release))
		})
	}
}
