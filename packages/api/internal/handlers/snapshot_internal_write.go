package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// PostInternalSandboxesSandboxIDPauseSnapshot writes the metadata for a Pause operation:
// it allocates an internal snapshot template and a build with status=snapshotting.
// The operation_id is only logged — idempotency is provided by the UpsertSnapshot CTE.
func (a *APIStore) PostInternalSandboxesSandboxIDPauseSnapshot(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	body, err := utils.ParseBody[api.PostInternalSandboxesSandboxIDPauseSnapshotJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	logger.L().Info(ctx, "Writing pause snapshot metadata",
		logger.WithSandboxID(sandboxID),
		logger.WithTeamID(body.TeamId.String()),
		zap.String("operationID", body.OperationId),
	)

	params, err := runtimeMetadataToUpsertParams(sandboxID, body)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid snapshot metadata: %s", err))

		return
	}

	result, err := a.sqlcDB.UpsertSnapshot(ctx, params)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error upserting pause snapshot metadata", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error writing pause snapshot metadata")

		return
	}

	c.JSON(http.StatusOK, api.PauseSnapshotResponse{
		TemplateId: result.TemplateID,
		BuildId:    result.BuildID,
	})
}

// PostInternalSandboxesSandboxIDSnapshotTemplates writes the metadata for a user Checkpoint:
// it upserts the runtime snapshot and creates a persistent snapshot template pointing to the
// new build. This internal endpoint does not support user-provided names/aliases.
func (a *APIStore) PostInternalSandboxesSandboxIDSnapshotTemplates(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	body, err := utils.ParseBody[api.PostInternalSandboxesSandboxIDSnapshotTemplatesJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	logger.L().Info(ctx, "Writing checkpoint snapshot template metadata",
		logger.WithSandboxID(sandboxID),
		logger.WithTeamID(body.TeamId.String()),
		zap.String("operationID", body.OperationId),
	)

	params, err := runtimeMetadataToUpsertParams(sandboxID, body)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid snapshot metadata: %s", err))

		return
	}

	upsertResult, err := a.sqlcDB.UpsertSnapshot(ctx, params)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error upserting snapshot for checkpoint", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error writing checkpoint snapshot metadata")

		return
	}

	envID, err := a.sqlcDB.CreateSnapshotTemplateEnv(ctx, queries.CreateSnapshotTemplateEnvParams{
		SnapshotID: id.Generate(),
		TeamID:     body.TeamId,
		SandboxID:  sandboxID,
		BuildID:    upsertResult.BuildID,
		Tag:        id.DefaultTag,
	})
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error creating snapshot template env", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error creating snapshot template")

		return
	}

	c.JSON(http.StatusOK, api.SnapshotTemplateResponse{
		SnapshotId: id.WithTag(envID, id.DefaultTag),
		TemplateId: envID,
		BuildId:    upsertResult.BuildID,
	})
}

// PostInternalBuildsBuildIDStatus writes back the terminal status of a snapshot build.
// Repeating the same terminal value is an idempotent success; a conflicting terminal value
// returns 409.
func (a *APIStore) PostInternalBuildsBuildIDStatus(c *gin.Context, buildID api.BuildID) {
	ctx := c.Request.Context()

	body, err := utils.ParseBody[api.PostInternalBuildsBuildIDStatusJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	logger.L().Info(ctx, "Writing back build status",
		zap.String("buildID", buildID),
		zap.String("operationID", body.OperationId),
		zap.String("status", string(body.Status)),
	)

	buildUUID, err := uuid.Parse(buildID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid build ID: %s", err))

		return
	}

	targetStatus := types.BuildStatusSuccess
	if body.Status == api.Failed {
		targetStatus = types.BuildStatusFailed
	}

	current, err := a.sqlcDB.GetEnvBuildStatusByID(ctx, buildUUID)
	if err != nil {
		if dberrors.IsNotFoundError(err) {
			a.sendAPIStoreError(c, http.StatusNotFound, fmt.Sprintf("Build '%s' not found", buildID))

			return
		}

		telemetry.ReportCriticalError(ctx, "error getting build status", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error getting build status")

		return
	}

	if current.Status == targetStatus {
		// Idempotent repeat of the same terminal value
		c.Status(http.StatusOK)

		return
	}

	if current.Status == types.BuildStatusSuccess || current.Status == types.BuildStatusFailed {
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("Build '%s' is already in terminal state '%s'", buildID, current.Status))

		return
	}

	reason := types.BuildReason{}
	if body.Error != nil {
		reason.Message = *body.Error
	}

	now := time.Now()
	err = a.sqlcDB.UpdateEnvBuildStatus(ctx, queries.UpdateEnvBuildStatusParams{
		Status:     targetStatus,
		FinishedAt: &now,
		Reason:     reason,
		BuildID:    buildUUID,
	})
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error updating build status", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error updating build status")

		return
	}

	c.Status(http.StatusOK)
}
