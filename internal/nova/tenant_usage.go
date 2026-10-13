package nova

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
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

	rows, err := svc.pool.Query(c.Request.Context(), tenantUsageQuery+` ORDER BY i.project_id`)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_tenant_usage").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list tenant usage"))
		return
	}
	totals, order, err := sumUsage(rows)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_tenant_usage").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list tenant usage"))
		return
	}

	var tenantUsages []gin.H
	for _, projectID := range order {
		tenantUsages = append(tenantUsages, totals[projectID].entry(projectID))
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

	rows, err := svc.pool.Query(c.Request.Context(), tenantUsageQuery+` WHERE i.project_id = $1`, tenantID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_tenant_usage").Str("project", tenantID).
			Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read tenant usage"))
		return
	}
	totals, _, err := sumUsage(rows)
	if err != nil {
		// A real error is reported, not flattened into zeroes. Only a project that
		// exists and owns nothing is legitimately zero (FR-008d).
		log.Error().Err(err).Str("operation", "get_tenant_usage").Str("project", tenantID).
			Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read tenant usage"))
		return
	}
	u := totals[tenantID]
	if u == nil {
		u = &usageTotals{}
	}
	c.JSON(http.StatusOK, gin.H{"tenant_usage": u.entry(tenantID)})
}

// entry renders one project's usage as the gin.H both os-simple-tenant-usage forms return.
func (u *usageTotals) entry(projectID string) gin.H {
	now := time.Now()
	return gin.H{
		"tenant_id":             projectID,
		"total_local_gb_usage":  u.localGB,
		"total_vcpus_usage":     u.vcpus,
		"total_memory_mb_usage": u.memoryMB,
		"total_hours":           u.hours,
		"start":                 now.Add(-30 * 24 * time.Hour).Format("2006-01-02T15:04:05.000000"),
		"stop":                  now.Format("2006-01-02T15:04:05.000000"),
		"server_usages":         []gin.H{},
	}
}

// tenantUsageQuery returns one row per instance: its project, its creation time and its
// flavor's size, NULL when it has none.
const tenantUsageQuery = `SELECT i.project_id, i.created_at, f.vcpus, f.ram_mb, f.disk_gb
	 FROM instances i
	 LEFT JOIN flavors f ON i.flavor_id = f.id`

// usageTotals is one project's consumption.
type usageTotals struct {
	hours, vcpus, memoryMB, localGB float64
}

// sumUsage adds up tenantUsageQuery's rows per project, in the order the projects first appear.
// Hours are counted here, against one clock reading, rather than by date arithmetic in
// SQL, which the two backends spell differently.
func sumUsage(rows database.Rows) (map[string]*usageTotals, []string, error) {
	defer rows.Close()
	now := time.Now()
	totals := map[string]*usageTotals{}
	var order []string
	for rows.Next() {
		var projectID string
		var created *time.Time
		var vcpus, ramMB, diskGB *int
		if err := rows.Scan(&projectID, &created, &vcpus, &ramMB, &diskGB); err != nil {
			return nil, nil, err
		}
		u := totals[projectID]
		if u == nil {
			u = &usageTotals{}
			totals[projectID] = u
			order = append(order, projectID)
		}
		u.add(now, created, vcpus, ramMB, diskGB)
	}
	return totals, order, rows.Err()
}

// add folds one instance's figures into the total; a NULL flavor column (no flavor) adds nothing.
func (u *usageTotals) add(now time.Time, created *time.Time, vcpus, ramMB, diskGB *int) {
	if created != nil {
		u.hours += now.Sub(*created).Hours()
	}
	if vcpus != nil {
		u.vcpus += float64(*vcpus)
	}
	if ramMB != nil {
		u.memoryMB += float64(*ramMB)
	}
	if diskGB != nil {
		u.localGB += float64(*diskGB)
	}
}
