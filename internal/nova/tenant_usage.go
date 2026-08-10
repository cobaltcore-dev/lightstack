package nova

import (
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// ListTenantUsage handles GET /v2.1/os-simple-tenant-usage — the aggregate form.
//
// Administrator-only (SPEC-023 FR-008b). It reports EVERY project's consumption
// broken down by project, so before this check any authenticated tenant could
// enumerate how large every other tenant on the deployment was. Usage is derived
// by grouping instances and has no owner row of its own, which is how it came to
// have no ownership check at all.
func (svc *Service) ListTenantUsage(c *gin.Context) {
	if !requireAdmin(c, "os-simple-tenant-usage:index") {
		return
	}

	// Query all projects with their instance counts and uptime
	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT
			i.project_id,
			COUNT(*) as total_instances,
			COALESCE(SUM(EXTRACT(EPOCH FROM (NOW() - i.created_at)) / 3600), 0) as total_hours,
			COALESCE(SUM(f.vcpus), 0) as total_vcpus_usage,
			COALESCE(SUM(f.ram_mb), 0) as total_memory_mb_usage,
			COALESCE(SUM(f.disk_gb), 0) as total_local_gb_usage
		 FROM instances i
		 LEFT JOIN flavors f ON i.flavor_id = f.id
		 GROUP BY i.project_id`,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_tenant_usage").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list tenant usage"))
		return
	}
	defer rows.Close()

	var tenantUsages []gin.H
	for rows.Next() {
		var projectID string
		var totalInstances int
		var totalHours, totalVCPUs, totalMemoryMB, totalLocalGB float64

		if err := rows.Scan(&projectID, &totalInstances, &totalHours, &totalVCPUs, &totalMemoryMB, &totalLocalGB); err != nil {
			log.Warn().Err(err).Str("operation", "ListTenantUsage").Msg("failed to scan row")
			continue
		}

		tenantUsages = append(tenantUsages, gin.H{
			"tenant_id":             projectID,
			"total_local_gb_usage":  totalLocalGB,
			"total_vcpus_usage":     totalVCPUs,
			"total_memory_mb_usage": totalMemoryMB,
			"total_hours":           totalHours,
			"start":                 time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02T15:04:05.000000"),
			"stop":                  time.Now().Format("2006-01-02T15:04:05.000000"),
			"server_usages":         []gin.H{},
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list tenant usage"))
		return
	}

	if tenantUsages == nil {
		tenantUsages = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"tenant_usages": tenantUsages})
}

// GetTenantUsage handles GET /v2.1/os-simple-tenant-usage/:id — one project.
//
// The target is resolved through the shared quota-shaped resolver (SPEC-023
// FR-008c): an administrator may name any project, everyone else is pinned to
// their own regardless of what they pass. Before this the `:id` path parameter
// went STRAIGHT into the query, so any tenant could read any named project's
// consumption just by knowing its id.
//
// A non-administrator's foreign id is ignored rather than resolved, so the
// endpoint cannot be used to probe which projects exist.
func (svc *Service) GetTenantUsage(c *gin.Context) {
	tenantID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		// The caller holds no project scope, an administrator named a project that
		// does not exist, or the lookup failed. Reporting zeroes here — which is
		// what this did before — is indistinguishable from a real project owning no
		// instances, and masks a malformed identifier as a success (FR-008d).
		if apiErr.StatusCode == http.StatusForbidden {
			// A non-administrator named some project other than their own. The
			// shared resolver words its refusal for the quota endpoints; this one
			// names the usage policy, as the aggregate form's refusal does.
			apiErr = common.NewForbiddenError(
				"Policy doesn't allow os-simple-tenant-usage:show to be performed. Admin role required.")
		}
		common.SendError(c, apiErr)
		return
	}

	// Query specific tenant usage
	var totalInstances int
	var totalHours, totalVCPUs, totalMemoryMB, totalLocalGB float64

	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT
			COUNT(*) as total_instances,
			COALESCE(SUM(EXTRACT(EPOCH FROM (NOW() - i.created_at)) / 3600), 0) as total_hours,
			COALESCE(SUM(f.vcpus), 0) as total_vcpus_usage,
			COALESCE(SUM(f.ram_mb), 0) as total_memory_mb_usage,
			COALESCE(SUM(f.disk_gb), 0) as total_local_gb_usage
		 FROM instances i
		 LEFT JOIN flavors f ON i.flavor_id = f.id
		 WHERE i.project_id = $1
		 GROUP BY i.project_id`,
		tenantID,
	).Scan(&totalInstances, &totalHours, &totalVCPUs, &totalMemoryMB, &totalLocalGB)

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// A real error is reported, not flattened into zeroes. Only the no-rows
		// case is legitimately zero: a project that exists and owns nothing
		// (FR-008d).
		log.Error().Err(err).Str("operation", "get_tenant_usage").Str("project", tenantID).
			Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read tenant usage"))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		totalInstances = 0
		totalHours = 0
		totalVCPUs = 0
		totalMemoryMB = 0
		totalLocalGB = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"tenant_usage": gin.H{
			"tenant_id":             tenantID,
			"total_local_gb_usage":  totalLocalGB,
			"total_vcpus_usage":     totalVCPUs,
			"total_memory_mb_usage": totalMemoryMB,
			"total_hours":           totalHours,
			"start":                 time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02T15:04:05.000000"),
			"stop":                  time.Now().Format("2006-01-02T15:04:05.000000"),
			"server_usages":         []gin.H{},
		},
	})
}
