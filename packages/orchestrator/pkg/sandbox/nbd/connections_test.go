package nbd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConnectionsPerDevice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		flagValue int
		linux419  bool
		want      int
	}{
		{name: "other kernel keeps the flag value", flagValue: 4, linux419: false, want: 4},
		{name: "other kernel keeps a single connection", flagValue: 1, linux419: false, want: 1},
		{name: "linux 4.19 uses one connection", flagValue: 4, linux419: true, want: 1},
		{name: "linux 4.19 keeps a single connection", flagValue: 1, linux419: true, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, connectionsPerDevice(tt.flagValue, tt.linux419))
		})
	}
}
