package cinder

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreateVolumeType creates a new volume type
func (svc *Service) CreateVolumeType(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:CreateVolumeType") {
		return
	}

	var req struct {
		VolumeType struct {
			Name        string  `json:"name" binding:"required"`
			Description *string `json:"description"`
			IsPublic    *bool   `json:"os-volume-type-access:is_public"`
		} `json:"volume_type" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	typeID := uuid.New().String()
	isPublic := true
	if req.VolumeType.IsPublic != nil {
		isPublic = *req.VolumeType.IsPublic
	}

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO volume_types (id, name, description, is_public)
		VALUES ($1, $2, $3, $4)
	`, typeID, req.VolumeType.Name, req.VolumeType.Description, isPublic)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_volume_type").Msg("failed to insert volume type")
		common.SendError(c, common.NewInternalServerError("failed to create volume type"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"volume_type": map[string]any{
			"id":                              typeID,
			"name":                            req.VolumeType.Name,
			"description":                     req.VolumeType.Description,
			"os-volume-type-access:is_public": isPublic,
			"extra_specs":                     map[string]string{},
		},
	})
}

// UpdateVolumeType updates a volume type
func (svc *Service) UpdateVolumeType(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:UpdateVolumeType") {
		return
	}

	typeID := c.Param("id")

	var req struct {
		VolumeType struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			IsPublic    *bool   `json:"os-volume-type-access:is_public"`
		} `json:"volume_type" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check if type exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volume_types WHERE id = $1)",
		typeID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}

	// Build update query
	u := &common.SQLUpdate{}

	if req.VolumeType.Name != nil {
		u.Set("name", *req.VolumeType.Name)
	}

	if req.VolumeType.Description != nil {
		u.Set("description", *req.VolumeType.Description)
	}

	if req.VolumeType.IsPublic != nil {
		u.Set("is_public", *req.VolumeType.IsPublic)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	query := "UPDATE volume_types SET " + u.Assignments() + " WHERE id = " + u.Arg(typeID)

	_, err = svc.pool.Exec(c.Request.Context(), query, u.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_volume_type").Msg("failed to update volume type")
		common.SendError(c, common.NewInternalServerError("failed to update volume type"))
		return
	}

	// Return updated type
	svc.GetVolumeType(c)
}

// DeleteVolumeType deletes a volume type
func (svc *Service) DeleteVolumeType(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:DeleteVolumeType") {
		return
	}

	typeID := c.Param("id")

	// See GetVolumeType: a non-UUID reference must 404, not 500 (SPEC-028).
	if _, err := uuid.Parse(typeID); err != nil {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM volume_types WHERE id = $1",
		typeID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_type").Msg("failed to delete volume type")
		common.SendError(c, common.NewInternalServerError("failed to delete volume type"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ListVolumeTypeExtraSpecs lists extra specs for a volume type
func (svc *Service) ListVolumeTypeExtraSpecs(c *gin.Context) {
	typeID := c.Param("id")

	// Check if type exists
	var extraSpecs map[string]string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(extra_specs, '{}'::jsonb) FROM volume_types WHERE id = $1",
		typeID,
	).Scan(&extraSpecs)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "list_extra_specs").Msg("failed to query extra specs")
		common.SendError(c, common.NewInternalServerError("failed to list extra specs"))
		return
	}

	if extraSpecs == nil {
		extraSpecs = make(map[string]string)
	}

	c.JSON(http.StatusOK, gin.H{"extra_specs": extraSpecs})
}

// CreateVolumeTypeExtraSpecs handles POST /v3/types/:id/extra_specs
func (svc *Service) CreateVolumeTypeExtraSpecs(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:CreateVolumeTypeExtraSpecs") {
		return
	}

	typeID := c.Param("id")

	var req struct {
		ExtraSpecs map[string]string `json:"extra_specs" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Convert to JSONB
	extraSpecsJSON, err := json.Marshal(req.ExtraSpecs)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_extra_specs").Msg("failed to marshal extra specs")
		common.SendError(c, common.NewInternalServerError("failed to create extra specs"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE volume_types SET extra_specs = $1 WHERE id = $2",
		extraSpecsJSON, typeID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_extra_specs").Msg("failed to update extra specs")
		common.SendError(c, common.NewInternalServerError("failed to create extra specs"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"extra_specs": req.ExtraSpecs})
}

// GetVolumeTypeExtraSpecKey handles GET /v3/types/:id/extra_specs/:key
func (svc *Service) GetVolumeTypeExtraSpecKey(c *gin.Context) {
	typeID := c.Param("id")
	key := c.Param("key")

	var extraSpecs map[string]string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(extra_specs, '{}'::jsonb) FROM volume_types WHERE id = $1",
		typeID,
	).Scan(&extraSpecs)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_extra_spec_key").Msg("failed to query extra specs")
		common.SendError(c, common.NewInternalServerError("failed to get extra spec"))
		return
	}

	value, ok := extraSpecs[key]
	if !ok {
		common.SendError(c, common.NewNotFoundError("extra spec key"))
		return
	}

	c.JSON(http.StatusOK, gin.H{key: value})
}

// UpdateVolumeTypeExtraSpecKey handles PUT /v3/types/:id/extra_specs/:key
func (svc *Service) UpdateVolumeTypeExtraSpecKey(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:UpdateVolumeTypeExtraSpecKey") {
		return
	}

	typeID := c.Param("id")
	key := c.Param("key")

	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	value, ok := req[key]
	if !ok {
		common.SendError(c, common.NewBadRequestError("key not found in request body"))
		return
	}

	// Get existing extra specs
	var extraSpecs map[string]string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(extra_specs, '{}'::jsonb) FROM volume_types WHERE id = $1",
		typeID,
	).Scan(&extraSpecs)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_extra_spec_key").Msg("failed to query extra specs")
		common.SendError(c, common.NewInternalServerError("failed to update extra spec"))
		return
	}

	if extraSpecs == nil {
		extraSpecs = make(map[string]string)
	}

	// Update the key
	extraSpecs[key] = value

	// Save back to database
	extraSpecsJSON, err := json.Marshal(extraSpecs)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_extra_spec_key").Msg("failed to marshal extra specs")
		common.SendError(c, common.NewInternalServerError("failed to update extra spec"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE volume_types SET extra_specs = $1 WHERE id = $2",
		extraSpecsJSON, typeID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_extra_spec_key").Msg("failed to save extra specs")
		common.SendError(c, common.NewInternalServerError("failed to update extra spec"))
		return
	}

	c.JSON(http.StatusOK, gin.H{key: value})
}

// DeleteVolumeTypeExtraSpecKey handles DELETE /v3/types/:id/extra_specs/:key
func (svc *Service) DeleteVolumeTypeExtraSpecKey(c *gin.Context) {
	// Admin-only: volume types, QoS specs and the
	// manage/unmanage surface shape storage for every tenant and address
	// host-level backends (upstream: volume_extension:* and
	// volume_extension:volume_admin_actions:*, all admin_api). Checked before
	// the body is bound so a non-admin gets 403, not a 400.
	if !common.RequireAdmin(c, "cinder:DeleteVolumeTypeExtraSpecKey") {
		return
	}

	typeID := c.Param("id")
	key := c.Param("key")

	// Get existing extra specs
	var extraSpecs map[string]string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(extra_specs, '{}'::jsonb) FROM volume_types WHERE id = $1",
		typeID,
	).Scan(&extraSpecs)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_extra_spec_key").Msg("failed to query extra specs")
		common.SendError(c, common.NewInternalServerError("failed to delete extra spec"))
		return
	}

	if extraSpecs == nil {
		extraSpecs = make(map[string]string)
	}

	// Check if key exists
	if _, ok := extraSpecs[key]; !ok {
		common.SendError(c, common.NewNotFoundError("extra spec key"))
		return
	}

	// Delete the key
	delete(extraSpecs, key)

	// Save back to database
	extraSpecsJSON, err := json.Marshal(extraSpecs)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_extra_spec_key").Msg("failed to marshal extra specs")
		common.SendError(c, common.NewInternalServerError("failed to delete extra spec"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE volume_types SET extra_specs = $1 WHERE id = $2",
		extraSpecsJSON, typeID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_extra_spec_key").Msg("failed to save extra specs")
		common.SendError(c, common.NewInternalServerError("failed to delete extra spec"))
		return
	}

	c.Status(http.StatusNoContent)
}
