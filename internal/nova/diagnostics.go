package nova

import (
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// GetServerDiagnostics handles GET /v2.1/servers/:id/diagnostics
func (svc *Service) GetServerDiagnostics(c *gin.Context) {
	instanceID, projectID, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}

	// Query instance to verify it exists and belongs to project
	var status, flavorID string
	var vcpus, memoryMB, diskGB int
	var createdAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT i.status, i.created_at, f.vcpus, f.ram_mb, f.disk_gb, i.flavor_id
		 FROM instances i
		 LEFT JOIN flavors f ON i.flavor_id = f.id
		 WHERE i.id = $1 AND i.project_id = $2`,
		instanceID, projectID,
	).Scan(&status, &createdAt, &vcpus, &memoryMB, &diskGB, &flavorID)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Calculate uptime
	uptime := time.Since(createdAt).Seconds()

	// Return diagnostic information
	// In stub mode, return mock values; in real mode, these would come from libvirt
	c.JSON(http.StatusOK, gin.H{
		"state":            status,
		"driver":           "libvirt",
		"hypervisor":       "kvm",
		"uptime":           int(uptime),
		"num_cpus":         vcpus,
		"num_nics":         1,
		"num_disks":        1,
		"memory":           memoryMB,
		"memory-actual":    memoryMB,
		"memory-rss":       memoryMB / 2,
		"cpu0_time":        int(uptime * 1000000000), // nanoseconds
		"vda_read":         1024 * 1024,              // bytes
		"vda_write":        512 * 1024,               // bytes
		"vda_read_req":     100,
		"vda_write_req":    50,
		"vnet0_rx":         2048 * 1024, // bytes
		"vnet0_tx":         1024 * 1024,
		"vnet0_rx_packets": 1000,
		"vnet0_tx_packets": 500,
	})
}

// resolveDeletedServerForHistory reports whether the caller may read the action history
// of a server that no longer exists, and the instance id to read it under.
//
// Deliberately narrow. It grants nothing but a read of records already attributed to
// the caller's own project (or, for an administrator, to any). A caller who is neither
// gets no answer at all, so a deleted server stays indistinguishable from one that
// never existed (FR-043b) — telling them apart would confirm another tenant's server
// had existed.
//
// Only a well-formed identifier is accepted: after deletion there is no row left to
// match a NAME against, and matching names against action records would let one
// project's server name reach another's history.
func (svc *Service) resolveDeletedServerForHistory(c *gin.Context, ref string) (string, bool) {
	if !wantsDeletedInstanceActions(c) {
		return "", false
	}
	if uuid.Validate(ref) != nil {
		return "", false
	}

	query := `SELECT DISTINCT instance_id::text FROM instance_actions
	           WHERE instance_id = $1 AND project_id = $2`
	args := []any{ref, c.GetString("project_id")}
	if common.HasRole(c, common.RoleAdmin) {
		query = `SELECT DISTINCT instance_id::text FROM instance_actions WHERE instance_id = $1`
		args = []any{ref}
	}

	var id string
	if err := svc.pool.QueryRow(c.Request.Context(), query, args...).Scan(&id); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Error().Err(err).Str("instance_ref", ref).
				Msg("looking up a deleted server's action history failed")
		}
		return "", false
	}
	return id, true
}

// ListInstanceActions handles GET /v2.1/servers/:id/os-instance-actions
//
// A DELETED server's history is still readable, by its owner and by an administrator
// (SPEC-025 FR-043). This used to resolve the server first and answer *absent* to
// everyone once it was gone — which contradicted the retention decision: records were
// kept that nothing would ever serve, leaving an audit trail reachable only by someone
// with database access.
//
// After deletion the server row is gone, so authorization comes from the ownership
// recorded with each action instead (FR-043a). That column holds the OWNING project —
// SPEC-023 fixed which value is written to it — which is what makes it usable here.
//
// Gated on microversion 2.21, as upstream gates it. See microversion.go for why this is
// the product's only such gate.
func (svc *Service) ListInstanceActions(c *gin.Context) {
	ref := c.Param("id")

	instanceID, _, authorized := resolveServerOr404Silent(c, svc.pool, ref)
	if !authorized {
		// The server is not reachable as a live instance. It may still be one this
		// caller deleted, whose history they are entitled to.
		id, ok := svc.resolveDeletedServerForHistory(c, ref)
		if !ok {
			// Same *absent* answer as before, revealing nothing about whether the
			// server ever existed (FR-043b).
			denyCrossTenant(c, svc.pool, ref)
			common.SendError(c, common.NewNotFoundError("instance"))
			return
		}
		instanceID = id
	}

	// Query instance actions
	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT id, action, request_id, user_id, project_id, start_time, message
		 FROM instance_actions
		 WHERE instance_id = $1
		 ORDER BY start_time DESC`,
		instanceID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_instance_actions").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list instance actions"))
		return
	}
	defer rows.Close()

	actions := []gin.H{}
	for rows.Next() {
		var id, action, requestID, userID, projectIDStr, message string
		var startTime time.Time

		if err := rows.Scan(&id, &action, &requestID, &userID, &projectIDStr, &startTime, &message); err != nil {
			log.Warn().Err(err).Msg("failed to scan instance action row")
			continue
		}

		actions = append(actions, gin.H{
			"action":     action,
			"request_id": requestID,
			"user_id":    userID,
			"project_id": projectIDStr,
			"start_time": startTime.Format(time.RFC3339),
			"message":    message,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list instance actions"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"instanceActions": actions})
}

// GetInstanceAction handles GET /v2.1/servers/:id/os-instance-actions/:request_id
func (svc *Service) GetInstanceAction(c *gin.Context) {
	instanceID, _, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}
	requestID := c.Param("request_id")

	// Query specific action
	var action, userID, projectIDStr, message string
	var startTime time.Time

	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT action, user_id, project_id, start_time, message
		 FROM instance_actions
		 WHERE instance_id = $1 AND request_id = $2`,
		instanceID, requestID,
	).Scan(&action, &userID, &projectIDStr, &startTime, &message)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("action"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"instanceAction": gin.H{
			"action":     action,
			"request_id": requestID,
			"user_id":    userID,
			"project_id": projectIDStr,
			"start_time": startTime.Format(time.RFC3339),
			"message":    message,
		},
	})
}
