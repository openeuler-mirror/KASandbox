package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// softDeleteTemplateAndInvalidate soft-deletes a template and invalidates its tag/alias
// cache entries using the alias keys captured before the aliases were deleted.
func (a *APIStore) softDeleteTemplateAndInvalidate(ctx context.Context, templateID string) error {
	aliasKeys, err := a.sqlcDB.SoftDeleteTemplate(ctx, templateID)
	if err != nil {
		return err
	}

	a.templateCache.InvalidateAllTags(context.WithoutCancel(ctx), templateID)
	a.templateCache.InvalidateAliasesByTemplateID(context.WithoutCancel(ctx), templateID, aliasKeys)

	return nil
}

// DeleteInternalSandboxesSandboxIDSnapshot soft-deletes the sandbox's paused snapshot
// together with its internal restore template (envs row).
func (a *APIStore) DeleteInternalSandboxesSandboxIDSnapshot(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	envID, err := a.sqlcDB.SoftDeleteSnapshotBySandboxID(ctx, sandboxID)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			a.sendAPIStoreError(c, http.StatusNotFound, fmt.Sprintf("No paused snapshot found for sandbox '%s'", sandboxID))

			return
		}

		telemetry.ReportCriticalError(ctx, "error soft-deleting snapshot", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error deleting snapshot")

		return
	}

	err = a.softDeleteTemplateAndInvalidate(ctx, envID)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error soft-deleting snapshot restore template", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error deleting snapshot template")

		return
	}

	logger.L().Info(ctx, "Deleted paused snapshot metadata", logger.WithSandboxID(sandboxID), logger.WithTemplateID(envID))

	c.Status(http.StatusNoContent)
}

// DeleteInternalSnapshotTemplatesSnapshotID resolves a user snapshot reference and
// soft-deletes the checkpoint snapshot template. Repeat deletes return 204.
func (a *APIStore) DeleteInternalSnapshotTemplatesSnapshotID(c *gin.Context, snapshotID api.SnapshotID, params api.DeleteInternalSnapshotTemplatesSnapshotIDParams) {
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

	templateID, alreadyDeleted, apiErr := a.resolveInternalTemplateForDelete(ctx, identifier, params.TeamId)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		if apiErr.Code != http.StatusNotFound {
			telemetry.ReportError(ctx, "error resolving snapshot template", apiErr.Err)
		}

		return
	}

	if alreadyDeleted {
		// Repeat delete of a soft-deleted template is a success
		c.Status(http.StatusNoContent)

		return
	}

	// Check if there are paused sandboxes using this snapshot template
	hasSnapshots, err := a.sqlcDB.ExistsTemplateSnapshots(ctx, templateID)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error when checking if template has snapshots", err, telemetry.WithTemplateID(templateID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when checking if template has snapshots")

		return
	}

	if hasSnapshots {
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("cannot delete snapshot '%s' because there are paused sandboxes using it", snapshotID))

		return
	}

	// Check that the latest build for the tag is not an in-progress checkpoint
	build, err := a.sqlcDB.GetLatestTemplateBuildStatusByTag(ctx, queries.GetLatestTemplateBuildStatusByTagParams{
		EnvID: templateID,
		Tag:   tagName,
	})
	if err != nil && !dberrors.IsNotFoundError(err) {
		telemetry.ReportCriticalError(ctx, "error when checking template build status", err, telemetry.WithTemplateID(templateID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when checking template build status")

		return
	}

	if err == nil && build.Status == types.BuildStatusSnapshotting {
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("cannot delete snapshot '%s' because a checkpoint build is in progress", snapshotID))

		return
	}

	err = a.softDeleteTemplateAndInvalidate(ctx, templateID)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error soft-deleting snapshot template", err, telemetry.WithTemplateID(templateID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error deleting snapshot template")

		return
	}

	logger.L().Info(ctx, "Deleted snapshot template metadata", logger.WithTemplateID(templateID), logger.WithTeamID(params.TeamId.String()))

	c.Status(http.StatusNoContent)
}

// DeleteInternalTemplatesTemplateID soft-deletes a base template for the given team.
// Unlike the SDK-facing delete, it does not check running sandboxes — the webhook service
// does that beforehand via its running-instance view. Repeat deletes return 204.
func (a *APIStore) DeleteInternalTemplatesTemplateID(c *gin.Context, templateID api.TemplateID, params api.DeleteInternalTemplatesTemplateIDParams) {
	ctx := c.Request.Context()

	identifier, _, err := id.ParseName(templateID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid template ID: %s", err))

		return
	}

	resolvedTemplateID, alreadyDeleted, apiErr := a.resolveInternalTemplateForDelete(ctx, identifier, params.TeamId)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		if apiErr.Code != http.StatusNotFound {
			telemetry.ReportError(ctx, "error resolving template", apiErr.Err)
		}

		return
	}

	if alreadyDeleted {
		c.Status(http.StatusNoContent)

		return
	}

	// Check if base template has snapshots
	hasSnapshots, err := a.sqlcDB.ExistsTemplateSnapshots(ctx, resolvedTemplateID)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error when checking if base template has snapshots", err, telemetry.WithTemplateID(resolvedTemplateID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when checking if template has snapshots")

		return
	}

	if hasSnapshots {
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("cannot delete template '%s' because there are paused sandboxes using it", resolvedTemplateID))

		return
	}

	err = a.softDeleteTemplateAndInvalidate(ctx, resolvedTemplateID)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error soft-deleting template", err, telemetry.WithTemplateID(resolvedTemplateID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when deleting template")

		return
	}

	logger.L().Info(ctx, "Deleted template metadata (internal)", logger.WithTemplateID(resolvedTemplateID), logger.WithTeamID(params.TeamId.String()))

	c.Status(http.StatusNoContent)
}
