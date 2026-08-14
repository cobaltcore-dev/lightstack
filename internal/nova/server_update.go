package nova

import (
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// UpdateServer handles PATCH /v2.1/servers/:id
func (svc *Service) UpdateServer(c *gin.Context) {
	instanceID, projectID, authorized := resolveServerOr404(c, c.Param("id"))
	if !authorized {
		return
	}

	var req struct {
		Server struct {
			Name *string `json:"name"`
		} `json:"server"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify server exists and get current details
	var (
		currentName     string
		currentStatus   string
		currentFlavorID string
		currentImageID  *string
		currentUserID   *string
		createdAt       time.Time
		updatedAt       time.Time
	)

	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT name, status, flavor_id, image_id, user_id, created_at, updated_at
		FROM instances
		WHERE id = $1 AND project_id = $2
	`, instanceID, projectID).Scan(
		&currentName, &currentStatus,
		&currentFlavorID, &currentImageID, &currentUserID, &createdAt, &updatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server_for_update").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get server"))
		return
	}

	// Build update query dynamically based on what's being updated
	u := &common.SQLUpdate{}

	if req.Server.Name != nil {
		u.Set("name", *req.Server.Name)
		currentName = *req.Server.Name
	}

	if u.Empty() {
		// No updates requested, just return current state
	} else {
		u.Set("updated_at", time.Now())

		query := "UPDATE instances SET " + u.Assignments() +
			" WHERE id = " + u.Arg(instanceID) + " AND project_id = " + u.Arg(projectID)

		_, err = database.DB.Exec(c.Request.Context(), query, u.Args()...)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_server").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to update server"))
			return
		}
	}

	// Build response with full server details
	server := gin.H{
		"id":        instanceID,
		"name":      currentName,
		"status":    currentStatus,
		"tenant_id": projectID,
		"user_id": func() string {
			if currentUserID != nil {
				return *currentUserID
			}
			return ""
		}(),
		"created":   createdAt.Format(time.RFC3339),
		"updated":   time.Now().Format(time.RFC3339),
		"hostId":    "",
		"addresses": gin.H{},
		"links": []gin.H{
			{
				"href": c.Request.Host + "/v2.1/servers/" + instanceID,
				"rel":  "self",
			},
		},
		"image": gin.H{
			"id": "",
		},
		"flavor": gin.H{
			"id": currentFlavorID,
		},
	}

	if currentImageID != nil {
		server["image"] = gin.H{"id": *currentImageID}
	}

	c.JSON(http.StatusOK, gin.H{"server": server})
}
