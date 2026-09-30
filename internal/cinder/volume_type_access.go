package cinder

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// VolumeTypeAction handles POST /v3/types/:id/action
func (svc *Service) VolumeTypeAction(c *gin.Context) {
	typeID := c.Param("id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Handle addProjectAccess action
	if addAccess, ok := req["addProjectAccess"]; ok {
		addMap, ok := addAccess.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("addProjectAccess must be an object"))
			return
		}
		projectID, ok := addMap["project"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("addProjectAccess.project must be a string"))
			return
		}

		// Verify volume type exists and is private
		var isPublic bool
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT is_public FROM volume_types WHERE id = $1",
			typeID,
		).Scan(&isPublic)

		if err != nil {
			common.SendError(c, common.NewNotFoundError("volume type"))
			return
		}

		if isPublic {
			common.SendError(c, common.NewConflictError("cannot add access to public volume type"))
			return
		}

		// Add access
		_, err = database.DB.Exec(c.Request.Context(), `
			INSERT INTO volume_type_access (volume_type_id, project_id, created_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (volume_type_id, project_id) DO NOTHING
		`, typeID, projectID, time.Now())

		if err != nil {
			log.Error().Err(err).Str("operation", "add_volume_type_access").Msg("failed to add project access")
			common.SendError(c, common.NewInternalServerError("failed to add project access"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	// Handle removeProjectAccess action
	if removeAccess, ok := req["removeProjectAccess"]; ok {
		removeMap, ok := removeAccess.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("removeProjectAccess must be an object"))
			return
		}
		projectID, ok := removeMap["project"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("removeProjectAccess.project must be a string"))
			return
		}

		// Remove access
		result, err := database.DB.Exec(c.Request.Context(),
			"DELETE FROM volume_type_access WHERE volume_type_id = $1 AND project_id = $2",
			typeID, projectID,
		)

		if err != nil {
			log.Error().Err(err).Str("operation", "remove_volume_type_access").Msg("failed to remove project access")
			common.SendError(c, common.NewInternalServerError("failed to remove project access"))
			return
		}

		if result.RowsAffected() == 0 {
			common.SendError(c, common.NewNotFoundError("project access"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	common.SendError(c, common.NewBadRequestError("unknown action"))
}

// ListVolumeTypeAccess handles GET /v3/types/:id/os-volume-type-access
func (svc *Service) ListVolumeTypeAccess(c *gin.Context) {
	typeID := c.Param("id")

	// Verify volume type exists and is private
	var isPublic bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT is_public FROM volume_types WHERE id = $1",
		typeID,
	).Scan(&isPublic)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}

	if isPublic {
		// Public types don't have access control
		c.JSON(http.StatusOK, gin.H{"volume_type_access": []any{}})
		return
	}

	// Get access list
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT volume_type_id, project_id
		FROM volume_type_access
		WHERE volume_type_id = $1
	`, typeID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_volume_type_access").Msg("failed to query access list")
		common.SendError(c, common.NewInternalServerError("failed to list volume type access"))
		return
	}
	defer rows.Close()

	access := []map[string]any{}
	for rows.Next() {
		var volumeTypeID, projectID uuid.UUID
		if err := rows.Scan(&volumeTypeID, &projectID); err != nil {
			log.Warn().Err(err).Str("operation", "ListVolumeTypeAccess").Msg("failed to scan row")
		} else {
			access = append(access, map[string]any{
				"volume_type_id": volumeTypeID.String(),
				"project_id":     projectID.String(),
			})
		}
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"volume_type_access": access})
}
