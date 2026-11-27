package neutron

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListTrunks handles GET /v2.0/trunks: the caller's project's trunks, every project's for an
// administrator, narrowed by project_id/tenant_id (SPEC-033 FR-057).
func (svc *Service) ListTrunks(c *gin.Context) {
	f := &common.SQLFilter{}
	projectScope(c, f, "project_id", ownedBy("project_id"))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at
		FROM trunks`+f.Where(), f.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_trunks").Msg("failed to query trunks")
		common.SendError(c, common.NewInternalServerError("failed to list trunks"))
		return
	}
	defer rows.Close()

	trunks := []map[string]any{}
	for rows.Next() {
		var id, portID uuid.UUID
		var projectIDStr uuid.UUID
		var name, description, status string
		var adminStateUp bool
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &name, &description, &projectIDStr, &portID, &adminStateUp, &status, &createdAt, &updatedAt)
		if err != nil {
			continue
		}

		// Get subports
		subPortRows, _ := svc.pool.Query(c.Request.Context(), `
			SELECT port_id, segmentation_type, segmentation_id
			FROM trunk_subports
			WHERE trunk_id = $1
		`, id)

		subPorts := []map[string]any{}
		if subPortRows != nil {
			for subPortRows.Next() {
				var subPortID uuid.UUID
				var segmentationType string
				var segmentationID int
				if subPortRows.Scan(&subPortID, &segmentationType, &segmentationID) == nil {
					subPorts = append(subPorts, map[string]any{
						"port_id":           subPortID.String(),
						"segmentation_type": segmentationType,
						"segmentation_id":   segmentationID,
					})
				}
			}
			if err := subPortRows.Err(); err != nil {
				log.Warn().Err(err).Msg("subPortRows iteration error; list may be truncated")
			}
			subPortRows.Close()
		}

		trunk := map[string]any{
			"id":             id.String(),
			"name":           name,
			"description":    description,
			"project_id":     projectIDStr.String(),
			"tenant_id":      projectIDStr.String(),
			"port_id":        portID.String(),
			"admin_state_up": adminStateUp,
			"status":         status,
			"sub_ports":      subPorts,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		}
		trunks = append(trunks, trunk)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"trunks": trunks})
}

// trunkPort resolves a trunk's parent port or subport: a port the caller can see, any project's for an
// administrator, as upstream's trunk validator reads it in the caller's context (404 otherwise; SPEC-033
// FR-073, T258). A malformed id is 400.
func trunkPort(ctx context.Context, q subnetQuerier, portID string, caller callerScope) (string, error) {
	if uuid.Validate(portID) != nil {
		return "", common.NewBadRequestError("invalid port_id")
	}
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM ports WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		portID, caller.project, caller.admin).Scan(&id)
	if errors.Is(err, database.ErrNoRows) {
		return "", common.NewNotFoundError("port")
	}
	return id, err
}

// CreateTrunk handles POST /v2.0/trunks. The trunk belongs to the project an administrator names, or
// else to the caller's (SPEC-033 FR-058).
func (svc *Service) CreateTrunk(c *gin.Context) {
	var req struct {
		Trunk struct {
			Name         string  `json:"name"`
			Description  string  `json:"description"`
			PortID       string  `json:"port_id" binding:"required"`
			AdminStateUp *bool   `json:"admin_state_up"`
			ProjectID    *string `json:"project_id"`
			TenantID     *string `json:"tenant_id"`
		} `json:"trunk" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	projectID, ok := svc.ownerOf(c, req.Trunk.ProjectID, req.Trunk.TenantID)
	if !ok {
		return
	}

	adminStateUp := true
	if req.Trunk.AdminStateUp != nil {
		adminStateUp = *req.Trunk.AdminStateUp
	}

	trunkID := uuid.New()
	portID, err := trunkPort(c.Request.Context(), svc.pool, req.Trunk.PortID, scopeOf(c))
	if err != nil {
		sendChangeError(c, err, "create", "trunk")
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO trunks (id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, trunkID, req.Trunk.Name, req.Trunk.Description, projectID, portID, adminStateUp, "ACTIVE", time.Now(), time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_trunk").Msg("failed to create trunk")
		common.SendError(c, common.NewInternalServerError("failed to create trunk"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"trunk": map[string]any{
			"id":             trunkID.String(),
			"name":           req.Trunk.Name,
			"description":    req.Trunk.Description,
			"project_id":     projectID,
			"tenant_id":      projectID,
			"port_id":        req.Trunk.PortID,
			"admin_state_up": adminStateUp,
			"status":         "ACTIVE",
			"sub_ports":      []any{},
			"created_at":     time.Now().Format(time.RFC3339),
			"updated_at":     time.Now().Format(time.RFC3339),
		},
	})
}

// GetTrunk handles GET /v2.0/trunks/:id
func (svc *Service) GetTrunk(c *gin.Context) {
	trunkID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, portID uuid.UUID
	var projectIDStr uuid.UUID
	var name, description, status string
	var adminStateUp bool
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at
		FROM trunks
		WHERE id = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`
	`, trunkID, projectID, isAdmin(c)).Scan(&id, &name, &description, &projectIDStr, &portID, &adminStateUp, &status, &createdAt, &updatedAt)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("trunk"))
		return
	}

	// Get subports
	subPortRows, _ := svc.pool.Query(c.Request.Context(), `
		SELECT port_id, segmentation_type, segmentation_id
		FROM trunk_subports
		WHERE trunk_id = $1
	`, id)

	subPorts := []map[string]any{}
	if subPortRows != nil {
		defer subPortRows.Close()
		for subPortRows.Next() {
			var subPortID uuid.UUID
			var segmentationType string
			var segmentationID int
			if subPortRows.Scan(&subPortID, &segmentationType, &segmentationID) == nil {
				subPorts = append(subPorts, map[string]any{
					"port_id":           subPortID.String(),
					"segmentation_type": segmentationType,
					"segmentation_id":   segmentationID,
				})
			}
		}
		if err := subPortRows.Err(); err != nil {
			log.Warn().Err(err).Msg("subPortRows iteration error; list may be truncated")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"trunk": map[string]any{
			"id":             id.String(),
			"name":           name,
			"description":    description,
			"project_id":     projectIDStr.String(),
			"tenant_id":      projectIDStr.String(),
			"port_id":        portID.String(),
			"admin_state_up": adminStateUp,
			"status":         status,
			"sub_ports":      subPorts,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		},
	})
}

// UpdateTrunk handles PUT /v2.0/trunks/:id
func (svc *Service) UpdateTrunk(c *gin.Context) {
	trunkID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Trunk map[string]any `json:"trunk" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify trunk exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM trunks WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+")",
		trunkID, projectID, isAdmin(c),
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("trunk"))
		return
	}

	// Build UPDATE query dynamically
	u := &common.SQLUpdate{}

	if name, ok := req.Trunk["name"].(string); ok {
		u.Set("name", name)
	}

	if description, ok := req.Trunk["description"].(string); ok {
		u.Set("description", description)
	}

	if adminStateUp, ok := req.Trunk["admin_state_up"].(bool); ok {
		u.Set("admin_state_up", adminStateUp)
	}

	if !u.Empty() {
		u.Set("updated_at", time.Now())

		query := "UPDATE trunks SET " + u.Assignments() +
			" WHERE id = " + u.Arg(trunkID) + " AND " + ownerOrAdmin("project_id", u.Arg(projectID), u.Arg(isAdmin(c)))

		_, err = svc.pool.Exec(c.Request.Context(), query, u.Args()...)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_trunk").Msg("failed to update trunk")
			common.SendError(c, common.NewInternalServerError("failed to update trunk"))
			return
		}
	}

	// Fetch updated trunk
	var id, portID uuid.UUID
	var projectIDStr uuid.UUID
	var name, description, status string
	var adminStateUp bool
	var createdAt, updatedAt time.Time

	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at
		FROM trunks
		WHERE id = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`
	`, trunkID, projectID, isAdmin(c)).Scan(&id, &name, &description, &projectIDStr, &portID, &adminStateUp, &status, &createdAt, &updatedAt)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_trunk_fetch").Msg("failed to fetch updated trunk")
		common.SendError(c, common.NewInternalServerError("failed to fetch updated trunk"))
		return
	}

	// Get subports
	subPortRows, _ := svc.pool.Query(c.Request.Context(), `
		SELECT port_id, segmentation_type, segmentation_id
		FROM trunk_subports
		WHERE trunk_id = $1
	`, id)

	subPorts := []map[string]any{}
	if subPortRows != nil {
		defer subPortRows.Close()
		for subPortRows.Next() {
			var subPortID uuid.UUID
			var segmentationType string
			var segmentationID int
			if subPortRows.Scan(&subPortID, &segmentationType, &segmentationID) == nil {
				subPorts = append(subPorts, map[string]any{
					"port_id":           subPortID.String(),
					"segmentation_type": segmentationType,
					"segmentation_id":   segmentationID,
				})
			}
		}
		if err := subPortRows.Err(); err != nil {
			log.Warn().Err(err).Msg("subPortRows iteration error; list may be truncated")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"trunk": map[string]any{
			"id":             id.String(),
			"name":           name,
			"description":    description,
			"project_id":     projectIDStr.String(),
			"tenant_id":      projectIDStr.String(),
			"port_id":        portID.String(),
			"admin_state_up": adminStateUp,
			"status":         status,
			"sub_ports":      subPorts,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		},
	})
}

// DeleteTrunk handles DELETE /v2.0/trunks/:id
func (svc *Service) DeleteTrunk(c *gin.Context) {
	trunkID := c.Param("id")
	projectID := c.GetString("project_id")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM trunks WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		trunkID, projectID, isAdmin(c),
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_trunk").Msg("failed to delete trunk")
		common.SendError(c, common.NewInternalServerError("failed to delete trunk"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewNotFoundError("trunk"))
		return
	}

	c.Status(http.StatusNoContent)
}

// AddSubports handles PUT /v2.0/trunks/:id/add_subports
func (svc *Service) AddSubports(c *gin.Context) {
	trunkID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		SubPorts []struct {
			PortID           string `json:"port_id" binding:"required"`
			SegmentationType string `json:"segmentation_type" binding:"required"`
			SegmentationID   int    `json:"segmentation_id" binding:"required"`
		} `json:"sub_ports" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify trunk exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM trunks WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+")",
		trunkID, projectID, isAdmin(c),
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("trunk"))
		return
	}

	// Every subport is a port the caller can see, all checked before any is added.
	ports := make([]string, len(req.SubPorts))
	for i, subPort := range req.SubPorts {
		if ports[i], err = trunkPort(c.Request.Context(), svc.pool, subPort.PortID, scopeOf(c)); err != nil {
			sendChangeError(c, err, "add a subport to", "trunk")
			return
		}
	}
	for i, subPort := range req.SubPorts {
		portID := ports[i]
		_, err = svc.pool.Exec(c.Request.Context(), `
			INSERT INTO trunk_subports (trunk_id, port_id, segmentation_type, segmentation_id, created_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (trunk_id, port_id) DO NOTHING
		`, trunkID, portID, subPort.SegmentationType, subPort.SegmentationID, time.Now())

		if err != nil {
			log.Error().Err(err).Str("operation", "add_subports").Msg("failed to add subport")
			common.SendError(c, common.NewInternalServerError("failed to add subport"))
			return
		}
	}

	// Update trunk updated_at
	_, _ = svc.pool.Exec(c.Request.Context(),
		"UPDATE trunks SET updated_at = $1 WHERE id = $2",
		time.Now(), trunkID,
	)

	// Fetch updated trunk
	var id, portID uuid.UUID
	var projectIDStr uuid.UUID
	var name, description, status string
	var adminStateUp bool
	var createdAt, updatedAt time.Time

	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at
		FROM trunks
		WHERE id = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`
	`, trunkID, projectID, isAdmin(c)).Scan(&id, &name, &description, &projectIDStr, &portID, &adminStateUp, &status, &createdAt, &updatedAt)

	if err != nil {
		log.Error().Err(err).Str("operation", "add_subports_fetch").Msg("failed to fetch trunk after adding subports")
		common.SendError(c, common.NewInternalServerError("failed to fetch trunk"))
		return
	}

	// Get subports
	subPortRows, _ := svc.pool.Query(c.Request.Context(), `
		SELECT port_id, segmentation_type, segmentation_id
		FROM trunk_subports
		WHERE trunk_id = $1
	`, id)

	subPorts := []map[string]any{}
	if subPortRows != nil {
		defer subPortRows.Close()
		for subPortRows.Next() {
			var subPortID uuid.UUID
			var segmentationType string
			var segmentationID int
			if subPortRows.Scan(&subPortID, &segmentationType, &segmentationID) == nil {
				subPorts = append(subPorts, map[string]any{
					"port_id":           subPortID.String(),
					"segmentation_type": segmentationType,
					"segmentation_id":   segmentationID,
				})
			}
		}
		if err := subPortRows.Err(); err != nil {
			log.Warn().Err(err).Msg("subPortRows iteration error; list may be truncated")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"trunk": map[string]any{
			"id":             id.String(),
			"name":           name,
			"description":    description,
			"project_id":     projectIDStr.String(),
			"tenant_id":      projectIDStr.String(),
			"port_id":        portID.String(),
			"admin_state_up": adminStateUp,
			"status":         status,
			"sub_ports":      subPorts,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		},
	})
}

// RemoveSubports handles PUT /v2.0/trunks/:id/remove_subports
func (svc *Service) RemoveSubports(c *gin.Context) {
	trunkID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		SubPorts []struct {
			PortID string `json:"port_id" binding:"required"`
		} `json:"sub_ports" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify trunk exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM trunks WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+")",
		trunkID, projectID, isAdmin(c),
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("trunk"))
		return
	}

	// Remove subports
	for _, subPort := range req.SubPorts {
		_, err = svc.pool.Exec(c.Request.Context(),
			"DELETE FROM trunk_subports WHERE trunk_id = $1 AND port_id = $2",
			trunkID, subPort.PortID,
		)

		if err != nil {
			log.Error().Err(err).Str("operation", "remove_subports").Msg("failed to remove subport")
			common.SendError(c, common.NewInternalServerError("failed to remove subport"))
			return
		}
	}

	// Update trunk updated_at
	_, _ = svc.pool.Exec(c.Request.Context(),
		"UPDATE trunks SET updated_at = $1 WHERE id = $2",
		time.Now(), trunkID,
	)

	// Fetch updated trunk
	var id, portID uuid.UUID
	var projectIDStr uuid.UUID
	var name, description, status string
	var adminStateUp bool
	var createdAt, updatedAt time.Time

	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, project_id, port_id, admin_state_up, status, created_at, updated_at
		FROM trunks
		WHERE id = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`
	`, trunkID, projectID, isAdmin(c)).Scan(&id, &name, &description, &projectIDStr, &portID, &adminStateUp, &status, &createdAt, &updatedAt)

	if err != nil {
		log.Error().Err(err).Str("operation", "remove_subports_fetch").Msg("failed to fetch trunk after removing subports")
		common.SendError(c, common.NewInternalServerError("failed to fetch trunk"))
		return
	}

	// Get subports
	subPortRows, _ := svc.pool.Query(c.Request.Context(), `
		SELECT port_id, segmentation_type, segmentation_id
		FROM trunk_subports
		WHERE trunk_id = $1
	`, id)

	subPorts := []map[string]any{}
	if subPortRows != nil {
		defer subPortRows.Close()
		for subPortRows.Next() {
			var subPortID uuid.UUID
			var segmentationType string
			var segmentationID int
			if subPortRows.Scan(&subPortID, &segmentationType, &segmentationID) == nil {
				subPorts = append(subPorts, map[string]any{
					"port_id":           subPortID.String(),
					"segmentation_type": segmentationType,
					"segmentation_id":   segmentationID,
				})
			}
		}
		if err := subPortRows.Err(); err != nil {
			log.Warn().Err(err).Msg("subPortRows iteration error; list may be truncated")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"trunk": map[string]any{
			"id":             id.String(),
			"name":           name,
			"description":    description,
			"project_id":     projectIDStr.String(),
			"tenant_id":      projectIDStr.String(),
			"port_id":        portID.String(),
			"admin_state_up": adminStateUp,
			"status":         status,
			"sub_ports":      subPorts,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		},
	})
}
