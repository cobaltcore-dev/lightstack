package neutron

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreateRBACPolicy handles POST /v2.0/rbac-policies. The policy belongs to the project an
// administrator names, or else to the caller's (SPEC-033 FR-058).
func (svc *Service) CreateRBACPolicy(c *gin.Context) {
	var req struct {
		RBACPolicy struct {
			ObjectType   string  `json:"object_type" binding:"required"`
			ObjectID     string  `json:"object_id" binding:"required"`
			TargetTenant string  `json:"target_tenant" binding:"required"`
			Action       string  `json:"action" binding:"required"`
			ProjectID    *string `json:"project_id"`
			TenantID     *string `json:"tenant_id"`
		} `json:"rbac_policy" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	projectID, ok := svc.ownerOf(c, req.RBACPolicy.ProjectID, req.RBACPolicy.TenantID)
	if !ok {
		return
	}

	policyID := uuid.New().String()
	now := time.Now()

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO rbac_policies (id, project_id, object_type, object_id, target_tenant, action, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, policyID, projectID, req.RBACPolicy.ObjectType, req.RBACPolicy.ObjectID, req.RBACPolicy.TargetTenant, req.RBACPolicy.Action, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_rbac_policy").Msg("failed to create RBAC policy")
		common.SendError(c, common.NewInternalServerError("failed to create RBAC policy"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"rbac_policy": gin.H{
			"id":            policyID,
			"project_id":    projectID,
			"object_type":   req.RBACPolicy.ObjectType,
			"object_id":     req.RBACPolicy.ObjectID,
			"target_tenant": req.RBACPolicy.TargetTenant,
			"action":        req.RBACPolicy.Action,
		},
	})
}

// ListRBACPolicies handles GET /v2.0/rbac-policies: the caller's project's policies, every
// project's for an administrator, narrowed by project_id/tenant_id (SPEC-033 FR-057).
func (svc *Service) ListRBACPolicies(c *gin.Context) {
	f := &common.SQLFilter{}
	projectScope(c, f, "project_id", ownedBy("project_id"))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, project_id, object_type, object_id, target_tenant, action
		FROM rbac_policies`+f.Where()+`
		ORDER BY created_at DESC
	`, f.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_rbac_policies").Msg("failed to query RBAC policies")
		common.SendError(c, common.NewInternalServerError("failed to list RBAC policies"))
		return
	}
	defer rows.Close()

	policies := []gin.H{}
	for rows.Next() {
		var id, projectID, objectType, objectID, targetTenant, action string

		if err := rows.Scan(&id, &projectID, &objectType, &objectID, &targetTenant, &action); err != nil {
			log.Warn().Err(err).Str("operation", "ListRBACPolicies").Msg("failed to scan row")
			continue
		}

		policies = append(policies, gin.H{
			"id":            id,
			"project_id":    projectID,
			"object_type":   objectType,
			"object_id":     objectID,
			"target_tenant": targetTenant,
			"action":        action,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"rbac_policies": policies})
}

// GetRBACPolicy handles GET /v2.0/rbac-policies/:id
func (svc *Service) GetRBACPolicy(c *gin.Context) {
	policyID := c.Param("id")
	projectID := c.GetString("project_id")

	var objectType, objectID, targetTenant, action string

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT project_id, object_type, object_id, target_tenant, action
		FROM rbac_policies
		WHERE id = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`
	`, policyID, projectID, isAdmin(c)).Scan(&projectID, &objectType, &objectID, &targetTenant, &action)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("RBAC policy"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"rbac_policy": gin.H{
			"id":            policyID,
			"project_id":    projectID,
			"object_type":   objectType,
			"object_id":     objectID,
			"target_tenant": targetTenant,
			"action":        action,
		},
	})
}

// UpdateRBACPolicy handles PUT /v2.0/rbac-policies/:id
func (svc *Service) UpdateRBACPolicy(c *gin.Context) {
	policyID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		RBACPolicy struct {
			TargetTenant *string `json:"target_tenant"`
		} `json:"rbac_policy" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build update query
	u := &common.SQLUpdate{}

	if req.RBACPolicy.TargetTenant != nil {
		u.Set("target_tenant", *req.RBACPolicy.TargetTenant)
	}

	// Checked BEFORE updated_at is added, so "nothing was asked for" is the empty case rather than
	// a count of one that a later assignment would silently shift.
	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}
	u.Set("updated_at", time.Now())

	query := "UPDATE rbac_policies SET " + u.Assignments() +
		" WHERE id = " + u.Arg(policyID) + " AND " + ownerOrAdmin("project_id", u.Arg(projectID), u.Arg(isAdmin(c)))

	_, err := svc.pool.Exec(c.Request.Context(), query, u.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_rbac_policy").Msg("failed to update RBAC policy")
		common.SendError(c, common.NewInternalServerError("failed to update RBAC policy"))
		return
	}

	// Return updated policy
	svc.GetRBACPolicy(c)
}

// DeleteRBACPolicy handles DELETE /v2.0/rbac-policies/:id
func (svc *Service) DeleteRBACPolicy(c *gin.Context) {
	policyID := c.Param("id")
	projectID := c.GetString("project_id")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM rbac_policies WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		policyID, projectID, isAdmin(c),
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_rbac_policy").Msg("failed to delete RBAC policy")
		common.SendError(c, common.NewInternalServerError("failed to delete RBAC policy"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("RBAC policy"))
		return
	}

	c.Status(http.StatusNoContent)
}
