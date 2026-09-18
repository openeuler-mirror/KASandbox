package network

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveEgressMode(t *testing.T) {
	t.Parallel()

	capableCfg := Config{SandboxEgressProxyMode: EgressProxyModePerSandbox}

	tests := []struct {
		name       string
		cfg        Config
		externalNS bool
		metadata   map[string]string
		wantMode   EgressMode
		wantCode   codes.Code
	}{
		{
			name:     "explicit per-sandbox accepted",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeyEgressMode: "per-sandbox", MetadataKeySandboxMIS: "mis-1"},
			wantMode: EgressModePerSandbox,
		},
		{
			name:     "explicit off short-circuits",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeyEgressMode: "off", MetadataKeySandboxMIS: "mis-1"},
			wantMode: EgressModeOff,
		},
		{
			name:     "derived per-sandbox when capable and identity present",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeySandboxMIS: "mis-1"},
			wantMode: EgressModePerSandbox,
		},
		{
			name:     "derived off without identity",
			cfg:      capableCfg,
			metadata: nil,
			wantMode: EgressModeOff,
		},
		{
			name:     "derived off when node not capable",
			cfg:      Config{},
			metadata: map[string]string{MetadataKeySandboxMIS: "mis-1"},
			wantMode: EgressModeOff,
		},
		{
			name:     "explicit per-sandbox without node capability fails precondition",
			cfg:      Config{},
			metadata: map[string]string{MetadataKeyEgressMode: "per-sandbox", MetadataKeySandboxMIS: "mis-1"},
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "per-sandbox on CNI sandbox downgrades to off",
			cfg:        capableCfg,
			externalNS: true,
			metadata:   map[string]string{MetadataKeyEgressMode: "per-sandbox", MetadataKeySandboxMIS: "mis-1"},
			wantMode:   EgressModeOff,
		},
		{
			name:       "derived per-sandbox on CNI sandbox downgrades to off",
			cfg:        capableCfg,
			externalNS: true,
			metadata:   map[string]string{MetadataKeySandboxMIS: "mis-1"},
			wantMode:   EgressModeOff,
		},
		{
			name:       "CNI downgrade never fails even without node capability",
			cfg:        Config{},
			externalNS: true,
			metadata:   map[string]string{MetadataKeyEgressMode: "per-sandbox", MetadataKeySandboxMIS: "mis-1"},
			wantMode:   EgressModeOff,
		},
		{
			name:     "per-sandbox with empty sandbox-mis is invalid",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeyEgressMode: "per-sandbox"},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "invalid egress-mode value",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeyEgressMode: "gateway", MetadataKeySandboxMIS: "mis-1"},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "explicit off wins over derivation",
			cfg:      capableCfg,
			metadata: map[string]string{MetadataKeyEgressMode: "off"},
			wantMode: EgressModeOff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mode, err := ResolveEgressMode(context.Background(), tt.cfg, tt.externalNS, "sbx-1", tt.metadata)
			if tt.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tt.wantCode, status.Code(err))
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestEgressIdentityFromMetadata(t *testing.T) {
	t.Parallel()

	assert.Nil(t, EgressIdentityFromMetadata(nil))
	assert.Nil(t, EgressIdentityFromMetadata(map[string]string{"unrelated": "x"}))

	got := EgressIdentityFromMetadata(map[string]string{
		MetadataKeyEgressMode:     "per-sandbox",
		MetadataKeyEgressUpstream: "http://proxy:8080",
		MetadataKeySandboxMIS:     "mis-1",
		MetadataKeyEgressProfile:  "internal",
		MetadataKeyEgressMitm:     "true",
		"unrelated":               "dropped",
	})
	assert.Equal(t, map[string]string{
		MetadataKeyEgressMode:     "per-sandbox",
		MetadataKeyEgressUpstream: "http://proxy:8080",
		MetadataKeySandboxMIS:     "mis-1",
		MetadataKeyEgressProfile:  "internal",
		MetadataKeyEgressMitm:     "true",
	}, got)
}
