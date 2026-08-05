package nova

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// UpdateQuotaRequest represents a quota update request
type UpdateQuotaRequest struct {
	QuotaSet map[string]int `json:"quota_set"`
}

// GetQuotaSet returns quota limits and usage for a project
func (svc *Service) GetQuotaSet(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// Resolve limits through the shared helper so this view and the absolute-limits
	// view always agree (FR-003).
	limits, stored, err := resolveQuotaLimits(c.Request.Context(), projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_quota_set").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get quota set"))
		return
	}

	quotaSet := gin.H{
		"id": projectID,
	}
	// The declared quota-set resources. The limits-only ones (keypairs, server groups)
	// are reported by the absolute-limits view instead, so they are not defaulted here.
	for resource := range quotaDefaults {
		quotaSet[resource] = limits[resource]
	}
	// Plus every resource actually stored for this project, declared or not.
	// UpdateQuotaSet persists whatever key it is handed and then returns THIS response,
	// so omitting a stored resource makes a successful write invisible to the client
	// that made it: PUT {"quota_set":{"keypairs":5}} is honoured (GetLimits reports
	// maxTotalKeypairs 5) but would be absent from the 200 that confirms it. The
	// pre-existing handler echoed every stored row; keeping that is FR-016.
	for resource := range stored {
		quotaSet[resource] = limits[resource]
	}

	// Calculate usage. A count that fails is an error (500), never zero usage: reporting 0
	// for a count that could not be made tells an operator the project consumes nothing.
	var instanceCount, coreCount, ramCount int
	if err := database.DB.QueryRow(c.Request.Context(), `
		SELECT COUNT(*), COALESCE(SUM(f.vcpus), 0), COALESCE(SUM(f.ram_mb), 0)
		FROM instances i
		LEFT JOIN flavors f ON i.flavor_id = f.id
		WHERE i.project_id = $1
	`, projectID).Scan(&instanceCount, &coreCount, &ramCount); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to calculate quota usage")
		common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
		return
	}

	// Add usage information (OpenStack includes these as separate keys)
	quotaSet["instances_used"] = instanceCount
	quotaSet["cores_used"] = coreCount
	quotaSet["ram_used"] = ramCount

	// Volume usage, under the same rule.
	var volumeCount, gigabyteCount int
	if err := database.DB.QueryRow(c.Request.Context(), `
		SELECT COUNT(*), COALESCE(SUM(size_gb), 0)
		FROM volumes
		WHERE project_id = $1
	`, projectID).Scan(&volumeCount, &gigabyteCount); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query volume usage")
		common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
		return
	}

	quotaSet["volumes_used"] = volumeCount
	quotaSet["gigabytes_used"] = gigabyteCount

	// Network resource usage, under the same rule.
	var networkCount, subnetCount, portCount, routerCount, floatingipCount, sgCount, snapshotCount, sgRuleCount int
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
		{"snapshots", `SELECT COUNT(*) FROM snapshots WHERE project_id = $1`, &snapshotCount},
		{"security_group_rules", `SELECT COUNT(*) FROM security_group_rules WHERE security_group_id IN (SELECT id FROM security_groups WHERE project_id = $1)`, &sgRuleCount},
	}
	for _, q := range networkUsageQueries {
		if err := database.DB.QueryRow(c.Request.Context(), q.sql, projectID).Scan(q.dst); err != nil {
			log.Error().Err(err).Str("resource", q.label).Str("project_id", projectID).Msg("failed to query network resource usage")
			common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
			return
		}
	}

	quotaSet["networks_used"] = networkCount
	quotaSet["subnets_used"] = subnetCount
	quotaSet["ports_used"] = portCount
	quotaSet["routers_used"] = routerCount
	quotaSet["floatingip_used"] = floatingipCount
	quotaSet["security_groups_used"] = sgCount
	// Both resources appear in the quota-set limits, so they need usage too (FR-004).
	quotaSet["snapshots_used"] = snapshotCount
	quotaSet["security_group_rules_used"] = sgRuleCount

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// UpdateQuotaSet updates quota limits for a project (admin only)
func (svc *Service) UpdateQuotaSet(c *gin.Context) {
	// Admin-only operation. Authorization is checked BEFORE the body is bound so a
	// non-admin gets 403 rather than a 400 that leaks request-validation detail.
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	// Resolved exactly as the reads resolve it, so `default` names the same project on
	// PUT as on GET — the caller's own. Any other target must exist: quotas.project_id
	// references projects(id), so an unknown project would surface as an FK violation
	// (500) and a non-UUID as an invalid-uuid-syntax error (500) instead of a 404.
	projectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	var req UpdateQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
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

// GetQuotaSetDetail returns per-resource {in_use, limit, reserved} objects, the
// detail form of the quota set (SPEC-020 FR-002). `reserved` is always 0: Lightstack
// completes operations synchronously and models no reservations (Art. II).
func (svc *Service) GetQuotaSetDetail(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	limits, stored, err := resolveQuotaLimits(c.Request.Context(), projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_quota_set_detail").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get quota set"))
		return
	}

	quotaSet := gin.H{"id": projectID}
	for resource := range quotaDefaults {
		used := 0
		q, qErr := usageQuery(resource)
		if qErr != nil {
			// Unreachable while quotaDefaults and usageQuery agree, which
			// TestUsageQueryCoversEveryQuotaResource enforces. Logged rather than
			// discarded because the failure mode is silent: a resource added to
			// quotaDefaults alone reports in_use: 0 forever, which reads as "no
			// usage" instead of "not measured".
			log.Error().Err(qErr).Str("resource", resource).Msg("no usage query for quota resource")
		} else if scanErr := database.DB.QueryRow(c.Request.Context(), q, projectID).Scan(&used); scanErr != nil {
			log.Error().Err(scanErr).Str("resource", resource).Str("project_id", projectID).
				Msg("failed to query quota usage")
			common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
			return
		}
		quotaSet[resource] = gin.H{
			"in_use":   used,
			"limit":    limits[resource],
			"reserved": 0,
		}
	}

	// The resources Lightstack models as limits-only belong here too: upstream's detail response
	// carries key pairs and server groups, and GetLimits already reports their limits, so
	// omitting them made this the only view that disagreed about which resources exist.
	//
	// `keypairs` and `server_group_members` report in_use 0. **This is exactly what upstream
	// nova does**, not a Lightstack shortcut — nova/quota.py `_get_usages` short-circuits both
	// before any counting:
	//
	//	if resource.name in ('key_pairs', 'server_group_members'):
	//	    # These per user resources are special cases whose usages
	//	    # are not considered when validating limit create/update or
	//	    # displaying used limits. They are always zero.
	//	    usages[resource.name] = {'in_use': 0}
	//
	// (https://opendev.org/openstack/nova/src/branch/master/nova/quota.py#L177-L181, identical on
	// stable/2025.2 and stable/2025.1.) It is unconditional: both get_project_quotas and
	// get_user_quotas call that one `_get_usages`, so `?user_id=` does not route around it —
	// the per-user counter registered for `key_pairs` serves quota *enforcement* on create,
	// not this view.
	//
	// An earlier revision counted `SELECT COUNT(*) FROM keypairs WHERE user_id = $1` with the
	// REQUESTING user's id, which both undercounts a multi-user project and reports an admin's
	// own keypairs as the target project's usage. Raised on PR #79. Counting at project scope is
	// not available either — `keypairs` has no project_id (migration 001) and neither has `users`.
	//
	// `server_groups` is a genuine project resource upstream and is counted per project here.
	//
	// Names are Lightstack's internal ones, matching the rest of this response. Upstream spells
	// several of these differently (`key_pairs`, `floating_ips`); that divergence is
	// endpoint-wide and pre-dates this view, so it is not half-fixed here for one key.
	serverGroups, err := countUsage(c.Request.Context(), "server_groups",
		`SELECT COUNT(*) FROM server_groups WHERE project_id = $1`, projectID)
	if err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query quota usage")
		common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
		return
	}
	for resource, used := range map[string]int{
		"keypairs":             0,
		"server_groups":        serverGroups,
		"server_group_members": 0,
	} {
		quotaSet[resource] = gin.H{
			"in_use":   used,
			"limit":    limits[resource],
			"reserved": 0,
		}
	}

	// Finally anything else the project has a stored row for. The resource set is therefore
	// defaults ∪ limits-only ∪ stored, which is what makes it the same set GetQuotaSet reports
	// (contract C3, "same resource set as C2"): UpdateQuotaSet persists whatever key it is
	// handed, so a stored resource Lightstack does not declare appears in the flat 200 that
	// confirmed the write and would otherwise be missing here (raised on PR #79).
	//
	// No usageQuery call: there is none for a resource outside quotaDefaults, and asking would
	// log an error per request for a condition that is expected rather than wrong. Upstream
	// accepts several resources Lightstack does not model (`metadata_items`, `fixed_ips`,
	// `injected_files`, …) — for those `in_use: 0` is the honest answer, since nothing consumes
	// them here.
	for resource := range stored {
		if _, already := quotaSet[resource]; already {
			continue
		}
		quotaSet[resource] = gin.H{
			"in_use":   0,
			"limit":    limits[resource],
			"reserved": 0,
		}
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// countUsage runs a single-column COUNT. A failure is returned, never reported as zero: a
// usage view that answers 0 when it could not count tells an operator the project consumes
// nothing.
func countUsage(ctx context.Context, resource, query, arg string) (int, error) {
	var n int
	if err := database.DB.QueryRow(ctx, query, arg).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s usage: %w", resource, err)
	}
	return n, nil
}

// GetQuotaSetDefaults returns the default quota limits (code defaults only — no
// quotas-table overlay and no usage), read from the same source as every other
// quota view so the values cannot drift (FR-003).
func (svc *Service) GetQuotaSetDefaults(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c)
	if apiErr != nil && apiErr.StatusCode == http.StatusForbidden {
		// Upstream's os-quota-sets:defaults is RULE_ANY: anyone may read the defaults, which
		// are the same for every project, so a non-admin naming another project is not refused.
		projectID, apiErr = c.GetString("project_id"), nil
	}
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	quotaSet := gin.H{"id": projectID}
	for resource, limit := range quotaDefaults {
		quotaSet[resource] = limit
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// quotaDefaults holds the default hard limits when no quota row exists in the DB.
// These are the resources reported by the quota-set views.
//
// This is the SINGLE source of default compute limits for the package: GetLimits,
// GetQuotaSet, GetQuotaSetDefaults, GetQuotaSetDetail and CheckQuota all resolve
// through resolveQuotaLimits, so the absolute-limits view and the quota-set view
// can never report different values for the same resource (SPEC-020 FR-003).
// Previously GetLimits carried its own map with instances/cores/ram at
// 100/200/512000, ten times these values.
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

// limitsOnlyDefaults are resources the absolute-limits view reports that are not
// part of the quota-set resource list, kept separate so the quota-set response
// shape is unchanged.
var limitsOnlyDefaults = map[string]int{
	"keypairs":             100,
	"server_groups":        10,
	"server_group_members": 10,
}

// resolveQuotaLimits returns the effective hard limits for a project: the package
// defaults overlaid with whatever the quotas table configures. Reading every limits
// and quota-set handler through this is what keeps the two views in agreement and
// makes an admin's quota override visible in both (FR-003).
// It also reports which resources have a stored row, which GetQuotaSet needs to tell a
// project-specific override apart from a code default — the rows are already in hand
// here, so this costs no extra query.
func resolveQuotaLimits(ctx context.Context, projectID string) (map[string]int, map[string]struct{}, error) {
	limits := make(map[string]int, len(quotaDefaults)+len(limitsOnlyDefaults))
	maps.Copy(limits, quotaDefaults)
	maps.Copy(limits, limitsOnlyDefaults)
	stored := map[string]struct{}{}

	rows, err := database.DB.Query(ctx,
		`SELECT resource, hard_limit FROM quotas WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("query quotas for project %s: %w", projectID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var resource string
		var hardLimit int
		if err := rows.Scan(&resource, &hardLimit); err != nil {
			return nil, nil, fmt.Errorf("scan quota row for project %s: %w", projectID, err)
		}
		limits[resource] = hardLimit
		stored[resource] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate quotas for project %s: %w", projectID, err)
	}
	return limits, stored, nil
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
	case "snapshots":
		return `SELECT COUNT(*) FROM snapshots WHERE project_id = $1`, nil
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
