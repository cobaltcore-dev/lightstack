package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// cinderQuotaDefaults holds the default block-storage limits used when a project has
// no row in cinder_quotas.
//
// This is the SINGLE source of default limits for the package: the quota-set views and
// the absolute-limits view both resolve through resolveCinderQuotaLimits, so they
// cannot report different values for the same resource (SPEC-020 FR-003). Previously
// the limits handler hardcoded its own values (maxTotalVolumes 1000 against a
// quota-set default of 100) and ignored cinder_quotas entirely, so an admin's quota
// override had no effect on the limits view at all.
var cinderQuotaDefaults = map[string]int{
	"volumes":              100,
	"snapshots":            100,
	"gigabytes":            10000,
	"backups":              100,
	"backup_gigabytes":     10000,
	"per_volume_gigabytes": 1000,
}

// resolveCinderQuotaLimits returns the effective limits for a project: the package
// defaults overlaid with whatever cinder_quotas configures.
func resolveCinderQuotaLimits(ctx context.Context, projectID string) (map[string]int, error) {
	limits := make(map[string]int, len(cinderQuotaDefaults))
	maps.Copy(limits, cinderQuotaDefaults)

	rows, err := database.DB.Query(ctx, `
		SELECT resource, "limit"
		FROM cinder_quotas
		WHERE project_id = $1
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query cinder quotas for project %s: %w", projectID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var resource string
		var limit int
		if err := rows.Scan(&resource, &limit); err != nil {
			return nil, fmt.Errorf("scan cinder quota row for project %s: %w", projectID, err)
		}
		limits[resource] = limit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cinder quotas for project %s: %w", projectID, err)
	}
	return limits, nil
}

// cinderQuotaUsage counts the project's block-storage consumption, keyed by the same
// resource names as cinderQuotaDefaults so the two can be zipped. A failed count is
// returned, never reported as zero usage.
func cinderQuotaUsage(ctx context.Context, projectID string) (map[string]int, error) {
	var volumes, gigabytes, snapshots, backups, backupGigabytes int
	if err := database.DB.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volumes WHERE project_id = $1 AND status != 'deleted'",
		projectID).Scan(&volumes, &gigabytes); err != nil {
		return nil, fmt.Errorf("count volume usage: %w", err)
	}
	if err := database.DB.QueryRow(ctx,
		"SELECT COUNT(*) FROM snapshots WHERE project_id = $1 AND status != 'deleted'",
		projectID).Scan(&snapshots); err != nil {
		return nil, fmt.Errorf("count snapshot usage: %w", err)
	}
	// volume_backups (migration 022) carries no 'deleted' status — DeleteBackup removes
	// the row — so this counts every row for the project.
	if err := database.DB.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volume_backups WHERE project_id = $1",
		projectID).Scan(&backups, &backupGigabytes); err != nil {
		return nil, fmt.Errorf("count backup usage: %w", err)
	}
	return map[string]int{
		"volumes":          volumes,
		"gigabytes":        gigabytes,
		"snapshots":        snapshots,
		"backups":          backups,
		"backup_gigabytes": backupGigabytes,
	}, nil
}

// GetQuotaSetDefaults returns the default quota limits (code defaults only — no
// cinder_quotas overlay and no usage), from the same source as every other quota
// view so the values cannot drift (FR-003).
func (svc *Service) GetQuotaSetDefaults(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	quotaSet := gin.H{"id": projectID}
	for resource, limit := range cinderQuotaDefaults {
		quotaSet[resource] = limit
	}
	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// GetQuotaSet returns quota limits for a project, flat by default.
//
// With `?usage=true` each resource becomes {in_use, limit, reserved} instead — the
// Block Storage v3 spelling for the usage view ("Show project's quota usage
// information. Default is false"). Note this is NOT Nova's `/detail` sub-resource:
// block storage has no such path, so `cinder quota-show --usage` and
// `cinderclient.quotas.get(project, usage=True)` both arrive here with a query
// parameter (SPEC-020 FR-002). `reserved` is always 0 — Lightstack completes operations
// synchronously and models no reservations (Art. II).
func (svc *Service) GetQuotaSet(c *gin.Context) {
	targetProjectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// Resolve limits through the shared helper so this view and the absolute-limits
	// view always agree (FR-003).
	quotas, err := resolveCinderQuotaLimits(c.Request.Context(), targetProjectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_quota_set").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get quota set"))
		return
	}

	quotaSet := gin.H{"id": targetProjectID}
	// ParseBool accepts the "True" that python-cinderclient sends as well as "true"
	// and "1"; anything unparseable is the documented default, false.
	if withUsage, _ := strconv.ParseBool(c.Query("usage")); withUsage {
		usage, usageErr := cinderQuotaUsage(c.Request.Context(), targetProjectID)
		if usageErr != nil {
			log.Error().Err(usageErr).Str("project_id", targetProjectID).Msg("failed to query quota usage")
			common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
			return
		}
		for resource, limit := range quotas {
			quotaSet[resource] = gin.H{
				"in_use":   usage[resource],
				"limit":    limit,
				"reserved": 0,
			}
		}
	} else {
		for resource, limit := range quotas {
			quotaSet[resource] = limit
		}
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// UpdateQuotaSet updates quota limits for a project
func (svc *Service) UpdateQuotaSet(c *gin.Context) {
	// Admin-only: quota limits are an operator concern. Checked before the body is
	// bound so a non-admin gets 403, not a request-validation 400 (SPEC-020 FR-020).
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	// Resolved exactly as the reads resolve it, so `default` names the same project on
	// PUT as on GET — the caller's own.
	resolved, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}
	var req struct {
		QuotaSet map[string]any `json:"quota_set"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Update each quota resource
	for resource, limitVal := range req.QuotaSet {
		// Skip non-integer fields like "id"
		if resource == "id" {
			continue
		}

		var limit int
		switch v := limitVal.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		case json.Number:
			// Handle json.Number from any unmarshaling
			val, err := v.Int64()
			if err != nil {
				continue
			}
			limit = int(val)
		default:
			continue
		}

		_, err := database.DB.Exec(c.Request.Context(), `
			INSERT INTO cinder_quotas (project_id, resource, "limit")
			VALUES ($1, $2, $3)
			ON CONFLICT (project_id, resource)
			DO UPDATE SET "limit" = $3, updated_at = NOW()
		`, resolved, resource, limit)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_quota").Str("resource", resource).Msg("failed to update quota")
			common.SendError(c, common.NewInternalServerError("failed to update quota"))
			return
		}
	}

	// Return the resulting quotas (defaults + overrides) from the shared helper.
	limits, err := resolveCinderQuotaLimits(c.Request.Context(), resolved)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_quota").Msg("failed to read back quotas")
		common.SendError(c, common.NewInternalServerError("failed to read back quotas"))
		return
	}

	quotaSet := make(map[string]any)
	quotaSet["id"] = resolved
	for k, v := range limits {
		quotaSet[k] = v
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// DeleteQuotaSet resets quotas to defaults for a project
func (svc *Service) DeleteQuotaSet(c *gin.Context) {
	// Admin-only, matching UpdateQuotaSet and upstream Cinder (SPEC-020 FR-020).
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	// Same target resolution as the reads and UpdateQuotaSet.
	targetProjectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// A DELETE matching no rows is not an error: resetting a project that never had an
	// override is a successful no-op, and Exec reports that as a zero row count, never
	// as pgx.ErrNoRows (that sentinel only comes from QueryRow/Scan).
	if _, err := database.DB.Exec(c.Request.Context(),
		"DELETE FROM cinder_quotas WHERE project_id = $1",
		targetProjectID); err != nil {
		log.Error().Err(err).Str("operation", "delete_quota_set").Msg("failed to reset quotas")
		common.SendError(c, common.NewInternalServerError("failed to reset quotas"))
		return
	}

	c.Status(http.StatusNoContent)
}
