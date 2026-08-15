package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	templatecache "github.com/e2b-dev/infra/packages/api/internal/cache/templates"
	dbapi "github.com/e2b-dev/infra/packages/api/internal/db"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
	sharedUtils "github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// jsonRoundtrip converts a free-form object (map[string]interface{} or []map[string]interface{})
// from a request/response DTO to a typed struct (and back) via JSON.
func jsonRoundtrip[T any](v any) (T, error) {
	var out T

	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}

	err = json.Unmarshal(raw, &out)

	return out, err
}

// pausedSandboxConfigFromBody assembles a PausedSandboxConfig from the free-form
// network/auto_resume/volume_mounts fields of the request body.
func pausedSandboxConfigFromBody(body api.RuntimeSnapshotMetadata) (*types.PausedSandboxConfig, error) {
	config := &types.PausedSandboxConfig{Version: types.PausedSandboxConfigVersion}

	if body.Network != nil {
		network, err := jsonRoundtrip[types.SandboxNetworkConfig](*body.Network)
		if err != nil {
			return nil, fmt.Errorf("invalid network config: %w", err)
		}

		config.Network = &network
	}

	if body.AutoResume != nil {
		autoResume, err := jsonRoundtrip[types.SandboxAutoResumeConfig](*body.AutoResume)
		if err != nil {
			return nil, fmt.Errorf("invalid auto_resume config: %w", err)
		}

		config.AutoResume = &autoResume
	}

	if body.VolumeMounts != nil {
		volumeMounts, err := jsonRoundtrip[[]*types.SandboxVolumeMountConfig](*body.VolumeMounts)
		if err != nil {
			return nil, fmt.Errorf("invalid volume_mounts config: %w", err)
		}

		config.VolumeMounts = volumeMounts
	}

	return config, nil
}

// runtimeMetadataToUpsertParams maps the RuntimeSnapshotMetadata DTO collected by the
// webhook service onto UpsertSnapshotParams. Free-form objects are converted via JSON;
// source_machine_info maps to the machineinfo.MachineInfo JSON layout (cpu_* keys).
func runtimeMetadataToUpsertParams(sandboxID api.SandboxID, body api.RuntimeSnapshotMetadata) (queries.UpsertSnapshotParams, error) {
	config, err := pausedSandboxConfigFromBody(body)
	if err != nil {
		return queries.UpsertSnapshotParams{}, err
	}

	metadata := types.JSONBStringMap{}
	if body.Metadata != nil {
		metadata = *body.Metadata
	}

	autoPause := false
	if body.AutoPause != nil {
		autoPause = *body.AutoPause
	}

	var cpuArchitecture, cpuFamily, cpuModel, cpuModelName *string

	var cpuFlags []string

	if body.SourceMachineInfo != nil {
		machineInfo, err := jsonRoundtrip[machineinfo.MachineInfo](*body.SourceMachineInfo)
		if err != nil {
			return queries.UpsertSnapshotParams{}, fmt.Errorf("invalid source_machine_info: %w", err)
		}

		cpuArchitecture = sharedUtils.ToPtr(machineInfo.CPUArchitecture)
		cpuFamily = sharedUtils.ToPtr(machineInfo.CPUFamily)
		cpuModel = sharedUtils.ToPtr(machineInfo.CPUModel)
		cpuModelName = sharedUtils.ToPtr(machineInfo.CPUModelName)
		cpuFlags = machineInfo.CPUFlags
	}

	return queries.UpsertSnapshotParams{
		// Used if there's no snapshot for this sandbox yet
		TemplateID:          id.Generate(),
		TeamID:              body.TeamId,
		SandboxID:           sandboxID,
		BaseTemplateID:      body.BaseTemplateId,
		Metadata:            metadata,
		StartedAt:           pgtype.Timestamptz{Time: body.SandboxStartedAt, Valid: true},
		Secure:              body.Secure,
		AllowInternetAccess: body.AllowInternetAccess,
		OriginNodeID:        body.SourceNodeName,
		AutoPause:           autoPause,
		Config:              config,
		Vcpu:                body.Vcpu,
		RamMb:               body.RamMb,
		// We don't know this information
		FreeDiskSizeMb:     0,
		KernelVersion:      body.KernelVersion,
		FirecrackerVersion: body.FirecrackerVersion,
		EnvdVersion:        &body.EnvdVersion,
		Status:             types.BuildStatusSnapshotting,
		TotalDiskSizeMb:    &body.TotalDiskSizeMb,
		CpuArchitecture:    cpuArchitecture,
		CpuFamily:          cpuFamily,
		CpuModel:           cpuModel,
		CpuModelName:       cpuModelName,
		CpuFlags:           cpuFlags,
	}, nil
}

// machineInfoMap assembles the free-form machine_info response object from the
// cpu_* build columns. Returns nil when no CPU info was recorded.
func machineInfoMap(arch, family, model, modelName *string, flags []string) *map[string]interface{} {
	if arch == nil && family == nil && model == nil && modelName == nil && len(flags) == 0 {
		return nil
	}

	info := map[string]interface{}{}
	if arch != nil {
		info["cpu_architecture"] = *arch
	}

	if family != nil {
		info["cpu_family"] = *family
	}

	if model != nil {
		info["cpu_model"] = *model
	}

	if modelName != nil {
		info["cpu_model_name"] = *modelName
	}

	if len(flags) > 0 {
		info["cpu_flags"] = flags
	}

	return &info
}

// resumeMetadataFromLastSnapshot maps a GetLastSnapshot row to the ResumeMetadata DTO.
func resumeMetadataFromLastSnapshot(row queries.GetLastSnapshotRow) api.ResumeMetadata {
	result := api.ResumeMetadata{
		TemplateId:         row.Snapshot.EnvID,
		BuildId:            row.EnvBuild.ID,
		BaseTemplateId:     &row.Snapshot.BaseEnvID,
		OriginNode:         row.Snapshot.OriginNodeID,
		Vcpu:               row.EnvBuild.Vcpu,
		RamMb:              row.EnvBuild.RamMb,
		TotalDiskSizeMb:    row.EnvBuild.TotalDiskSizeMb,
		KernelVersion:      row.EnvBuild.KernelVersion,
		FirecrackerVersion: row.EnvBuild.FirecrackerVersion,
		EnvdVersion:        row.EnvBuild.EnvdVersion,
		EnvSecure:          row.Snapshot.EnvSecure,
		AutoPause:          row.Snapshot.AutoPause,
		SandboxStartedAt:   row.Snapshot.SandboxStartedAt.Time,
		MachineInfo:        machineInfoMap(row.EnvBuild.CpuArchitecture, row.EnvBuild.CpuFamily, row.EnvBuild.CpuModel, row.EnvBuild.CpuModelName, row.EnvBuild.CpuFlags),
	}

	if len(row.Aliases) > 0 {
		result.Aliases = &row.Aliases
	}

	if row.Snapshot.Metadata != nil {
		metadata := map[string]string(row.Snapshot.Metadata)
		result.Metadata = &metadata
	}

	fillConfigFields(&result, row.Snapshot.Config)

	return result
}

// fillConfigFields splits the snapshots.config jsonb back into the free-form
// network/auto_resume/volume_mounts response fields.
func fillConfigFields(result *api.ResumeMetadata, config *types.PausedSandboxConfig) {
	if config == nil {
		return
	}

	if config.Network != nil {
		if network, err := jsonRoundtrip[map[string]interface{}](*config.Network); err == nil {
			result.Network = &network
		}
	}

	if config.AutoResume != nil {
		if autoResume, err := jsonRoundtrip[map[string]interface{}](*config.AutoResume); err == nil {
			result.AutoResume = &autoResume
		}
	}

	if len(config.VolumeMounts) > 0 {
		if volumeMounts, err := jsonRoundtrip[[]map[string]interface{}](config.VolumeMounts); err == nil {
			result.VolumeMounts = &volumeMounts
		}
	}
}

// resolveInternalTemplate resolves a template identifier (template ID or alias) without a
// user auth context — the team comes from the request and its slug is the alias namespace
// fallback. Returns 422 when the resolved template belongs to a different team.
func (a *APIStore) resolveInternalTemplate(ctx context.Context, identifier string, teamID uuid.UUID) (*templatecache.AliasInfo, *api.APIError) {
	team, err := dbapi.GetTeamByID(ctx, a.authDB, teamID)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			return nil, &api.APIError{
				Code:      http.StatusNotFound,
				ClientMsg: fmt.Sprintf("team '%s' not found", teamID),
				Err:       err,
			}
		}

		return nil, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "Error getting team",
			Err:       err,
		}
	}

	aliasInfo, err := a.templateCache.ResolveAlias(ctx, identifier, team.Slug)
	if err != nil {
		return nil, templatecache.ErrorToAPIError(err, identifier)
	}

	if aliasInfo.TeamID != teamID {
		return nil, &api.APIError{
			Code:      http.StatusUnprocessableEntity,
			ClientMsg: fmt.Sprintf("template '%s' does not belong to team '%s'", identifier, teamID),
			Err:       fmt.Errorf("team '%s' does not own template '%s'", teamID, aliasInfo.TemplateID),
		}
	}

	return aliasInfo, nil
}

// resolveInternalTemplateForDelete resolves an identifier for deletion. When the identifier
// no longer resolves (aliases deleted, env soft-deleted) it falls back to a direct env lookup
// including soft-deleted rows, so repeat deletes stay idempotent (alreadyDeleted=true).
func (a *APIStore) resolveInternalTemplateForDelete(ctx context.Context, identifier string, teamID uuid.UUID) (templateID string, alreadyDeleted bool, apiErr *api.APIError) {
	aliasInfo, apiErr := a.resolveInternalTemplate(ctx, identifier, teamID)
	if apiErr == nil {
		return aliasInfo.TemplateID, false, nil
	}

	if apiErr.Code != http.StatusNotFound {
		return "", false, apiErr
	}

	env, err := a.sqlcDB.GetTemplateIncludingSoftDeleted(ctx, identifier)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			// Genuinely unknown reference — keep the original 404
			return "", false, apiErr
		}

		return "", false, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "Error resolving template",
			Err:       err,
		}
	}

	if env.TeamID != teamID {
		return "", false, &api.APIError{
			Code:      http.StatusUnprocessableEntity,
			ClientMsg: fmt.Sprintf("template '%s' does not belong to team '%s'", identifier, teamID),
			Err:       fmt.Errorf("team '%s' does not own template '%s'", teamID, env.ID),
		}
	}

	if env.DeletedAt == nil {
		// The env exists but is not visible to alias resolution — treat as not found
		return "", false, &api.APIError{
			Code:      http.StatusNotFound,
			ClientMsg: fmt.Sprintf("template '%s' not found", identifier),
			Err:       errors.New("template is not resolvable"),
		}
	}

	return env.ID, true, nil
}
