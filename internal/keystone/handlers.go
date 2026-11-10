package keystone

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Service handles Keystone API endpoints
type Service struct {
	pool        database.DB
	authService *AuthService
	cache       *cache.Cache
}

// NewService creates a new Keystone service
func NewService(pool database.DB, authService *AuthService, cacheInstance *cache.Cache) *Service {
	return &Service{
		pool:        pool,
		authService: authService,
		cache:       cacheInstance,
	}
}

// RegisterRoutes registers Keystone routes.
// Public routes (auth) require no token. Management routes require a valid token.
func (svc *Service) RegisterRoutes(r *gin.RouterGroup, authMiddleware gin.HandlerFunc) {
	v3 := r.Group("/v3")

	// Version discovery — no auth required
	v3.GET("", svc.GetVersion)

	// Authentication — no auth required (these are how you get a token)
	auth := v3.Group("/auth")
	auth.POST("/tokens", svc.AuthenticateToken)
	auth.GET("/tokens", svc.ValidateToken)
	auth.DELETE("/tokens", svc.RevokeToken)
	auth.GET("/projects", svc.ListAuthProjects)

	// S3/EC2 signature validation for Ceph RGW (rgw s3 auth use keystone) —
	// public + self-validating: the S3 signature is the credential (SPEC-014 FR-006).
	v3.POST("/s3tokens", svc.S3Tokens)
	v3.POST("/ec2tokens", svc.EC2Tokens)

	// All management routes require authentication
	mgmt := v3.Group("")
	mgmt.Use(authMiddleware)

	// Malformed UUID path ids (params named "id" or "*_id") get a Keystone-verbatim
	// 404 before the handler, on every management route EXCEPT the authz-gated ones
	// (user delete, password change, EC2 credentials). Those stay on `mgmt` and
	// validate the id in-handler AFTER their authz check, so 401/403 still precede
	// the 404 (cloud-storage#515).
	val := mgmt.Group("")
	val.Use(uuidParamMiddleware)

	// Users. Authz: reads and writes of the user
	// collection are admin-only; user-scoped subresources are self-or-admin.
	// Routes with an authz check sit on `mgmt` and validate ids in-handler so
	// 403 precedes the 404 (#515, asserted by TestIdentityAuthzPrecedence).
	val.GET("/users", svc.ListUsers)                                                            // admin-only (in-handler)
	val.POST("/users", svc.CreateUser)                                                          // admin-only (in-handler)
	mgmt.GET("/users/:id", svc.GetUser)                                                         // self-or-admin
	mgmt.PATCH("/users/:id", svc.UpdateUser)                                                    // self-or-admin, admin-only fields
	mgmt.DELETE("/users/:id", svc.DeleteUser)                                                   // authz-gated (admin)
	mgmt.POST("/users/:id/password", svc.ChangePassword)                                        // authz-gated (self-or-admin)
	mgmt.GET("/users/:id/projects", svc.GetUserProjects)                                        // self-or-admin
	mgmt.GET("/users/:id/groups", svc.GetUserGroups)                                            // self-or-admin
	mgmt.POST("/users/:id/application_credentials", svc.CreateApplicationCredential)            // self-or-admin
	mgmt.GET("/users/:id/application_credentials", svc.ListApplicationCredentials)              // self-or-admin
	mgmt.GET("/users/:id/application_credentials/:cred_id", svc.GetApplicationCredential)       // self-or-admin
	mgmt.DELETE("/users/:id/application_credentials/:cred_id", svc.DeleteApplicationCredential) // self-or-admin
	// EC2/S3 credentials — authz-gated (self-or-admin via denyIfNotSelfOrAdmin);
	// the :id (user) is validated in-handler after that check (SPEC-014 FR-005).
	mgmt.POST("/users/:id/credentials/OS-EC2", svc.CreateEC2Credential)
	mgmt.GET("/users/:id/credentials/OS-EC2", svc.ListEC2Credentials)
	mgmt.GET("/users/:id/credentials/OS-EC2/:access", svc.GetEC2Credential)
	mgmt.DELETE("/users/:id/credentials/OS-EC2/:access", svc.DeleteEC2Credential)

	// Projects (role assignments must come before /projects/:id to avoid conflicts).
	// Authz: project management and role assignment
	// are admin-only — the assignment pair is the privilege-escalation primitive.
	val.GET("/projects", svc.ListProjects)                                           // admin-only (in-handler)
	val.POST("/projects", svc.CreateProject)                                         // admin-only (in-handler)
	mgmt.PUT("/projects/:id/users/:user_id/roles/:role_id", svc.AssignRole)          // admin-only
	mgmt.DELETE("/projects/:id/users/:user_id/roles/:role_id", svc.UnassignRole)     // admin-only
	mgmt.GET("/projects/:id/users/:user_id/roles", svc.ListUserProjectRoles)         // admin-only
	mgmt.GET("/projects/:id/users/:user_id/roles/:role_id", svc.CheckRoleAssignment) // admin-only
	mgmt.GET("/projects/:id", svc.GetProject)                                        // admin-only
	mgmt.PATCH("/projects/:id", svc.UpdateProject)                                   // admin-only
	mgmt.DELETE("/projects/:id", svc.DeleteProject)                                  // admin-only

	// Groups — admin-only.
	val.GET("/groups", svc.ListGroups)   // admin-only (in-handler)
	val.POST("/groups", svc.CreateGroup) // admin-only (in-handler)
	mgmt.GET("/groups/:id", svc.GetGroup)
	mgmt.PATCH("/groups/:id", svc.UpdateGroup)
	mgmt.DELETE("/groups/:id", svc.DeleteGroup)
	mgmt.GET("/groups/:id/users", svc.ListGroupUsers)
	mgmt.PUT("/groups/:id/users/:user_id", svc.AddUserToGroup)
	mgmt.DELETE("/groups/:id/users/:user_id", svc.RemoveUserFromGroup)

	// Roles — admin-only.
	val.GET("/roles", svc.ListRoles)   // admin-only (in-handler)
	val.POST("/roles", svc.CreateRole) // admin-only (in-handler)
	mgmt.GET("/roles/:id", svc.GetRole)
	mgmt.PATCH("/roles/:id", svc.UpdateRole)
	mgmt.DELETE("/roles/:id", svc.DeleteRole)

	// Role Assignments — admin-only.
	val.GET("/role_assignments", svc.ListRoleAssignments) // admin-only (in-handler)

	// Domains.
	//
	// GET /domains/:id sits on `mgmt` rather than `val` so it can resolve the
	// `default` alias: the uuid middleware would answer 404 for it before the handler
	// ran. Standard clients resolve a `--domain` argument client-side by retrieving
	// the domain first, so without this the alias works only via a client fallback
	// that a future client version may drop (SPEC-022 FR-001a, A11).
	//
	// The exception is one route, one method, one alias. Every other non-identifier
	// value on it still gets #515's verbatim 404, produced in-handler; and PATCH,
	// DELETE and /config stay on `val`, where the middleware still answers.
	//
	// Domain management is admin-only.
	val.GET("/domains", svc.ListDomains)                 // admin-only (in-handler)
	val.POST("/domains", svc.CreateDomain)               // admin-only (in-handler)
	mgmt.GET("/domains/:id", svc.GetDomain)              // alias-aware (see above), admin-only
	mgmt.PATCH("/domains/:id", svc.UpdateDomain)         // admin-only
	mgmt.DELETE("/domains/:id", svc.DeleteDomain)        // admin-only
	mgmt.GET("/domains/:id/config", svc.GetDomainConfig) // admin-only

	// Services (catalog management) — admin-only.
	val.GET("/services", svc.ListServices)   // admin-only (in-handler)
	val.POST("/services", svc.CreateService) // admin-only (in-handler)
	mgmt.GET("/services/:id", svc.GetService)
	mgmt.PATCH("/services/:id", svc.UpdateService)
	mgmt.DELETE("/services/:id", svc.DeleteService)

	// Endpoints — admin-only.
	val.GET("/endpoints", svc.ListEndpoints)   // admin-only (in-handler)
	val.POST("/endpoints", svc.CreateEndpoint) // admin-only (in-handler)
	mgmt.DELETE("/endpoints/:id", svc.DeleteEndpoint)

	// Credentials
	val.GET("/credentials", svc.ListCredentials)
	val.POST("/credentials", svc.CreateCredential)
	val.GET("/credentials/:id", svc.GetCredential)
	val.PATCH("/credentials/:id", svc.UpdateCredential)
	val.DELETE("/credentials/:id", svc.DeleteCredential)

	// Application Credentials (lookup by ID without user_id)
	val.GET("/application_credentials/:id", svc.GetApplicationCredentialByID)
}

// GetVersion returns Keystone API version info
func (svc *Service) GetVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": gin.H{
			"id":     "v3.14",
			"status": "stable",
			"links": []gin.H{
				{
					"rel":  "self",
					"href": fmt.Sprintf("%s/v3", common.BaseURL(c, 35357)),
				},
			},
			"media-types": []gin.H{
				{
					"base": "application/json",
					"type": "application/vnd.openstack.identity-v3+json",
				},
			},
		},
	})
}

// unsupportedScope reports the refusal a scope warrants, or nil when the scope
// is one Lightstack supports (SPEC-023 FR-009 – FR-014).
//
// Two scope kinds exist and only two: project-scoped and explicitly unscoped. A
// top-level domain scope is refused because role_assignments binds a user to a
// role within a PROJECT, so there is no domain-level grant to read and nothing a
// domain-scoped token could authorize. Any other scope — a string other than
// "unscoped", or a kind such as system or OS-TRUST:trust — is refused too
// (FR-010, FR-011) rather than dropped.
//
// This is a function rather than an inline branch in AuthenticateToken because
// the SUPPORTED forms cannot be asserted through the route: they fall through to
// password or token authentication, which needs a live database. A test that has
// to reach one is not testing this decision. The refusals still go through the
// real route — they return before the fork and touch nothing.
//
// Placed here rather than in ScopeField.UnmarshalJSON because a decode error
// surfaces as "invalid request body: ...", which conflates an unsupported scope
// with malformed JSON and buries the reason.
//
// Note what is NOT resolved: the domain reference is never looked up. That is
// deliberate and is what makes the refusal identical for a domain that exists,
// one that does not, one the caller holds no role in, and a malformed identifier
// — so the answer discloses nothing about which domains exist (FR-014). It also
// means a malformed reference cannot reach a uuid-typed column and surface as a
// 500.
func unsupportedScope(scope *ScopeField) *common.OpenStackError {
	if scope == nil {
		return nil
	}
	if scope.Unsupported != nil {
		return common.NewBadRequestError(fmt.Sprintf(
			"scope %q is not supported; use a project scope or an unscoped token", *scope.Unsupported))
	}
	// scope.Domain is the TOP-LEVEL domain scope, emphatically not the
	// scope.project.domain qualifier every real client sends — that lives on
	// scope.Project and is untouched here.
	if scope.Domain == nil {
		return nil
	}
	if scope.Project != nil {
		return common.NewBadRequestError(
			"scope may name either a project or a domain, not both")
	}
	// 400, not 401: the credentials may be perfectly good. A caller must be able
	// to tell "what you asked for is unsupported" from "who you are is wrong"
	// (FR-013) — the previous behaviour was a 201 carrying neither the scope
	// asked for nor any roles, which told them nothing at all.
	return common.NewBadRequestError(
		"domain-scoped tokens are not supported; use a project scope or an unscoped token")
}

// AuthenticateToken handles token authentication (POST /v3/auth/tokens)
func (svc *Service) AuthenticateToken(c *gin.Context) {
	var req AuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Debug().Err(err).Str("path", c.Request.URL.Path).Msg("auth request parse failed")
		common.SendError(c, common.NewBadRequestError("invalid request body: "+err.Error()))
		return
	}

	// Refuse an unsupported scope ONCE, here, before the password/token fork
	// below (SPEC-023 FR-009 – FR-014).
	//
	// Placed before the fork rather than in each authenticate path because FR-016
	// requires the two to agree — one check satisfies that by construction, and
	// those paths already carry duplicated scope logic that has drifted once.
	if osErr := unsupportedScope(req.Auth.Scope); osErr != nil {
		common.SendError(c, osErr)
		return
	}

	// Determine authentication method
	var resp *AuthResponse
	var tokenString string
	var err error

	if req.Auth.Identity.Token != nil && req.Auth.Identity.Token.ID != "" {
		// Token-based authentication (re-scoping)
		resp, tokenString, err = svc.authService.AuthenticateToken(c.Request.Context(), &req, c.Request.Host)
	} else if req.Auth.Identity.Password != nil {
		// Password-based authentication
		resp, tokenString, err = svc.authService.AuthenticatePassword(c.Request.Context(), &req, c.Request.Host)
	} else {
		common.SendError(c, common.NewBadRequestError("password or token authentication required"))
		return
	}

	if err != nil {
		if osErr, ok := err.(*common.OpenStackError); ok {
			common.SendError(c, osErr)
			return
		}
		log.Error().Err(err).Str("operation", "authenticate").Msg("Authentication failed")
		common.SendError(c, common.NewInternalServerError("authentication failed"))
		return
	}

	// Return token in X-Subject-Token header
	c.Header("X-Subject-Token", tokenString)
	c.JSON(http.StatusCreated, resp)
}

// ValidateToken validates a token (GET /v3/auth/tokens)
func (svc *Service) ValidateToken(c *gin.Context) {
	tokenString := c.GetHeader("X-Subject-Token")
	if tokenString == "" {
		common.SendError(c, common.NewBadRequestError("X-Subject-Token header required"))
		return
	}

	claims, err := svc.authService.ValidateToken(tokenString)
	if err != nil {
		if osErr, ok := err.(*common.OpenStackError); ok {
			common.SendError(c, osErr)
			return
		}
		common.SendError(c, common.NewUnauthorizedError("invalid token"))
		return
	}

	// Return the standard Keystone v3 token envelope — the same shape as the
	// POST /v3/auth/tokens responses. Ceph RGW validates every user token via
	// this endpoint and parses the response with
	// rgw::keystone::TokenEnvelope::decode_json (src/rgw/rgw_keystone.cc),
	// whose mandatory fields are user{id,name}, expires_at, roles[]{name} and
	// project{id,name}; the old flat {"project_id", roles: [string]} shape was
	// unparseable by RGW and standard clients (gophercloud tokens.Get).
	c.JSON(http.StatusOK, svc.authService.BuildTokenResponse(c.Request.Context(), claims, c.Request.Host))
}

// RevokeToken revokes a token (DELETE /v3/auth/tokens)
func (svc *Service) RevokeToken(c *gin.Context) {
	// In JWT implementation, we don't maintain token blacklist
	// Tokens expire naturally based on TTL
	c.Status(http.StatusNoContent)
}

// ListAuthProjects lists projects available to the authenticated user (GET /v3/auth/projects)
func (svc *Service) ListAuthProjects(c *gin.Context) {
	// Extract user ID from token
	tokenString := c.GetHeader("X-Auth-Token")
	if tokenString == "" {
		tokenString = c.GetHeader("X-Subject-Token")
	}
	if tokenString == "" {
		common.SendError(c, common.NewUnauthorizedError("authentication required"))
		return
	}

	claims, err := svc.authService.ValidateToken(tokenString)
	if err != nil {
		common.SendError(c, common.NewUnauthorizedError("invalid token"))
		return
	}

	// Query projects the user has access to
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT DISTINCT p.id, p.name, p.description, p.enabled, p.domain_id, d.name as domain_name
		FROM projects p
		JOIN role_assignments ra ON ra.project_id = p.id
		LEFT JOIN domains d ON p.domain_id = d.id
		WHERE ra.user_id = $1 AND p.enabled = true
		ORDER BY p.name
	`, claims.UserID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_auth_projects").Str("user_id", claims.UserID).Msg("Failed to query auth projects")
		common.SendError(c, common.NewInternalServerError("failed to query projects"))
		return
	}
	defer rows.Close()

	var projects []gin.H
	for rows.Next() {
		var id, name string
		// Nullable columns, read as nullable for the reason ListProjects gives; the domain is
		// LEFT JOINed, so a project without one is listed rather than silently dropped.
		var description, domainID, domainName sql.NullString
		var enabled bool
		if err := rows.Scan(&id, &name, &description, &enabled, &domainID, &domainName); err != nil {
			log.Warn().Err(err).Str("operation", "ListAuthProjects").Msg("failed to scan row")
			continue
		}

		projects = append(projects, gin.H{
			"id":          id,
			"name":        name,
			"description": description.String,
			"enabled":     enabled,
			"domain_id":   domainID.String,
			"links":       identityLink(c, "projects", id),
			"domain": gin.H{
				"id":   domainID.String,
				"name": domainName.String,
			},
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if projects == nil {
		projects = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// ListUsers lists all users
func (svc *Service) ListUsers(c *gin.Context) {
	// Admin-only: the user collection exposes every
	// account's name, enabled state and domain — identity enumeration.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	var f listFilter
	f.eq(c, "name", "name")
	f.eqDomain(c, "domain_id", "domain_id")
	f.eqBool(c, "enabled", "enabled")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, name, enabled, domain_id FROM users"+f.Where()+" ORDER BY name", f.Args()...,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_users").Msg("Failed to query users")
		common.SendError(c, common.NewInternalServerError("failed to query users"))
		return
	}
	defer rows.Close()

	var users []gin.H
	for rows.Next() {
		var id, name, domainID string
		var enabled bool
		if err := rows.Scan(&id, &name, &enabled, &domainID); err != nil {
			log.Error().Err(err).Str("operation", "list_users_scan").Msg("Failed to scan user row")
			common.SendError(c, common.NewInternalServerError("failed to read users"))
			return
		}
		users = append(users, gin.H{
			"id":        id,
			"name":      name,
			"enabled":   enabled,
			"domain_id": domainID,
			"links":     identityLink(c, "users", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"users": users})
}

// GetUser returns a single user
func (svc *Service) GetUser(c *gin.Context) {
	userID := c.Param("id")

	// Self-or-admin: user records (name, email,
	// enabled) are only for the user itself or an admin.
	userID, denied := denyIfNotSelfOrAdmin(c, userID)
	if denied {
		return
	}

	var id, name, domainID string
	var enabled bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, name, enabled, domain_id FROM users WHERE id = $1",
		userID,
	).Scan(&id, &name, &enabled, &domainID)

	if err != nil {
		notFoundOr500(c, err, "user", userID)
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": gin.H{
		"id":        id,
		"name":      name,
		"enabled":   enabled,
		"domain_id": domainID,
		"links":     identityLink(c, "users", id),
	}})
}

// ListProjects lists all projects
func (svc *Service) ListProjects(c *gin.Context) {
	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}

	var f listFilter
	f.eq(c, "name", "name")
	f.eqDomain(c, "domain_id", "domain_id")
	// There is no project hierarchy, so every project is top-level, and upstream
	// gives a top-level project its domain as parent (keystone/resource/core.py).
	// A parent is therefore a domain; a project id names no parent and matches
	// nothing.
	f.eqDomain(c, "parent_id", "domain_id")
	f.eqBool(c, "enabled", "enabled")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, name, description, enabled, domain_id FROM projects"+f.Where()+" ORDER BY name", f.Args()...,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_projects").Msg("Failed to query projects")
		common.SendError(c, common.NewInternalServerError("failed to query projects"))
		return
	}
	defer rows.Close()

	var projects []gin.H
	for rows.Next() {
		var id, name string
		// description and domain_id are NULLABLE in the schema, so scanning them
		// into plain strings turns a legitimate row into a 500 for the whole
		// collection — one project without a domain made GET /v3/projects fail
		// entirely. Read them as nullable and render the zero value.
		var description, domainID sql.NullString
		var enabled bool
		if err := rows.Scan(&id, &name, &description, &enabled, &domainID); err != nil {
			log.Error().Err(err).Str("operation", "list_projects_scan").Msg("Failed to scan project row")
			common.SendError(c, common.NewInternalServerError("failed to read projects"))
			return
		}
		// .String, not the NullString itself — marshalling the struct would emit
		// {"String":"","Valid":false} and change the response shape for every
		// client.
		projects = append(projects, gin.H{
			"id":          id,
			"name":        name,
			"description": description.String,
			"enabled":     enabled,
			"domain_id":   domainID.String,
			"links":       identityLink(c, "projects", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// identityLink builds the links object every identity resource carries.
//
// python-openstackclient 6.x does an unconditional _info.pop('links') before
// rendering, so a resource without this field aborts the command with a bare 'links'
// and exit 1 — that is cloud-storage#547 B16, and it hits `project show`, `user show`
// and `role show`. A 10.x client tolerates the omission, so whether Lightstack looks
// functional depended on which client was first on PATH.
//
// Routed through common.BaseURL rather than concatenated by hand: several of these
// links already existed but were built as `c.Request.Host + "/v3/..."`, which has no
// scheme and is therefore not a resolvable URL (SPEC-025 FR-007, FR-012). One helper
// so the rule cannot drift between the eleven places that need it.
func identityLink(c *gin.Context, collection, id string) gin.H {
	return gin.H{"self": common.BaseURL(c, 35357) + "/v3/" + collection + "/" + id}
}

// GetProject returns a single project
func (svc *Service) GetProject(c *gin.Context) {
	projectID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) {
		return
	}

	var id, name string
	var description, domainID sql.NullString // nullable, as in ListProjects
	var enabled bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, name, description, enabled, domain_id FROM projects WHERE id = $1",
		projectID,
	).Scan(&id, &name, &description, &enabled, &domainID)

	if err != nil {
		notFoundOr500(c, err, "project", projectID)
		return
	}

	c.JSON(http.StatusOK, gin.H{"project": gin.H{
		"id":          id,
		"name":        name,
		"description": description.String,
		"enabled":     enabled,
		"domain_id":   domainID.String,
		"links":       identityLink(c, "projects", id),
	}})
}

// CreateProject handles POST /v3/projects
func (svc *Service) CreateProject(c *gin.Context) {
	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}

	var req struct {
		Project struct {
			Name        string `json:"name" binding:"required"`
			Description string `json:"description"`
			DomainID    string `json:"domain_id"`
			Enabled     *bool  `json:"enabled"`
		} `json:"project" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Resolve the domain reference before persisting anything, so a request naming a
	// domain that does not exist is refused rather than half-applied. An omitted
	// reference and the `default` alias both resolve the Default domain by name; a
	// UUID resolves directly. Passing the raw value into the `uuid`-typed domain_id
	// column is what used to make `domain_id: "default"` — what every OpenStack client
	// sends — a generic 500 (cloud-storage#545 B5/P1, FR-001..FR-003).
	domainID, outcome := resolveDomainRef(c.Request.Context(), svc.pool, req.Project.DomainID, enabledDomainsOnly)
	switch outcome {
	case domainNotFound:
		common.SendError(c, common.NewBadRequestError("specified domain not found or disabled"))
		return
	case domainQueryFailed:
		common.SendError(c, common.NewInternalServerError("failed to resolve domain"))
		return
	}

	// Default enabled to true if not specified
	enabled := true
	if req.Project.Enabled != nil {
		enabled = *req.Project.Enabled
	}

	// Generate new project ID
	projectID := uuid.New().String()
	now := time.Now()

	// Insert into database
	_, err := svc.pool.Exec(c.Request.Context(),
		`INSERT INTO projects (id, name, description, domain_id, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		projectID, req.Project.Name, req.Project.Description, domainID, enabled, now, now,
	)
	if err != nil {
		// projects.name is UNIQUE (migration 001), and the violation used to surface as
		// a generic 500. Report the collision for what it is, the way CreateUser
		// already does for users_name_key. Detected by SQLSTATE rather than by matching
		// the error text (FR-004).
		if database.IsUniqueViolation(err) {
			common.SendError(c, common.NewConflictError("project with name '"+req.Project.Name+"' already exists"))
			return
		}
		log.Error().Err(err).Str("operation", "create_project").Msg("Failed to create project")
		common.SendError(c, common.NewInternalServerError("failed to create project"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{"project": gin.H{
		"id":          projectID,
		"name":        req.Project.Name,
		"description": req.Project.Description,
		"domain_id":   domainID,
		"enabled":     enabled,
		"links":       identityLink(c, "projects", projectID),
	}})
}

// UpdateProject handles PATCH /v3/projects/:id
func (svc *Service) UpdateProject(c *gin.Context) {
	projectID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) {
		return
	}

	var req struct {
		Project struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Enabled     *bool   `json:"enabled"`
		} `json:"project" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build dynamic update query
	u := &common.SQLUpdate{}

	if req.Project.Name != nil {
		u.Set("name", *req.Project.Name)
	}
	if req.Project.Description != nil {
		u.Set("description", *req.Project.Description)
	}
	if req.Project.Enabled != nil {
		u.Set("enabled", *req.Project.Enabled)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	u.SetExpr("updated_at = NOW()")

	// One statement, not an UPDATE followed by a SELECT: the UPDATE both resolves
	// existence and hands back the row it wrote, so there is no window in which a
	// concurrent DELETE can make a successful PATCH look like a 404. RETURNING lists
	// every column the response needs, because a partial PATCH must still echo the
	// columns it did not touch (upstream Keystone returns the whole project). A miss
	// is database.ErrNoRows, which notFoundOr500 maps to the verbatim 404.
	query := "UPDATE projects SET " + u.Assignments() +
		" WHERE id = " + u.Arg(projectID) +
		" RETURNING name, description, domain_id, enabled"

	var name string
	var description, domainID sql.NullString // nullable, as in ListProjects
	var enabled bool
	err := svc.pool.QueryRow(c.Request.Context(), query, u.Args()...).
		Scan(&name, &description, &domainID, &enabled)
	if err != nil {
		notFoundOr500(c, err, "project", projectID)
		return
	}

	c.JSON(http.StatusOK, gin.H{"project": gin.H{
		"id":          projectID,
		"name":        name,
		"description": description.String,
		"domain_id":   domainID.String,
		"enabled":     enabled,
		"links":       identityLink(c, "projects", projectID),
	}})
}

// DeleteProject handles DELETE /v3/projects/:id
func (svc *Service) DeleteProject(c *gin.Context) {
	projectID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM projects WHERE id = $1",
		projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_project").Str("project_id", projectID).Msg("Failed to delete project")
		common.SendError(c, common.NewInternalServerError("failed to delete project"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("project", projectID))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListRoles lists all roles
func (svc *Service) ListRoles(c *gin.Context) {
	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}

	var f listFilter
	f.eq(c, "name", "name")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, name FROM roles"+f.Where()+" ORDER BY name", f.Args()...,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_roles").Msg("Failed to query roles")
		common.SendError(c, common.NewInternalServerError("failed to query roles"))
		return
	}
	defer rows.Close()

	var roles []gin.H
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Error().Err(err).Str("operation", "list_roles_scan").Msg("Failed to scan role row")
			common.SendError(c, common.NewInternalServerError("failed to read roles"))
			return
		}
		roles = append(roles, gin.H{
			"id":    id,
			"name":  name,
			"links": identityLink(c, "roles", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

// GetRole handles GET /v3/roles/:id
func (svc *Service) GetRole(c *gin.Context) {
	roleID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "role", roleID) {
		return
	}

	var id, name string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, name FROM roles WHERE id = $1",
		roleID,
	).Scan(&id, &name)

	if err != nil {
		notFoundOr500(c, err, "role", roleID)
		return
	}

	c.JSON(http.StatusOK, gin.H{"role": gin.H{
		"id":    id,
		"name":  name,
		"links": identityLink(c, "roles", id),
	}})
}

// CreateRole handles POST /v3/roles
func (svc *Service) CreateRole(c *gin.Context) {
	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}

	var req struct {
		Role struct {
			Name     string `json:"name" binding:"required"`
			DomainID string `json:"domain_id"`
		} `json:"role" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Generate new role ID
	roleID := uuid.New().String()
	now := time.Now()

	// Insert into database (domain_id support can be added later via migration)
	_, err := svc.pool.Exec(c.Request.Context(),
		`INSERT INTO roles (id, name, created_at)
		 VALUES ($1, $2, $3)`,
		roleID, req.Role.Name, now,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_role").Msg("Failed to create role")
		common.SendError(c, common.NewInternalServerError("failed to create role"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{"role": gin.H{
		"id":    roleID,
		"name":  req.Role.Name,
		"links": identityLink(c, "roles", roleID),
	}})
}

// UpdateRole handles PATCH /v3/roles/:id
func (svc *Service) UpdateRole(c *gin.Context) {
	roleID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "role", roleID) {
		return
	}

	var req struct {
		Role struct {
			Name *string `json:"name"`
		} `json:"role" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build dynamic update query
	u := &common.SQLUpdate{}

	if req.Role.Name != nil {
		u.Set("name", *req.Role.Name)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	// Single statement for the same reason as UpdateProject: the UPDATE resolves
	// existence and returns the row it wrote, so no write-then-read window remains.
	query := "UPDATE roles SET " + u.Assignments() + " WHERE id = " + u.Arg(roleID) + " RETURNING name"

	var name string
	err := svc.pool.QueryRow(c.Request.Context(), query, u.Args()...).Scan(&name)
	if err != nil {
		notFoundOr500(c, err, "role", roleID)
		return
	}

	c.JSON(http.StatusOK, gin.H{"role": gin.H{
		"id":    roleID,
		"name":  name,
		"links": identityLink(c, "roles", roleID),
	}})
}

// DeleteRole handles DELETE /v3/roles/:id
func (svc *Service) DeleteRole(c *gin.Context) {
	roleID := c.Param("id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "role", roleID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM roles WHERE id = $1",
		roleID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_role").Str("role_id", roleID).Msg("Failed to delete role")
		common.SendError(c, common.NewInternalServerError("failed to delete role"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("role", roleID))
		return
	}

	c.Status(http.StatusNoContent)
}

// AssignRole handles PUT /v3/projects/:id/users/:user_id/roles/:role_id
func (svc *Service) AssignRole(c *gin.Context) {
	projectID := c.Param("id")
	userID := c.Param("user_id")
	roleID := c.Param("role_id")

	// Admin-only: role assignment is the privilege
	// escalation primitive — without this check any authenticated user could
	// grant itself (or anyone) the admin role on any project.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) || denyIfMalformedID(c, "user", userID) ||
		denyIfMalformedID(c, "role", roleID) {
		return
	}

	// The foreign keys prevent dangling assignments; these lookups preserve
	// Keystone's resource-specific 404 instead of leaking a database error.
	var projectExists, userExists, roleExists bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1),
		       EXISTS(SELECT 1 FROM users WHERE id = $2),
		       EXISTS(SELECT 1 FROM roles WHERE id = $3)
	`, projectID, userID, roleID).Scan(&projectExists, &userExists, &roleExists)
	for _, tc := range []struct {
		kind, id string
		exists   bool
	}{
		{"project", projectID, projectExists},
		{"user", userID, userExists},
		{"role", roleID, roleExists},
	} {
		if existsOr404(c, err, tc.exists, tc.kind, tc.id) {
			return
		}
	}

	// Insert role assignment (idempotent)
	_, err = svc.pool.Exec(c.Request.Context(),
		`INSERT INTO role_assignments (id, user_id, project_id, role_id, created_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, NOW())
		 ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		userID, projectID, roleID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "assign_role").Str("project_id", projectID).Str("user_id", userID).Str("role_id", roleID).Msg("Failed to assign role")
		common.SendError(c, common.NewInternalServerError("failed to assign role"))
		return
	}

	c.Status(http.StatusNoContent)
}

// UnassignRole handles DELETE /v3/projects/:id/users/:user_id/roles/:role_id
func (svc *Service) UnassignRole(c *gin.Context) {
	projectID := c.Param("id")
	userID := c.Param("user_id")
	roleID := c.Param("role_id")

	// Admin-only: revoking roles is the inverse
	// escalation — any user could strip an admin's role and lock the
	// deployment out.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	if denyIfMalformedID(c, "project", projectID) || denyIfMalformedID(c, "user", userID) ||
		denyIfMalformedID(c, "role", roleID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM role_assignments WHERE user_id = $1 AND project_id = $2 AND role_id = $3",
		userID, projectID, roleID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "unassign_role").Msg("Failed to unassign role")
		common.SendError(c, common.NewInternalServerError("failed to unassign role"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewRoleAssignmentNotFoundError(roleID, userID, projectID))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListUserProjectRoles handles GET /v3/projects/:id/users/:user_id/roles
func (svc *Service) ListUserProjectRoles(c *gin.Context) {
	projectID := c.Param("id")
	userID := c.Param("user_id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) || denyIfMalformedID(c, "user", userID) {
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT r.id, r.name
		 FROM roles r
		 INNER JOIN role_assignments ra ON r.id = ra.role_id
		 WHERE ra.user_id = $1 AND ra.project_id = $2`,
		userID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_user_project_roles").Msg("Failed to query user project roles")
		common.SendError(c, common.NewInternalServerError("failed to query roles"))
		return
	}
	defer rows.Close()

	var roles []gin.H
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Warn().Err(err).Str("operation", "ListUserProjectRoles").Msg("failed to scan row")
			continue
		}
		roles = append(roles, gin.H{
			"id":    id,
			"name":  name,
			"links": identityLink(c, "roles", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if roles == nil {
		roles = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

// CheckRoleAssignment handles GET /v3/projects/:id/users/:user_id/roles/:role_id
func (svc *Service) CheckRoleAssignment(c *gin.Context) {
	projectID := c.Param("id")
	userID := c.Param("user_id")
	roleID := c.Param("role_id")

	// Admin-only: the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "project", projectID) || denyIfMalformedID(c, "user", userID) || denyIfMalformedID(c, "role", roleID) {
		return
	}

	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT EXISTS(
			SELECT 1 FROM role_assignments
			WHERE user_id = $1 AND project_id = $2 AND role_id = $3
		)`,
		userID, projectID, roleID,
	).Scan(&exists)

	if err != nil {
		log.Error().Err(err).Str("operation", "check_role_assignment").Msg("Failed to check role assignment")
		common.SendError(c, common.NewInternalServerError("failed to check role assignment"))
		return
	}

	if !exists {
		common.SendError(c, common.NewRoleAssignmentNotFoundError(roleID, userID, projectID))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListRoleAssignments handles GET /v3/role_assignments
func (svc *Service) ListRoleAssignments(c *gin.Context) {
	// Admin-only: role assignments grant authority and expose
	// the identity management surface.
	if !common.RequireAdmin(c, "keystone:identity-management") {
		return
	}

	// Canonical UUID filters preserve alternate spellings on both database backends.
	var f listFilter
	f.eqID(c, "user.id", "ra.user_id")
	f.eqID(c, "scope.project.id", "ra.project_id")
	f.eqID(c, "role.id", "ra.role_id")

	query := `SELECT ra.user_id, ra.project_id, ra.role_id, r.name as role_name
	          FROM role_assignments ra
	          INNER JOIN roles r ON ra.role_id = r.id` + f.Where()

	rows, err := svc.pool.Query(c.Request.Context(), query, f.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_role_assignments").Msg("Failed to query role assignments")
		common.SendError(c, common.NewInternalServerError("failed to query role assignments"))
		return
	}
	defer rows.Close()

	var assignments []gin.H
	for rows.Next() {
		var uid, pid, rid, rname string
		if err := rows.Scan(&uid, &pid, &rid, &rname); err != nil {
			log.Warn().Err(err).Msg("failed to scan role assignment row")
			continue
		}
		assignments = append(assignments, gin.H{
			"user": gin.H{
				"id": uid,
			},
			"scope": gin.H{
				"project": gin.H{
					"id": pid,
				},
			},
			"role": gin.H{
				"id":   rid,
				"name": rname,
			},
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if assignments == nil {
		assignments = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"role_assignments": assignments})
}

// CreateUser creates a new user (POST /v3/users)
func (svc *Service) CreateUser(c *gin.Context) {
	// Only admins may create users
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("admin role required to create users"))
		return
	}

	var req struct {
		User struct {
			Name        string `json:"name" binding:"required"`
			Password    string `json:"password"`
			Email       string `json:"email"`
			Description string `json:"description"`
			Enabled     *bool  `json:"enabled"`
			DomainID    string `json:"domain_id"`
		} `json:"user" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body: "+err.Error()))
		return
	}

	// Default enabled to true
	enabled := true
	if req.User.Enabled != nil {
		enabled = *req.User.Enabled
	}

	// Resolve the domain reference through the shared resolver, so this handler and
	// the project/group ones cannot disagree about what a valid domain is (FR-008).
	// The behaviour change here is narrow: `domain_id: "default"` — the alias every
	// OpenStack client sends — now resolves instead of being rejected. An unknown or
	// disabled domain keeps its existing 400 and its existing message text, and a
	// failed lookup is a 500 whichever domain was named (FR-002, FR-005).
	domainID, outcome := resolveDomainRef(c.Request.Context(), svc.pool, req.User.DomainID, enabledDomainsOnly)
	switch outcome {
	case domainNotFound:
		common.SendError(c, common.NewBadRequestError("specified domain not found or disabled"))
		return
	case domainQueryFailed:
		common.SendError(c, common.NewInternalServerError("failed to resolve domain"))
		return
	}

	// Hash password if provided
	var passwordHash string
	if req.User.Password != "" {
		hashedBytes, err := svc.authService.HashPassword(req.User.Password)
		if err != nil {
			log.Error().Err(err).Str("operation", "create_user_hash_password").Msg("Failed to hash password")
			common.SendError(c, common.NewInternalServerError("failed to hash password"))
			return
		}
		passwordHash = string(hashedBytes)
	}

	// Insert user into database
	var userID, userName, userEmail, userDescription string
	var userEnabled bool
	var userDomainID *string

	err := svc.pool.QueryRow(c.Request.Context(),
		`INSERT INTO users (name, password_hash, email, description, enabled, domain_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, name, email, description, enabled, domain_id`,
		req.User.Name, passwordHash, req.User.Email, req.User.Description, enabled, domainID,
	).Scan(&userID, &userName, &userEmail, &userDescription, &userEnabled, &userDomainID)

	if err != nil {
		if database.IsUniqueViolation(err) {
			common.SendError(c, common.NewConflictError("user with name '"+req.User.Name+"' already exists"))
			return
		}
		log.Error().Err(err).Str("operation", "create_user").Msg("Failed to create user")
		common.SendError(c, common.NewInternalServerError("failed to create user"))
		return
	}

	// Return created user
	responseDomainID := "default"
	if userDomainID != nil {
		responseDomainID = *userDomainID
	}

	c.JSON(http.StatusCreated, gin.H{
		"user": gin.H{
			"id":          userID,
			"name":        userName,
			"email":       userEmail,
			"description": userDescription,
			"enabled":     userEnabled,
			"domain_id":   responseDomainID,
		},
	})
}

// UpdateUser updates an existing user (PATCH /v3/users/:id)
func (svc *Service) UpdateUser(c *gin.Context) {
	userID := c.Param("id")

	// Self-or-admin: without this check any
	// authenticated caller could reset any user's password — including the
	// admin's — or disable any account, because this handler (unlike
	// ChangePassword) never asked for the original password.
	isAdmin := common.HasRole(c, common.RoleAdmin)
	if !sameUser(c.GetString("user_id"), userID) && !isAdmin {
		common.SendError(c, common.NewForbiddenError("cannot update another user"))
		return
	}
	// Authorization passed; validate the id after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "user", userID) {
		return
	}

	var req struct {
		User struct {
			Name        *string `json:"name"`
			Email       *string `json:"email"`
			Description *string `json:"description"`
			Enabled     *bool   `json:"enabled"`
			Password    *string `json:"password"`
		} `json:"user" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body: "+err.Error()))
		return
	}

	// A non-admin may only edit their own profile fields. Setting a password
	// without the original (ChangePassword's contract) or flipping `enabled`
	// stays admin-only — otherwise this route would be a weaker password
	// change and a self-enable.
	if !isAdmin && (req.User.Password != nil || req.User.Enabled != nil) {
		common.SendError(c, common.NewForbiddenError("admin role required to set password or enabled"))
		return
	}

	// Check if user exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)",
		userID,
	).Scan(&exists)

	if existsOr404(c, err, exists, "user", userID) {
		return
	}

	// Build dynamic UPDATE query
	u := &common.SQLUpdate{}

	if req.User.Name != nil {
		u.Set("name", *req.User.Name)
	}
	if req.User.Email != nil {
		u.Set("email", *req.User.Email)
	}
	if req.User.Description != nil {
		u.Set("description", *req.User.Description)
	}
	if req.User.Enabled != nil {
		u.Set("enabled", *req.User.Enabled)
	}
	if req.User.Password != nil {
		hashedBytes, err := svc.authService.HashPassword(*req.User.Password)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_user_hash_password").Msg("Failed to hash password")
			common.SendError(c, common.NewInternalServerError("failed to hash password"))
			return
		}
		u.Set("password_hash", string(hashedBytes))
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	// Add updated_at timestamp
	u.SetExpr("updated_at = NOW()")

	// Execute update
	query := "UPDATE users SET " + u.Assignments() + " WHERE id = " + u.Arg(userID)
	_, err = svc.pool.Exec(c.Request.Context(), query, u.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_user").Str("user_id", userID).Msg("Failed to update user")
		common.SendError(c, common.NewInternalServerError("failed to update user"))
		return
	}

	// Fetch updated user
	var id, name, email, description string
	var enabled bool
	var domainID *string

	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, name, email, description, enabled, domain_id FROM users WHERE id = $1",
		userID,
	).Scan(&id, &name, &email, &description, &enabled, &domainID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_user_fetch").Str("user_id", userID).Msg("Failed to fetch updated user")
		common.SendError(c, common.NewInternalServerError("failed to fetch updated user"))
		return
	}

	responseDomainID := "default"
	if domainID != nil {
		responseDomainID = *domainID
	}

	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id":          id,
			"name":        name,
			"email":       email,
			"description": description,
			"enabled":     enabled,
			"domain_id":   responseDomainID,
		},
	})
}

// DeleteUser deletes a user (DELETE /v3/users/:id)
func (svc *Service) DeleteUser(c *gin.Context) {
	userID := c.Param("id")

	// Canonicalise a well-formed id so the delete matches on SQLite's text ids; a
	// malformed id is left for the uuid.Validate guard below to 404 after the authz check.
	if parsed, err := uuid.Parse(userID); err == nil {
		userID = parsed.String()
	}

	// Only admins may delete users
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("admin role required to delete users"))
		return
	}

	// Authorization passed; validate the id after it so 403 precedes this 404 (#515).
	if denyIfMalformedID(c, "user", userID) {
		return
	}

	// Execute delete
	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM users WHERE id = $1",
		userID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_user").Str("user_id", userID).Msg("Failed to delete user")
		common.SendError(c, common.NewInternalServerError("failed to delete user"))
		return
	}

	// Check if user was actually deleted
	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("user", userID))
		return
	}

	c.Status(http.StatusNoContent)
}

// ChangePassword changes a user's password (POST /v3/users/:id/password)
func (svc *Service) ChangePassword(c *gin.Context) {
	userID := c.Param("id")

	// Canonicalise a well-formed id up front so the self-check compares like with like
	// (the token's user_id is canonical) and the queries match on SQLite's text ids. A
	// malformed id is left as-is: it can't match the caller's own id, so an unauthorised
	// caller still gets 403 before the 404 below (#515).
	if parsed, err := uuid.Parse(userID); err == nil {
		userID = parsed.String()
	}

	// Users may only change their own password; admins may change any user's password
	callerID := c.GetString("user_id")
	if !sameUser(callerID, userID) && !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("cannot change another user's password"))
		return
	}

	// Authorization passed; validate the id after it so 403 precedes this 404 (#515).
	if denyIfMalformedID(c, "user", userID) {
		return
	}

	var req struct {
		User struct {
			OriginalPassword string `json:"original_password" binding:"required"`
			Password         string `json:"password" binding:"required"`
		} `json:"user" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body: "+err.Error()))
		return
	}

	// Fetch current password hash
	var currentPasswordHash string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT password_hash FROM users WHERE id = $1",
		userID,
	).Scan(&currentPasswordHash)

	if err != nil {
		notFoundOr500(c, err, "user", userID)
		return
	}

	// Verify original password
	if !svc.authService.CheckPassword(req.User.OriginalPassword, currentPasswordHash) {
		common.SendError(c, common.NewUnauthorizedError("original password is incorrect"))
		return
	}

	// Hash new password
	hashedBytes, err := svc.authService.HashPassword(req.User.Password)
	if err != nil {
		log.Error().Err(err).Str("operation", "change_password_hash").Msg("Failed to hash password")
		common.SendError(c, common.NewInternalServerError("failed to hash password"))
		return
	}

	// Update password
	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2",
		string(hashedBytes), userID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "change_password_update").Str("user_id", userID).Msg("Failed to update password")
		common.SendError(c, common.NewInternalServerError("failed to update password"))
		return
	}

	c.Status(http.StatusNoContent)
}

// GetUserProjects returns projects for a user (GET /v3/users/:id/projects)
func (svc *Service) GetUserProjects(c *gin.Context) {
	userID := c.Param("id")

	// Self-or-admin: a user's project membership is
	// footprint information.
	userID, denied := denyIfNotSelfOrAdmin(c, userID)
	if denied {
		return
	}

	// Check if user exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)",
		userID,
	).Scan(&exists)

	if existsOr404(c, err, exists, "user", userID) {
		return
	}

	// Fetch projects via role_assignments
	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT DISTINCT p.id, p.name, p.description, p.enabled, p.domain_id
		 FROM projects p
		 INNER JOIN role_assignments ra ON ra.project_id = p.id
		 WHERE ra.user_id = $1
		 ORDER BY p.name`,
		userID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "get_user_projects").Str("user_id", userID).Msg("Failed to fetch user projects")
		common.SendError(c, common.NewInternalServerError("failed to fetch user projects"))
		return
	}
	defer rows.Close()

	var projects []gin.H
	for rows.Next() {
		var id, name string
		var description, domainID sql.NullString // nullable, as in ListProjects
		var enabled bool
		if err := rows.Scan(&id, &name, &description, &enabled, &domainID); err != nil {
			log.Error().Err(err).Str("operation", "get_user_projects_scan").Msg("Failed to scan project row")
			common.SendError(c, common.NewInternalServerError("failed to read project data"))
			return
		}
		projects = append(projects, gin.H{
			"id":          id,
			"name":        name,
			"description": description.String,
			"enabled":     enabled,
			"domain_id":   domainID.String,
			"links":       identityLink(c, "projects", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// GetUserGroups returns groups for a user (GET /v3/users/:id/groups)
func (svc *Service) GetUserGroups(c *gin.Context) {
	userID := c.Param("id")

	// Self-or-admin.
	userID, denied := denyIfNotSelfOrAdmin(c, userID)
	if denied {
		return
	}

	// Check if user exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)",
		userID,
	).Scan(&exists)

	if existsOr404(c, err, exists, "user", userID) {
		return
	}

	// Fetch groups via group_members
	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT g.id, g.name, g.description, g.domain_id
		 FROM groups g
		 INNER JOIN group_members gm ON gm.group_id = g.id
		 WHERE gm.user_id = $1
		 ORDER BY g.name`,
		userID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "get_user_groups").Str("user_id", userID).Msg("Failed to fetch user groups")
		common.SendError(c, common.NewInternalServerError("failed to fetch user groups"))
		return
	}
	defer rows.Close()

	var groups []gin.H
	for rows.Next() {
		var id, name, description, domainID string
		if err := rows.Scan(&id, &name, &description, &domainID); err != nil {
			log.Error().Err(err).Str("operation", "get_user_groups_scan").Msg("Failed to scan group row")
			common.SendError(c, common.NewInternalServerError("failed to read group data"))
			return
		}
		groups = append(groups, gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"domain_id":   domainID,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"groups": groups})
}
