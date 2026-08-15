package handlers

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	sharedUtils "github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// GetInternalSandboxesSandboxIDLastSnapshot returns the resume metadata of the sandbox's
// last paused snapshot (ready build only).
func (a *APIStore) GetInternalSandboxesSandboxIDLastSnapshot(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	row, err := a.sqlcDB.GetLastSnapshot(ctx, sandboxID)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			a.sendAPIStoreError(c, http.StatusNotFound, fmt.Sprintf("No paused snapshot found for sandbox '%s'", sandboxID))

			return
		}

		telemetry.ReportCriticalError(ctx, "error getting last snapshot", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error getting last snapshot")

		return
	}

	c.JSON(http.StatusOK, resumeMetadataFromLastSnapshot(row))
}

// GetInternalSandboxesSandboxIDSnapshotState returns whether the sandbox currently has a
// live (not soft-deleted) paused snapshot.
func (a *APIStore) GetInternalSandboxesSandboxIDSnapshotState(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	row, err := a.sqlcDB.GetSnapshotBySandboxID(ctx, sandboxID)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			c.JSON(http.StatusOK, api.SnapshotStateResponse{Paused: false})

			return
		}

		telemetry.ReportCriticalError(ctx, "error getting snapshot state", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error getting snapshot state")

		return
	}

	c.JSON(http.StatusOK, api.SnapshotStateResponse{
		Paused:     true,
		TemplateId: &row.EnvID,
		BuildId:    row.BuildID,
	})
}

// GetInternalSnapshotsSnapshotIDResolve resolves a user snapshot reference (template_id:tag
// or namespace/alias:tag) to its template and ready build and returns the resume metadata.
// Checkpoint templates have no recorded base env, so base_template_id/aliases and the
// sandbox-level metadata fields are empty.
func (a *APIStore) GetInternalSnapshotsSnapshotIDResolve(c *gin.Context, snapshotID api.SnapshotID, params api.GetInternalSnapshotsSnapshotIDResolveParams) {
	ctx := c.Request.Context()

	identifier, tag, err := id.ParseName(snapshotID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid snapshot ID: %s", err))

		return
	}

	tagName := id.DefaultTag
	if tag != nil {
		tagName = *tag
	}

	aliasInfo, apiErr := a.resolveInternalTemplate(ctx, identifier, params.TeamId)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		if apiErr.Code != http.StatusNotFound {
			telemetry.ReportError(ctx, "error resolving snapshot template", apiErr.Err)
		}

		return
	}

	row, err := a.sqlcDB.GetSnapshotTemplateWithReadyBuild(ctx, queries.GetSnapshotTemplateWithReadyBuildParams{
		Tag:        tagName,
		TemplateID: aliasInfo.TemplateID,
	})
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			a.sendAPIStoreError(c, http.StatusNotFound, fmt.Sprintf("No ready build found for snapshot '%s'", snapshotID))

			return
		}

		telemetry.ReportCriticalError(ctx, "error getting snapshot template with ready build", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error resolving snapshot")

		return
	}

	envdVersion := sharedUtils.FromPtr(row.EnvBuild.EnvdVersion)

	result := api.ResumeMetadata{
		TemplateId:         aliasInfo.TemplateID,
		BuildId:            row.EnvBuild.ID,
		OriginNode:         sharedUtils.FromPtr(row.EnvBuild.ClusterNodeID),
		Vcpu:               row.EnvBuild.Vcpu,
		RamMb:              row.EnvBuild.RamMb,
		TotalDiskSizeMb:    row.EnvBuild.TotalDiskSizeMb,
		KernelVersion:      row.EnvBuild.KernelVersion,
		FirecrackerVersion: row.EnvBuild.FirecrackerVersion,
		EnvdVersion:        &envdVersion,
		// Checkpoint templates record no env_secure; default to false
		EnvSecure: false,
		// snapshot_templates.created_at is the checkpoint creation time
		SandboxStartedAt: row.CreatedAt.Time,
		MachineInfo:      machineInfoMap(row.EnvBuild.CpuArchitecture, row.EnvBuild.CpuFamily, row.EnvBuild.CpuModel, row.EnvBuild.CpuModelName, row.EnvBuild.CpuFlags),
	}

	c.JSON(http.StatusOK, result)
}
