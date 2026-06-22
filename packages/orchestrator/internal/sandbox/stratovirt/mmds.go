package stratovirt

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/keys"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
)

type MmdsMetadata struct {
	SandboxID  string `json:"instanceID"`
	TemplateID string `json:"envID"`

	LogsCollectorAddress string `json:"address"`
	AccessTokenHash      string `json:"accessTokenHash,omitempty"`
}

func newMmdsMetadata(config cfg.BuilderConfig, metadata sbxlogger.SandboxMetadata, accessToken *string) *MmdsMetadata {
	tokenHash := keys.HashAccessToken("")
	if accessToken != nil && *accessToken != "" {
		tokenHash = keys.HashAccessToken(*accessToken)
	}

	return &MmdsMetadata{
		SandboxID:            metadata.SandboxID,
		TemplateID:           metadata.TemplateID,
		LogsCollectorAddress: fmt.Sprintf("http://%s/logs", config.NetworkConfig.OrchestratorInSandboxIPAddress),
		AccessTokenHash:      tokenHash,
	}
}

func (p *Process) setMmds(ctx context.Context, metadata sbxlogger.SandboxMetadata, accessToken *string) error {
	mmdsMeta := newMmdsMetadata(p.config, metadata, accessToken)
	payload := map[string]any{
		"instanceID":      mmdsMeta.SandboxID,
		"envID":           mmdsMeta.TemplateID,
		"address":         mmdsMeta.LogsCollectorAddress,
		"accessTokenHash": mmdsMeta.AccessTokenHash,
	}
	return p.qmpClient.executeCommandWithReturn(ctx, "put-mmds", payload, nil)
}

func (p *Process) setMmdsConfig(ctx context.Context) error {
	payload := map[string]any{
		"version":            "V2",
		"network-interfaces": []string{p.slot.VpeerName()},
	}
	return p.qmpClient.executeCommandWithReturn(ctx, "put-mmds-config", payload, nil)
}
