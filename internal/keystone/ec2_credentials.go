package keystone

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// ec2Blob is the JSON stored in credentials.blob for an EC2/S3 credential. The
// secret is stored recoverably (not hashed) because POST /v3/s3tokens must
// recompute the S3 HMAC signature from it — this is how OpenStack Keystone
// stores EC2 credentials (SPEC-014 plan Complexity Tracking).
type ec2Blob struct {
	Access   string `json:"access"`
	Secret   string `json:"secret"`
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ec2CredentialView(userID, tenantID, access, secret string) gin.H {
	return gin.H{
		"user_id":   userID,
		"tenant_id": tenantID,
		"access":    access,
		"secret":    secret,
		"trust_id":  nil,
		"links": gin.H{
			"self": "/v3/users/" + userID + "/credentials/OS-EC2/" + access,
		},
	}
}

// denyIfNotSelfOrAdmin writes a 403 and returns true when the caller is neither
// the target user nor an admin. The EC2-credential API returns the secret, so
// every handler must be owner-or-admin only — otherwise any authenticated tenant
// could read or mint another user's S3 credential and reach that tenant's RGW
// namespace (SPEC-014 FR-007; mirrors the self-or-admin check in ChangePassword).
func denyIfNotSelfOrAdmin(c *gin.Context, userID string) bool {
	if c.GetString("user_id") != userID && !common.HasRole(c, "admin") {
		common.SendError(c, common.NewForbiddenError("cannot manage another user's EC2 credentials"))
		return true
	}
	// Authorization passed; the path id must still be a well-formed user id or no
	// such user exists (cloud-storage#515). Checked after authz so a 403 always
	// precedes this 404 (FR-009), and so a malformed id never reaches the query.
	if uuid.Validate(userID) != nil {
		common.SendError(c, common.NewIdentityNotFoundError("user", userID))
		return true
	}
	return false
}

// CreateEC2Credential mints a long-lived, project-scoped S3 access/secret pair
// for a user (OpenStack EC2-credentials API; SPEC-014 FR-005).
// POST /v3/users/:id/credentials/OS-EC2
func (svc *Service) CreateEC2Credential(c *gin.Context) {
	userID := c.Param("id")

	if denyIfNotSelfOrAdmin(c, userID) {
		return
	}
	isAdmin := common.HasRole(c, "admin")

	var req struct {
		TenantID string `json:"tenant_id"`
	}
	_ = c.ShouldBindJSON(&req)
	// Admins may scope the credential to any project; a self-service caller is
	// bound to its own currently-scoped project so it can't mint a credential into
	// a project it has no role in.
	projectID := req.TenantID
	if projectID == "" || !isAdmin {
		projectID = c.GetString("project_id")
	}
	if projectID == "" {
		common.SendError(c, common.NewBadRequestError("tenant_id is required"))
		return
	}
	// A client may pass the dash-free project id used in RGW-compatible token
	// responses. uuid.Parse accepts both that form and the canonical UUID; the
	// parsed value below is stored canonically for the database and credential
	// blob, while /v3/s3tokens renders the dash-free tenant form.
	// Match Keystone (get_project -> ProjectNotFound = 404): a non-existent or
	// malformed tenant_id is a 404 with Keystone's exact message, not the generic
	// 500 the credentials.project_id foreign key would otherwise produce. A
	// malformed id can't name a real project, so treat it as not-found too.
	parsedTenantID, err := uuid.Parse(projectID)
	if err != nil {
		common.SendError(c, common.NewProjectNotFoundError(projectID))
		return
	}
	// Canonicalize before storing: uuid.Parse (like Postgres) accepts valid
	// UUIDs in alternate text encodings — 32 hex chars without dashes,
	// uppercase hex, braced — and the blob retains the stored spelling while
	// /v3/s3tokens derives the S3 tenant from it. Storing the canonical form
	// keeps the credential's tenant identical to the token/Swift side for
	// any valid input encoding.
	projectID = parsedTenantID.String()
	var projectExists bool
	if err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)", projectID).Scan(&projectExists); err != nil {
		log.Error().Err(err).Str("operation", "create_ec2_credential").Msg("project existence check failed")
		common.SendError(c, common.NewInternalServerError("failed to create EC2 credential"))
		return
	} else if !projectExists {
		common.SendError(c, common.NewProjectNotFoundError(projectID))
		return
	}

	access, err := randHex(16) // 32 hex chars
	if err != nil {
		log.Error().Err(err).Msg("ec2 cred: access key generation failed")
		common.SendError(c, common.NewInternalServerError("failed to create EC2 credential"))
		return
	}
	secret, err := randHex(20) // 40 hex chars
	if err != nil {
		log.Error().Err(err).Msg("ec2 cred: secret generation failed")
		common.SendError(c, common.NewInternalServerError("failed to create EC2 credential"))
		return
	}

	blob, _ := json.Marshal(ec2Blob{Access: access, Secret: secret, UserID: userID, TenantID: projectID})
	if _, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO credentials (id, user_id, project_id, type, blob, access_key, created_at, updated_at)
		VALUES ($1, $2, $3, 'ec2', $4, $5, $6, $6)
	`, uuid.New(), userID, projectID, string(blob), access, time.Now()); err != nil {
		log.Error().Err(err).Str("operation", "create_ec2_credential").Str("user_id", userID).Msg("failed to store EC2 credential")
		common.SendError(c, common.NewInternalServerError("failed to create EC2 credential"))
		return
	}

	c.JSON(201, gin.H{"credential": ec2CredentialView(userID, projectID, access, secret)})
}

// ListEC2Credentials lists a user's EC2 credentials.
// GET /v3/users/:id/credentials/OS-EC2
func (svc *Service) ListEC2Credentials(c *gin.Context) {
	userID := c.Param("id")
	if denyIfNotSelfOrAdmin(c, userID) {
		return
	}
	rows, err := database.DB.Query(c.Request.Context(),
		`SELECT project_id, blob FROM credentials WHERE user_id = $1 AND type = 'ec2' ORDER BY created_at DESC`, userID)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_ec2_credentials").Msg("failed to list EC2 credentials")
		common.SendError(c, common.NewInternalServerError("failed to list EC2 credentials"))
		return
	}
	defer rows.Close()

	creds := []gin.H{}
	for rows.Next() {
		var projectID *string
		var blob string
		if err := rows.Scan(&projectID, &blob); err != nil {
			log.Warn().Err(err).Str("operation", "ListEC2Credentials").Msg("failed to scan row")
			continue
		}
		var b ec2Blob
		if json.Unmarshal([]byte(blob), &b) != nil {
			continue
		}
		tid := b.TenantID
		if tid == "" && projectID != nil {
			tid = *projectID
		}
		creds = append(creds, ec2CredentialView(userID, tid, b.Access, b.Secret))
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("ec2 cred list: rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(200, gin.H{"credentials": creds})
}

// GetEC2Credential returns one EC2 credential by access key.
// GET /v3/users/:id/credentials/OS-EC2/:access
func (svc *Service) GetEC2Credential(c *gin.Context) {
	userID := c.Param("id")
	access := c.Param("access")
	if denyIfNotSelfOrAdmin(c, userID) {
		return
	}

	var projectID *string
	var blob string
	err := database.DB.QueryRow(c.Request.Context(),
		`SELECT project_id, blob FROM credentials WHERE user_id = $1 AND access_key = $2 AND type = 'ec2'`,
		userID, access).Scan(&projectID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("credential"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_ec2_credential").Msg("failed to get EC2 credential")
		common.SendError(c, common.NewInternalServerError("failed to get EC2 credential"))
		return
	}

	var b ec2Blob
	_ = json.Unmarshal([]byte(blob), &b)
	tid := b.TenantID
	if tid == "" && projectID != nil {
		tid = *projectID
	}
	c.JSON(200, gin.H{"credential": ec2CredentialView(userID, tid, b.Access, b.Secret)})
}

// DeleteEC2Credential revokes an EC2 credential (deletes the row).
// DELETE /v3/users/:id/credentials/OS-EC2/:access
func (svc *Service) DeleteEC2Credential(c *gin.Context) {
	userID := c.Param("id")
	access := c.Param("access")
	if denyIfNotSelfOrAdmin(c, userID) {
		return
	}

	res, err := database.DB.Exec(c.Request.Context(),
		`DELETE FROM credentials WHERE user_id = $1 AND access_key = $2 AND type = 'ec2'`, userID, access)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_ec2_credential").Msg("failed to delete EC2 credential")
		common.SendError(c, common.NewInternalServerError("failed to delete EC2 credential"))
		return
	}
	if res.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("credential"))
		return
	}
	c.Status(204)
}
