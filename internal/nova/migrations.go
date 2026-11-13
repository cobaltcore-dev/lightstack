package nova

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListMigrations handles GET /v2.1/os-migrations — the deployment-wide list.
//
// Administrator-only (SPEC-023 FR-008e). It reports every project's instance
// movements — server uuid, source and destination node, flavour changes — and is
// scoped to no project, so a tenant reading it learns what every other tenant is
// doing. The comment below already said as much; the check was simply missing.
func (svc *Service) ListMigrations(c *gin.Context) {
	// List all migrations (admin endpoint in real OpenStack)
	if !common.RequireAdmin(c, "os-migrations:index") {
		return
	}
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, server_uuid, source_node, dest_node, old_flavor_id, new_flavor_id,
		       status, migration_type, created_at, updated_at
		FROM server_migrations
		ORDER BY created_at DESC
	`)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_migrations").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list migrations"))
		return
	}
	defer rows.Close()

	migrations := []map[string]any{}
	for rows.Next() {
		var id, serverUUID uuid.UUID
		var oldFlavorID, newFlavorID *uuid.UUID
		var sourceNode, destNode, status, migrationType string
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &serverUUID, &sourceNode, &destNode, &oldFlavorID, &newFlavorID,
			&status, &migrationType, &createdAt, &updatedAt)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan migration row")
			continue
		}

		migration := map[string]any{
			"id":             id.String(),
			"server_uuid":    serverUUID.String(),
			"source_node":    sourceNode,
			"dest_node":      destNode,
			"status":         status,
			"migration_type": migrationType,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		}

		if oldFlavorID != nil {
			migration["old_instance_type_id"] = oldFlavorID.String()
		}
		if newFlavorID != nil {
			migration["new_instance_type_id"] = newFlavorID.String()
		}

		migrations = append(migrations, migration)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list migrations"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"migrations": migrations})
}

// The per-server migration handlers below take the ordinary owner-or-administrator
// override, unlike the deployment-wide list above: these records belong to a
// server, and that server has an owner (SPEC-023 FR-008g).
//
// Before SPEC-023 none of the four carried ANY check — no project predicate and
// no role check — so any tenant could read, act on and delete another tenant's
// server migration records.
//
// ListServerMigrations handles GET /v2.1/servers/:id/migrations
func (svc *Service) ListServerMigrations(c *gin.Context) {
	serverID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, server_uuid, source_node, dest_node, old_flavor_id, new_flavor_id,
		       status, migration_type, created_at, updated_at
		FROM server_migrations
		WHERE server_uuid = $1
		ORDER BY created_at DESC
	`, serverID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_server_migrations").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list server migrations"))
		return
	}
	defer rows.Close()

	migrations := []map[string]any{}
	for rows.Next() {
		var id, serverUUID uuid.UUID
		var oldFlavorID, newFlavorID *uuid.UUID
		var sourceNode, destNode, status, migrationType string
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &serverUUID, &sourceNode, &destNode, &oldFlavorID, &newFlavorID,
			&status, &migrationType, &createdAt, &updatedAt)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan migration row")
			continue
		}

		migration := map[string]any{
			"id":             id.String(),
			"server_uuid":    serverUUID.String(),
			"source_node":    sourceNode,
			"dest_node":      destNode,
			"status":         status,
			"migration_type": migrationType,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		}

		if oldFlavorID != nil {
			migration["old_instance_type_id"] = oldFlavorID.String()
		}
		if newFlavorID != nil {
			migration["new_instance_type_id"] = newFlavorID.String()
		}

		migrations = append(migrations, migration)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list server migrations"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"migrations": migrations})
}

// GetServerMigration handles GET /v2.1/:project_id/servers/:id/migrations/:migration_id
func (svc *Service) GetServerMigration(c *gin.Context) {
	serverID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}
	migrationID := c.Param("migration_id")

	var id, serverUUID uuid.UUID
	var oldFlavorID, newFlavorID *uuid.UUID
	var sourceNode, destNode, status, migrationType string
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, server_uuid, source_node, dest_node, old_flavor_id, new_flavor_id,
		       status, migration_type, created_at, updated_at
		FROM server_migrations
		WHERE id = $1 AND server_uuid = $2
	`, migrationID, serverID).Scan(&id, &serverUUID, &sourceNode, &destNode, &oldFlavorID, &newFlavorID,
		&status, &migrationType, &createdAt, &updatedAt)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("migration"))
		return
	}

	migration := map[string]any{
		"id":             id.String(),
		"server_uuid":    serverUUID.String(),
		"source_node":    sourceNode,
		"dest_node":      destNode,
		"status":         status,
		"migration_type": migrationType,
		"created_at":     createdAt.Format(time.RFC3339),
		"updated_at":     updatedAt.Format(time.RFC3339),
	}

	if oldFlavorID != nil {
		migration["old_instance_type_id"] = oldFlavorID.String()
	}
	if newFlavorID != nil {
		migration["new_instance_type_id"] = newFlavorID.String()
	}

	c.JSON(http.StatusOK, gin.H{"migration": migration})
}

// DeleteServerMigration handles DELETE /v2.1/:project_id/servers/:id/migrations/:migration_id
func (svc *Service) DeleteServerMigration(c *gin.Context) {
	serverID, ownerProject, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}
	migrationID := c.Param("migration_id")

	// Delete (cancel) migration
	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM server_migrations WHERE id = $1 AND server_uuid = $2",
		migrationID, serverID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_server_migration").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete migration"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("migration"))
		return
	}

	// Recorded against the owning project under upstream's name for an abort
	// (LIVE_MIGRATION_CANCEL), as ServerAction records its actions (SPEC-023 FR-007a).
	recordInstanceAction(c, svc.pool, serverID, ownerProject, "live_migration_cancel")
	c.Status(http.StatusNoContent)
}

// ServerMigrationAction handles POST /v2.1/:project_id/servers/:id/migrations/:migration_id/action
func (svc *Service) ServerMigrationAction(c *gin.Context) {
	serverID, ownerProject, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}
	migrationID := c.Param("migration_id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Handle force_complete action
	if _, ok := req["force_complete"]; ok {
		// Update migration status to completed
		result, err := svc.pool.Exec(c.Request.Context(), `
			UPDATE server_migrations
			SET status = $1, updated_at = $2
			WHERE id = $3 AND server_uuid = $4
		`, "completed", time.Now(), migrationID, serverID)

		if err != nil {
			log.Error().Err(err).Str("operation", "force_complete_migration").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to complete migration"))
			return
		}

		if result.RowsAffected() == 0 {
			common.SendError(c, common.NewNotFoundError("migration"))
			return
		}

		recordInstanceAction(c, svc.pool, serverID, ownerProject, "live_migration_force_complete")
		c.Status(http.StatusAccepted)
		return
	}

	common.SendError(c, common.NewBadRequestError("Unknown action"))
}
