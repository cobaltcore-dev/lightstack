package neutron

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListMeteringLabels handles GET /v2.0/metering/metering-labels: the caller's project's labels and
// the shared ones, every project's for an administrator, narrowed by project_id/tenant_id (SPEC-033
// FR-057).
func (svc *Service) ListMeteringLabels(c *gin.Context) {
	f := &common.SQLFilter{}
	projectScope(c, f, "project_id", ownedOrShared)
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, description, project_id, shared, created_at, updated_at
		FROM metering_labels`+f.Where()+`
		ORDER BY created_at DESC
	`, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_metering_labels").Msg("failed to query metering labels")
		common.SendError(c, common.NewInternalServerError("failed to list metering labels"))
		return
	}
	defer rows.Close()

	labels := []map[string]any{}
	for rows.Next() {
		var id, projectID uuid.UUID
		var name string
		var description *string
		var shared bool
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &name, &description, &projectID, &shared, &createdAt, &updatedAt)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan metering label row")
			continue
		}

		label := map[string]any{
			"id":         id.String(),
			"name":       name,
			"project_id": projectID.String(),
			"tenant_id":  projectID.String(),
			"shared":     shared,
		}

		if description != nil {
			label["description"] = *description
		}

		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metering_labels": labels})
}

// CreateMeteringLabel handles POST /v2.0/metering/metering-labels. The label belongs to the project
// an administrator names, or else to the caller's (SPEC-033 FR-058).
func (svc *Service) CreateMeteringLabel(c *gin.Context) {
	var req struct {
		MeteringLabel struct {
			Name        string  `json:"name" binding:"required"`
			Description string  `json:"description"`
			Shared      bool    `json:"shared"`
			ProjectID   *string `json:"project_id"`
			TenantID    *string `json:"tenant_id"`
		} `json:"metering_label" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	projectID, ok := svc.ownerOf(c, req.MeteringLabel.ProjectID, req.MeteringLabel.TenantID)
	if !ok {
		return
	}

	id := uuid.New()
	var description *string
	if req.MeteringLabel.Description != "" {
		description = &req.MeteringLabel.Description
	}

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO metering_labels (id, name, description, project_id, shared, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, req.MeteringLabel.Name, description, projectID, req.MeteringLabel.Shared, time.Now(), time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_metering_label").Msg("failed to create metering label")
		common.SendError(c, common.NewInternalServerError("failed to create metering label"))
		return
	}

	label := map[string]any{
		"id":         id.String(),
		"name":       req.MeteringLabel.Name,
		"project_id": projectID,
		"tenant_id":  projectID,
		"shared":     req.MeteringLabel.Shared,
	}

	if description != nil {
		label["description"] = *description
	}

	c.JSON(http.StatusCreated, gin.H{"metering_label": label})
}

// GetMeteringLabel handles GET /v2.0/metering/metering-labels/:id
func (svc *Service) GetMeteringLabel(c *gin.Context) {
	projectID := c.GetString("project_id")
	labelID := c.Param("id")

	var id, labelProjectID uuid.UUID
	var name string
	var description *string
	var shared bool
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, project_id, shared, created_at, updated_at
		FROM metering_labels
		WHERE id = $1 AND `+scopeOrAdmin(ownedOrShared("$2"), "$3")+`
	`, labelID, projectID, isAdmin(c)).Scan(&id, &name, &description, &labelProjectID, &shared, &createdAt, &updatedAt)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("metering label"))
		return
	}

	label := map[string]any{
		"id":         id.String(),
		"name":       name,
		"project_id": labelProjectID.String(),
		"tenant_id":  labelProjectID.String(),
		"shared":     shared,
	}

	if description != nil {
		label["description"] = *description
	}

	c.JSON(http.StatusOK, gin.H{"metering_label": label})
}

// DeleteMeteringLabel handles DELETE /v2.0/metering/metering-labels/:id
func (svc *Service) DeleteMeteringLabel(c *gin.Context) {
	projectID := c.GetString("project_id")
	labelID := c.Param("id")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM metering_labels WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		labelID, projectID, isAdmin(c),
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_metering_label").Msg("failed to delete metering label")
		common.SendError(c, common.NewInternalServerError("failed to delete metering label"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("metering label"))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListMeteringLabelRules handles GET /v2.0/metering/metering-label-rules: the rules of the labels the
// caller sees, every project's for an administrator, narrowed by project_id/tenant_id on the label's
// project (SPEC-033 FR-057).
func (svc *Service) ListMeteringLabelRules(c *gin.Context) {
	f := &common.SQLFilter{}
	projectScope(c, f, "ml.project_id", func(caller string) string {
		return "(ml.project_id = " + caller + " OR ml.shared = true)"
	})
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT mlr.id, mlr.metering_label_id, mlr.remote_ip_prefix, mlr.direction, mlr.excluded, mlr.created_at
		FROM metering_label_rules mlr
		JOIN metering_labels ml ON mlr.metering_label_id = ml.id`+f.Where()+`
		ORDER BY mlr.created_at DESC
	`, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_metering_label_rules").Msg("failed to query metering label rules")
		common.SendError(c, common.NewInternalServerError("failed to list metering label rules"))
		return
	}
	defer rows.Close()

	rules := []map[string]any{}
	for rows.Next() {
		var id, labelID uuid.UUID
		var remoteIPPrefix, direction string
		var excluded bool
		var createdAt time.Time

		err := rows.Scan(&id, &labelID, &remoteIPPrefix, &direction, &excluded, &createdAt)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan metering label rule row")
			continue
		}

		rule := map[string]any{
			"id":                id.String(),
			"metering_label_id": labelID.String(),
			"remote_ip_prefix":  remoteIPPrefix,
			"direction":         direction,
			"excluded":          excluded,
		}

		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metering_label_rules": rules})
}

// CreateMeteringLabelRule handles POST /v2.0/metering/metering-label-rules
func (svc *Service) CreateMeteringLabelRule(c *gin.Context) {
	projectID := c.GetString("project_id")

	var req struct {
		MeteringLabelRule struct {
			MeteringLabelID string `json:"metering_label_id" binding:"required"`
			RemoteIPPrefix  string `json:"remote_ip_prefix" binding:"required"`
			Direction       string `json:"direction"`
			Excluded        bool   `json:"excluded"`
		} `json:"metering_label_rule" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify label belongs to project
	var labelExists bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM metering_labels WHERE id = $1 AND `+scopeOrAdmin(ownedOrShared("$2"), "$3")+`)
	`, req.MeteringLabelRule.MeteringLabelID, projectID, isAdmin(c)).Scan(&labelExists)

	if err != nil || !labelExists {
		common.SendError(c, common.NewNotFoundError("metering label"))
		return
	}

	// Default direction
	direction := req.MeteringLabelRule.Direction
	if direction == "" {
		direction = "ingress"
	}

	id := uuid.New()

	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO metering_label_rules (id, metering_label_id, remote_ip_prefix, direction, excluded, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, req.MeteringLabelRule.MeteringLabelID, req.MeteringLabelRule.RemoteIPPrefix, direction, req.MeteringLabelRule.Excluded, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_metering_label_rule").Msg("failed to create metering label rule")
		common.SendError(c, common.NewInternalServerError("failed to create metering label rule"))
		return
	}

	rule := map[string]any{
		"id":                id.String(),
		"metering_label_id": req.MeteringLabelRule.MeteringLabelID,
		"remote_ip_prefix":  req.MeteringLabelRule.RemoteIPPrefix,
		"direction":         direction,
		"excluded":          req.MeteringLabelRule.Excluded,
	}

	c.JSON(http.StatusCreated, gin.H{"metering_label_rule": rule})
}

// DeleteMeteringLabelRule handles DELETE /v2.0/metering/metering-label-rules/:id
func (svc *Service) DeleteMeteringLabelRule(c *gin.Context) {
	projectID := c.GetString("project_id")
	ruleID := c.Param("id")

	result, err := svc.pool.Exec(c.Request.Context(), `
		DELETE FROM metering_label_rules
		WHERE id = $1 AND metering_label_id IN (
			SELECT id FROM metering_labels WHERE `+ownerOrAdmin("project_id", "$2", "$3")+`
		)
	`, ruleID, projectID, isAdmin(c))

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_metering_label_rule").Msg("failed to delete metering label rule")
		common.SendError(c, common.NewInternalServerError("failed to delete metering label rule"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("metering label rule"))
		return
	}

	c.Status(http.StatusNoContent)
}
