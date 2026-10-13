package keystone

import (
	"errors"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// ListCredentials returns all credentials
func (svc *Service) ListCredentials(c *gin.Context) {
	// Review 2026-10-13 C-3: the generic credentials API returns the
	// recoverable EC2/S3 secret blob, so it is scoped like the EC2 API —
	// a non-admin sees only their own credentials; an admin sees all.
	callerID := c.GetString("user_id")
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, user_id, project_id, type, blob, created_at
		FROM credentials
		WHERE user_id = $1 OR $2
		ORDER BY created_at DESC
	`, callerID, common.HasRole(c, common.RoleAdmin))
	if err != nil {
		log.Error().Err(err).Str("operation", "list_credentials").Msg("Failed to query credentials")
		common.SendError(c, common.NewInternalServerError("failed to query credentials"))
		return
	}
	defer rows.Close()

	credentials := []map[string]any{}
	for rows.Next() {
		var id, userID, credType, blob string
		var projectID *string
		var createdAt time.Time

		if err := rows.Scan(&id, &userID, &projectID, &credType, &blob, &createdAt); err != nil {
			log.Warn().Err(err).Str("operation", "ListCredentials").Msg("failed to scan row")
			continue
		}

		credential := map[string]any{
			"id":      id,
			"user_id": userID,
			"type":    credType,
			"blob":    blob,
		}

		if projectID != nil {
			credential["project_id"] = *projectID
		}

		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(200, gin.H{"credentials": credentials})
}

// CreateCredential creates a new credential
func (svc *Service) CreateCredential(c *gin.Context) {
	var req struct {
		Credential struct {
			UserID    string `json:"user_id" binding:"required"`
			ProjectID string `json:"project_id"`
			Type      string `json:"type" binding:"required"`
			Blob      string `json:"blob" binding:"required"`
		} `json:"credential"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Self-or-admin (review 2026-10-13 C-3): a credential's blob is the
	// recoverable secret consumed by S3/EC2 signature auth — minting one for
	// another user (with a known secret) would defeat that auth.
	if c.GetString("user_id") != req.Credential.UserID && !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("cannot create credentials for another user"))
		return
	}

	credID := uuid.New()
	now := time.Now()

	var projectID any
	if req.Credential.ProjectID != "" {
		projectID = req.Credential.ProjectID
	}

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO credentials (id, user_id, project_id, type, blob, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, credID, req.Credential.UserID, projectID, req.Credential.Type, req.Credential.Blob, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_credential").Msg("Failed to create credential")
		common.SendError(c, common.NewInternalServerError("failed to create credential"))
		return
	}

	credential := map[string]any{
		"id":      credID.String(),
		"user_id": req.Credential.UserID,
		"type":    req.Credential.Type,
		"blob":    req.Credential.Blob,
	}

	if req.Credential.ProjectID != "" {
		credential["project_id"] = req.Credential.ProjectID
	}

	c.JSON(201, gin.H{"credential": credential})
}

// credentialOwnerOrViewer enforces the owner-or-admin rule on a single
// credential (review 2026-10-13 C-3): 404 when it does not exist, 403 when it
// belongs to another user and the caller is not admin.
func (svc *Service) credentialOwnerOrViewer(c *gin.Context, credID string) bool {
	var owner string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT user_id FROM credentials WHERE id = $1", credID,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewIdentityNotFoundError("credential", credID))
		return false
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "credential_owner").Str("cred_id", credID).Msg("Failed to query credential")
		common.SendError(c, common.NewInternalServerError("failed to query credential"))
		return false
	}
	if c.GetString("user_id") != owner && !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("cannot manage another user's credentials"))
		return false
	}
	return true
}

// GetCredential returns a specific credential by ID
func (svc *Service) GetCredential(c *gin.Context) {
	credID := c.Param("id")

	if !svc.credentialOwnerOrViewer(c, credID) {
		return
	}

	var id, userID, credType, blob string
	var projectID *string

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, user_id, project_id, type, blob
		FROM credentials
		WHERE id = $1
	`, credID).Scan(&id, &userID, &projectID, &credType, &blob)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewIdentityNotFoundError("credential", credID))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_credential").Str("cred_id", credID).Msg("Failed to query credential")
		common.SendError(c, common.NewInternalServerError("failed to query credential"))
		return
	}

	credential := map[string]any{
		"id":      id,
		"user_id": userID,
		"type":    credType,
		"blob":    blob,
	}

	if projectID != nil {
		credential["project_id"] = *projectID
	}

	c.JSON(200, gin.H{"credential": credential})
}

// UpdateCredential updates a credential
func (svc *Service) UpdateCredential(c *gin.Context) {
	credID := c.Param("id")

	if !svc.credentialOwnerOrViewer(c, credID) {
		return
	}

	var req struct {
		Credential map[string]any `json:"credential"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build dynamic update query
	u := &common.SQLUpdate{}

	if blob, ok := req.Credential["blob"].(string); ok {
		u.Set("blob", blob)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	u.Set("updated_at", time.Now())
	query := "UPDATE credentials SET " + u.Assignments() + " WHERE id = " + u.Arg(credID)

	result, err := svc.pool.Exec(c.Request.Context(), query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_credential").Str("cred_id", credID).Msg("Failed to update credential")
		common.SendError(c, common.NewInternalServerError("failed to update credential"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("credential", credID))
		return
	}

	// Return updated credential
	svc.GetCredential(c)
}

// DeleteCredential deletes a credential
func (svc *Service) DeleteCredential(c *gin.Context) {
	credID := c.Param("id")

	if !svc.credentialOwnerOrViewer(c, credID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM credentials WHERE id = $1",
		credID)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_credential").Str("cred_id", credID).Msg("Failed to delete credential")
		common.SendError(c, common.NewInternalServerError("failed to delete credential"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("credential", credID))
		return
	}

	c.Status(204)
}
