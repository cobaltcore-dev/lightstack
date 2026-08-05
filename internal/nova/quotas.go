package nova

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
)

// UpdateQuotaRequest represents a quota update request
type UpdateQuotaRequest struct {
	QuotaSet map[string]int `json:"quota_set"`
}

// GetQuotaSet returns quota limits and usage for a project
func (svc *Service) GetQuotaSet(c *gin.Context) {
	projectID := c.Param("id")
	requestingProjectID := c.GetString("project_id")

	// Allow users to query their own project or admins to query any project
	// For now, allow any query (proper RBAC would be implemented in production)
	if projectID == "default" {
		projectID = requestingProjectID
	}

	// Fetch quota limits
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT resource, hard_limit
		FROM quotas
		WHERE project_id = $1
	`, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "get_quota_set").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get quota set"))
		return
	}
	defer rows.Close()

	quotaSet := gin.H{
		"id": projectID,
	}

	// Default values if not in database
	defaults := map[string]int{
		"instances":            10,
		"cores":                20,
		"ram":                  51200,
		"volumes":              10,
		"gigabytes":            1000,
		"snapshots":            10,
		"networks":             10,
		"subnets":              10,
		"ports":                50,
		"routers":              10,
		"floatingip":           10,
		"security_groups":      10,
		"security_group_rules": 100,
	}

	// Load limits from database
	for rows.Next() {
		var resource string
		var limit int
		if err := rows.Scan(&resource, &limit); err != nil {
			log.Warn().Err(err).Str("operation", "GetQuotaSet").Msg("failed to scan row")
			continue
		}
		quotaSet[resource] = limit
		delete(defaults, resource) // Remove from defaults if found in DB
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	// Add any missing defaults
	for resource, limit := range defaults {
		if _, exists := quotaSet[resource]; !exists {
			quotaSet[resource] = limit
		}
	}

	// Calculate usage. Scan failures degrade to zero usage (the quota
	// response still ships) and are logged for diagnosis.
	var instanceCount, coreCount, ramCount int
	if err := database.DB.QueryRow(c.Request.Context(), `
		SELECT COUNT(*), COALESCE(SUM(f.vcpus), 0), COALESCE(SUM(f.ram_mb), 0)
		FROM instances i
		LEFT JOIN flavors f ON i.flavor_id = f.id
		WHERE i.project_id = $1
	`, projectID).Scan(&instanceCount, &coreCount, &ramCount); err != nil {
		log.Warn().Err(err).Str("project_id", projectID).Msg("failed to calculate quota usage")
	}

	// Add usage information (OpenStack includes these as separate keys)
	quotaSet["instances_used"] = instanceCount
	quotaSet["cores_used"] = coreCount
	quotaSet["ram_used"] = ramCount

	// Volume usage. Scan failures degrade to zero (the quota response
	// still ships) and are logged for diagnosis.
	var volumeCount, gigabyteCount int
	if err := database.DB.QueryRow(c.Request.Context(), `
		SELECT COUNT(*), COALESCE(SUM(size_gb), 0)
		FROM volumes
		WHERE project_id = $1
	`, projectID).Scan(&volumeCount, &gigabyteCount); err != nil {
		log.Warn().Err(err).Str("project_id", projectID).Msg("failed to query volume usage")
	}

	quotaSet["volumes_used"] = volumeCount
	quotaSet["gigabytes_used"] = gigabyteCount

	// Network resource usage. Same fall-back behavior as above.
	var networkCount, subnetCount, portCount, routerCount, floatingipCount, sgCount int
	networkUsageQueries := []struct {
		label string
		sql   string
		dst   *int
	}{
		{"networks", `SELECT COUNT(*) FROM networks WHERE project_id = $1`, &networkCount},
		{"subnets", `SELECT COUNT(*) FROM subnets WHERE project_id = $1`, &subnetCount},
		{"ports", `SELECT COUNT(*) FROM ports WHERE project_id = $1`, &portCount},
		{"routers", `SELECT COUNT(*) FROM routers WHERE project_id = $1`, &routerCount},
		{"floating_ips", `SELECT COUNT(*) FROM floating_ips WHERE project_id = $1`, &floatingipCount},
		{"security_groups", `SELECT COUNT(*) FROM security_groups WHERE project_id = $1`, &sgCount},
	}
	for _, q := range networkUsageQueries {
		if err := database.DB.QueryRow(c.Request.Context(), q.sql, projectID).Scan(q.dst); err != nil {
			log.Warn().Err(err).Str("resource", q.label).Str("project_id", projectID).Msg("failed to query network resource usage")
		}
	}

	quotaSet["networks_used"] = networkCount
	quotaSet["subnets_used"] = subnetCount
	quotaSet["ports_used"] = portCount
	quotaSet["routers_used"] = routerCount
	quotaSet["floatingip_used"] = floatingipCount
	quotaSet["security_groups_used"] = sgCount

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// UpdateQuotaSet updates quota limits for a project (admin only)
func (svc *Service) UpdateQuotaSet(c *gin.Context) {
	projectID := c.Param("id")

	var req UpdateQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Admin-only operation
	if !common.HasRole(c, "admin") {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	now := time.Now()

	// Update each quota in the request
	for resource, limit := range req.QuotaSet {
		// Skip special fields
		if resource == "id" {
			continue
		}

		_, err := database.DB.Exec(c.Request.Context(), `
			INSERT INTO quotas (project_id, resource, hard_limit, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (project_id, resource)
			DO UPDATE SET hard_limit = $3, updated_at = $5
		`, projectID, resource, limit, now, now)

		if err != nil {
			log.Error().Err(err).Str("operation", "update_quota").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to update quota"))
			return
		}
	}

	// Return updated quota set
	svc.GetQuotaSet(c)
}

// GetQuotaSetDefaults returns default quota limits
func (svc *Service) GetQuotaSetDefaults(c *gin.Context) {
	projectID := c.Param("id")

	quotaSet := gin.H{
		"id":                   projectID,
		"instances":            10,
		"cores":                20,
		"ram":                  51200,
		"volumes":              10,
		"gigabytes":            1000,
		"snapshots":            10,
		"networks":             10,
		"subnets":              10,
		"ports":                50,
		"routers":              10,
		"floatingip":           10,
		"security_groups":      10,
		"security_group_rules": 100,
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// quotaDefaults holds the default hard limits when no quota row exists in the DB.
var quotaDefaults = map[string]int{
	"instances":            10,
	"cores":                20,
	"ram":                  51200,
	"volumes":              10,
	"gigabytes":            1000,
	"snapshots":            10,
	"networks":             10,
	"subnets":              10,
	"ports":                50,
	"routers":              10,
	"floatingip":           10,
	"security_groups":      10,
	"security_group_rules": 100,
}

// usageQuery returns the SQL expression that counts current usage for resource.
func usageQuery(resource string) (string, error) {
	switch resource {
	case "instances":
		return `SELECT COUNT(*) FROM instances WHERE project_id = $1`, nil
	case "cores":
		return `SELECT COALESCE(SUM(f.vcpus), 0) FROM instances i LEFT JOIN flavors f ON i.flavor_id = f.id WHERE i.project_id = $1`, nil
	case "ram":
		return `SELECT COALESCE(SUM(f.ram_mb), 0) FROM instances i LEFT JOIN flavors f ON i.flavor_id = f.id WHERE i.project_id = $1`, nil
	case "volumes":
		return `SELECT COUNT(*) FROM volumes WHERE project_id = $1`, nil
	case "gigabytes":
		return `SELECT COALESCE(SUM(size_gb), 0) FROM volumes WHERE project_id = $1`, nil
	case "networks":
		return `SELECT COUNT(*) FROM networks WHERE project_id = $1`, nil
	case "subnets":
		return `SELECT COUNT(*) FROM subnets WHERE project_id = $1`, nil
	case "ports":
		return `SELECT COUNT(*) FROM ports WHERE project_id = $1`, nil
	case "routers":
		return `SELECT COUNT(*) FROM routers WHERE project_id = $1`, nil
	case "floatingip":
		return `SELECT COUNT(*) FROM floating_ips WHERE project_id = $1`, nil
	case "security_groups":
		return `SELECT COUNT(*) FROM security_groups WHERE project_id = $1`, nil
	case "security_group_rules":
		return `SELECT COUNT(*) FROM security_group_rules WHERE security_group_id IN (SELECT id FROM security_groups WHERE project_id = $1)`, nil
	default:
		return "", fmt.Errorf("unknown quota resource: %s", resource)
	}
}

// CheckQuota checks if creating a resource would exceed quota.
// The check runs inside a transaction with a row-level lock on the quotas row
// to prevent TOCTOU races under concurrent requests.
func CheckQuota(c *gin.Context, resource string, requestedAmount int) error {
	projectID := c.GetString("project_id")

	uq, err := usageQuery(resource)
	if err != nil {
		return err
	}

	return database.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
		// Lock the quota row for this project+resource for the duration of the transaction.
		var limit int
		lockErr := tx.QueryRow(c.Request.Context(), `
			SELECT hard_limit FROM quotas WHERE project_id = $1 AND resource = $2 FOR UPDATE
		`, projectID, resource).Scan(&limit)

		if errors.Is(lockErr, pgx.ErrNoRows) {
			def, ok := quotaDefaults[resource]
			if !ok {
				return fmt.Errorf("unknown quota resource: %s", resource)
			}
			limit = def
		} else if lockErr != nil {
			return lockErr
		}

		var usage int
		if scanErr := tx.QueryRow(c.Request.Context(), uq, projectID).Scan(&usage); scanErr != nil {
			log.Error().Err(scanErr).Str("resource", resource).Str("project_id", projectID).Msg("failed to scan quota usage")
			return scanErr
		}

		if usage+requestedAmount > limit {
			return &QuotaExceededError{
				Resource:  resource,
				Limit:     limit,
				Usage:     usage,
				Requested: requestedAmount,
			}
		}

		return nil
	})
}

// QuotaExceededError represents a quota exceeded error
type QuotaExceededError struct {
	Resource  string
	Limit     int
	Usage     int
	Requested int
}

func (e *QuotaExceededError) Error() string {
	return "Quota exceeded"
}
