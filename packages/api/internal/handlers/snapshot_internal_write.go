package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/dberrors"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// internalSnapshotLockClass namespaces the advisory locks taken on sandbox IDs
// (two-key form, so they cannot collide with single-key advisory locks).
const internalSnapshotLockClass = 0x534e4150 // "SNAP"

// upsertInternalSnapshot runs UpsertInternalSnapshot under a transaction-scoped
// advisory lock on the sandbox ID. Without it, two calls for a sandbox that has
// no live snapshot yet each create an env, and the one committing later points
// the snapshots row at its own env: the build of the earlier call then hangs
// off an env the row no longer names, and last-snapshot cannot find it. The
// lock is taken in a statement of its own so that the upsert, which reads at
// its statement start, sees what the earlier call committed.
func (a *APIStore) upsertInternalSnapshot(ctx context.Context, params queries.UpsertInternalSnapshotParams) (queries.UpsertInternalSnapshotRow, error) {
	txClient, tx, err := a.sqlcDB.WithTx(ctx)
	if err != nil {
		return queries.UpsertInternalSnapshotRow{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2))", internalSnapshotLockClass, params.SandboxID); err != nil {
		return queries.UpsertInternalSnapshotRow{}, fmt.Errorf("lock sandbox %s: %w", params.SandboxID, err)
	}

	result, err := txClient.UpsertInternalSnapshot(ctx, params)
	if err != nil {
		return queries.UpsertInternalSnapshotRow{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return queries.UpsertInternalSnapshotRow{}, fmt.Errorf("commit: %w", err)
	}

	return result, nil
}

// PostInternalSandboxesSandboxIDPauseSnapshot writes the metadata for a Pause operation:
// it allocates an internal snapshot template and a build with status=snapshotting.
// The operation_id is only logged — idempotency is provided by the UpsertInternalSnapshot CTE,
// serialized per sandbox by upsertInternalSnapshot.
func (a *APIStore) PostInternalSandboxesSandboxIDPauseSnapshot(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	body, err := ginutils.ParseBody[api.PostInternalSandboxesSandboxIDPauseSnapshotJSONRequestBody](ctx, c)
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

	result, err := a.upsertInternalSnapshot(ctx, params)
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

	body, err := ginutils.ParseBody[api.PostInternalSandboxesSandboxIDSnapshotTemplatesJSONRequestBody](ctx, c)
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

	upsertResult, err := a.upsertInternalSnapshot(ctx, params)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error upserting snapshot for checkpoint", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error writing checkpoint snapshot metadata")

		return
	}

	envID, err := a.sqlcDB.CreateSnapshotTemplateEnv(ctx, queries.CreateSnapshotTemplateEnvParams{
		SnapshotID: id.Generate(),
		TeamID:     body.TeamId,
		// Same cluster as the snapshot env written above (local)
		ClusterID:    nil,
		SandboxID:    sandboxID,
		OriginNodeID: &body.SourceNodeName,
		BuildID:      &upsertResult.BuildID,
		Tag:          id.DefaultTag,
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

	body, err := ginutils.ParseBody[api.PostInternalBuildsBuildIDStatusJSONRequestBody](ctx, c)
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
