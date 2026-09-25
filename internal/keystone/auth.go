package keystone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// TokenClaims represents JWT token claims
type TokenClaims struct {
	UserID    string   `json:"user_id"`
	UserName  string   `json:"user_name"`
	ProjectID string   `json:"project_id,omitempty"`
	Roles     []string `json:"roles,omitempty"`
	jwt.RegisteredClaims
}

// AuthService handles authentication operations
type AuthService struct {
	jwtSecret        []byte
	tokenTTL         time.Duration
	cache            *cache.Cache
	glanceS3Endpoint string // resolved cfg.Glance.S3Endpoint, advertised as the `s3` catalog endpoint
	// rgwCompatTenantIDs is the resolved keystone.rgw_compat_tenant_ids flag:
	// render token.project.id with '-' → '_' in the token payload (Ceph RGW
	// compatibility). See SetRGWCompatTenantIDs.
	rgwCompatTenantIDs bool
}

// NewAuthService creates a new auth service
func NewAuthService(jwtSecret string, tokenTTL time.Duration, cacheInstance *cache.Cache) *AuthService {
	return &AuthService{
		jwtSecret: []byte(jwtSecret),
		tokenTTL:  tokenTTL,
		cache:     cacheInstance,
	}
}

// SetGlanceS3Endpoint wires the resolved Glance S3/RGW endpoint (cfg.Glance.S3Endpoint,
// which the config layer resolves from YAML or LIGHTSTACK_GLANCE_S3_ENDPOINT) so the service
// catalog advertises the real external RGW rather than a placeholder. Set once at
// startup, mirroring nova.SetImageStore.
func (s *AuthService) SetGlanceS3Endpoint(url string) { s.glanceS3Endpoint = url }

// SetRGWCompatTenantIDs wires keystone.rgw_compat_tenant_ids (YAML or
// LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS, resolved by the config layer) so
// the token payload renders project ids in Ceph RGW's tenant-name-safe form.
// Set once at startup, mirroring SetGlanceS3Endpoint. When false (the default),
// token.project.id keeps the canonical hyphenated UUID.
func (s *AuthService) SetRGWCompatTenantIDs(enabled bool) { s.rgwCompatTenantIDs = enabled }

// rgwCompatTenantID renders a project id in Ceph RGW's tenant-name-safe form:
// every '-' becomes '_'. Ceph validates tenant names with
// rgw_validate_tenant_name (src/rgw/rgw_user.cc, identical in the squid and
// main branches), which accepts [A-Za-z0-9_] only — hyphens are rejected with
// ERR_INVALID_TENANT_NAME. Under rgw_keystone_implicit_tenants, RGW derives the
// Swift tenant name and the implicit user id (<project>$<project>) from the
// Keystone token's project id, so a hyphenated UUID project id cannot back an
// RGW tenant. Injective per id (distinct UUIDs stay distinct).
func rgwCompatTenantID(id string) string { return strings.ReplaceAll(id, "-", "_") }

// rgwReverseTenantID is the inverse of rgwCompatTenantID ('_' → '-'); used
// by fromAdvertisedProjectID to canonicalize ids clients copied out of a
// token envelope. See that helper for the UUID-shape guards.
func rgwReverseTenantID(id string) string { return strings.ReplaceAll(id, "_", "-") }

// tokenProjectID renders the project id for a token payload: the canonical
// (hyphenated) UUID, or the RGW-safe form when keystone.rgw_compat_tenant_ids
// is enabled. Only the token-payload builders in this file (password issue,
// token re-scope, and the validation envelope built by BuildTokenResponse for
// GET /v3/auth/tokens) render through this; BuildServiceCatalog, the
// management APIs and everything else keep the canonical id.
func (s *AuthService) tokenProjectID(id string) string {
	if s == nil || !s.rgwCompatTenantIDs {
		return id
	}
	return rgwCompatTenantID(id)
}

// fromAdvertisedProjectID maps a project id that a client copied out of a
// token envelope back to its canonical form. While rgw_compat_tenant_ids is
// on, the envelope advertises token.project.id with every '-' rendered as
// '_' (rgwCompatTenantID); clients that feed that value back — a token
// re-scope with scope.project.id, or an EC2 credential minted with it as
// tenant_id — would otherwise miss the canonical row and fail with a
// confusing 401/404. The value is canonicalized only when the flag is on,
// it contains an underscore, it is not itself a valid UUID (canonical ids
// never contain underscores), and reversing '_' → '-' yields a valid UUID —
// the exact inverse of rgwCompatTenantID over UUID-shaped ids, so a genuine
// project id that merely contains underscores passes through untouched.
// Nil-safe: keystone.Service is constructed without an AuthService in some
// test wirings (NewService(nil, nil)).
func (s *AuthService) fromAdvertisedProjectID(id string) string {
	if s == nil || !s.rgwCompatTenantIDs || !strings.Contains(id, "_") {
		return id
	}
	if _, err := uuid.Parse(id); err == nil {
		return id
	}
	reversed := rgwReverseTenantID(id)
	if _, err := uuid.Parse(reversed); err == nil {
		return reversed
	}
	return id
}

// keystoneTimestamp renders a token timestamp (expires_at / issued_at) in the
// exact form Ceph RGW's keystone TokenEnvelope decoder accepts: RFC3339 with
// a "Z" (UTC) tail. Go formats time.Time in the process-local zone, so a
// deployment with TZ != UTC would otherwise emit "+02:00"-style tails, which
// parse_iso8601 (src/rgw/rgw_common.cc, extended format — the mode
// rgw_keystone.cc uses for expires_at) rejects: only an empty tail, "Z" or
// ".fractionZ" parse, so rgw_keystone.cc throws "Failed to parse ISO8601
// expiration date from Keystone response" and every token validation fails.
// rgw_keystone.cc interprets the parsed fields as UTC (internal_timegm), so
// the instant must be converted, not merely re-suffixed.
func keystoneTimestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// AuthRequest represents an authentication request
// ScopeField handles both string ("unscoped") and object scope formats
type ScopeField struct {
	IsUnscoped bool
	Project    *struct {
		Name   string `json:"name"`
		ID     string `json:"id"`
		Domain *struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"domain,omitempty"`
	}
}

// UnmarshalJSON implements custom JSON unmarshaling for ScopeField
func (s *ScopeField) UnmarshalJSON(data []byte) error {
	// Try to unmarshal as string first
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		s.IsUnscoped = (str == "unscoped")
		return nil
	}

	// Otherwise unmarshal as object
	var temp struct {
		Project *struct {
			Name   string `json:"name"`
			ID     string `json:"id"`
			Domain *struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"domain,omitempty"`
		} `json:"project,omitempty"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	s.Project = temp.Project
	s.IsUnscoped = false
	return nil
}

type AuthRequest struct {
	Auth struct {
		Identity struct {
			Methods  []string `json:"methods"`
			Password *struct {
				User struct {
					Name     string `json:"name"`
					Password string `json:"password"`
					Domain   *struct {
						Name string `json:"name"`
					} `json:"domain"`
				} `json:"user"`
			} `json:"password,omitempty"`
			Token *struct {
				ID string `json:"id"`
			} `json:"token,omitempty"`
		} `json:"identity"`
		Scope *ScopeField `json:"scope,omitempty"`
	} `json:"auth"`
}

// AuthResponse represents an authentication response
type AuthResponse struct {
	Token struct {
		ExpiresAt string           `json:"expires_at"`
		IssuedAt  string           `json:"issued_at"`
		Methods   []string         `json:"methods,omitempty"`
		User      map[string]any   `json:"user"`
		Catalog   []CatalogEntry   `json:"catalog,omitempty"`
		Project   *map[string]any  `json:"project,omitempty"`
		Roles     []map[string]any `json:"roles,omitempty"`
	} `json:"token"`
}

// CatalogEntry represents a service in the catalog
type CatalogEntry struct {
	Type      string     `json:"type"`
	Name      string     `json:"name"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Endpoint represents a service endpoint
type Endpoint struct {
	Interface string `json:"interface"`
	Region    string `json:"region"`
	URL       string `json:"url"`
}

// AuthenticatePassword authenticates user with password
func (s *AuthService) AuthenticatePassword(ctx context.Context, req *AuthRequest) (*AuthResponse, string, error) {
	if req.Auth.Identity.Password == nil {
		return nil, "", common.NewBadRequestError("password authentication required")
	}

	username := req.Auth.Identity.Password.User.Name
	password := req.Auth.Identity.Password.User.Password

	// Get domain name (default to "Default" if not specified)
	domainName := "Default"
	if req.Auth.Identity.Password.User.Domain != nil && req.Auth.Identity.Password.User.Domain.Name != "" {
		domainName = req.Auth.Identity.Password.User.Domain.Name
	}

	// Look up domain ID
	var domainID string
	err := database.DB.QueryRow(ctx,
		"SELECT id FROM domains WHERE name = $1",
		domainName,
	).Scan(&domainID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", common.NewUnauthorizedError("invalid domain")
	}
	if err != nil {
		return nil, "", fmt.Errorf("database error looking up domain: %w", err)
	}

	// Fetch user from database (with domain filter)
	var user database.User
	err = database.DB.QueryRow(ctx,
		"SELECT id, name, password_hash, enabled, domain_id FROM users WHERE name = $1 AND domain_id = $2",
		username, domainID,
	).Scan(&user.ID, &user.Name, &user.PasswordHash, &user.Enabled, &user.DomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", common.NewUnauthorizedError("invalid credentials")
	}
	if err != nil {
		return nil, "", fmt.Errorf("database error: %w", err)
	}

	if !user.Enabled {
		return nil, "", common.NewUnauthorizedError("user is disabled")
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, "", common.NewUnauthorizedError("invalid credentials")
	}

	// Handle scoped vs unscoped
	var projectID string
	var roles []string
	var project *database.Project

	// Check if scope is explicitly requested
	if req.Auth.Scope != nil && !req.Auth.Scope.IsUnscoped && req.Auth.Scope.Project != nil {
		// Scoped authentication
		projectName := req.Auth.Scope.Project.Name
		projectIDParam := s.fromAdvertisedProjectID(req.Auth.Scope.Project.ID)

		// A non-UUID project id cannot name a project (projects.id is a UUID
		// column): fail with the same 401 as a missing project instead of
		// letting Postgres reject the parameter as a 500.
		if projectIDParam != "" && uuid.Validate(projectIDParam) != nil {
			return nil, "", common.NewUnauthorizedError("project not found")
		}

		// Fetch project (with domain filter)
		var proj database.Project
		var query string
		var params []any
		if projectIDParam != "" {
			query = "SELECT id, name, description, enabled, domain_id FROM projects WHERE id = $1 AND domain_id = $2"
			params = []any{projectIDParam, domainID}
		} else {
			query = "SELECT id, name, description, enabled, domain_id FROM projects WHERE name = $1 AND domain_id = $2"
			params = []any{projectName, domainID}
		}

		err := database.DB.QueryRow(ctx, query, params...).Scan(
			&proj.ID, &proj.Name, &proj.Description, &proj.Enabled, &proj.DomainID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", common.NewUnauthorizedError("project not found")
		}
		if err != nil {
			return nil, "", fmt.Errorf("database error: %w", err)
		}

		if !proj.Enabled {
			return nil, "", common.NewUnauthorizedError("project is disabled")
		}

		projectID = proj.ID
		project = &proj

		// Fetch roles
		rows, err := database.DB.Query(ctx, `
			SELECT r.name
			FROM role_assignments ra
			JOIN roles r ON ra.role_id = r.id
			WHERE ra.user_id = $1 AND ra.project_id = $2
		`, user.ID, projectID)
		if err != nil {
			return nil, "", fmt.Errorf("failed to fetch roles: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var roleName string
			if err := rows.Scan(&roleName); err != nil {
				return nil, "", fmt.Errorf("failed to scan role: %w", err)
			}
			roles = append(roles, roleName)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("rows iteration error")
			return nil, "", fmt.Errorf("failed to iterate roles: %w", err)
		}

		if len(roles) == 0 {
			return nil, "", common.NewForbiddenError("user has no roles on this project")
		}
	}
	// Otherwise: unscoped token (no project, no roles)

	// Generate JWT token
	now := time.Now()
	expiresAt := now.Add(s.tokenTTL)
	claims := &TokenClaims{
		UserID:    user.ID,
		UserName:  user.Name,
		ProjectID: projectID,
		Roles:     roles,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Subject:   user.ID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(s.jwtSecret)
	if err != nil {
		return nil, "", fmt.Errorf("failed to sign token: %w", err)
	}

	// Build response
	resp := &AuthResponse{}
	resp.Token.ExpiresAt = keystoneTimestamp(expiresAt)
	resp.Token.IssuedAt = keystoneTimestamp(now)
	resp.Token.Methods = req.Auth.Identity.Methods

	// Query user's domain name
	var userDomainName string
	err = database.DB.QueryRow(ctx,
		"SELECT name FROM domains WHERE id = $1",
		user.DomainID,
	).Scan(&userDomainName)
	if err != nil {
		userDomainName = "Default" // fallback
	}

	resp.Token.User = map[string]any{
		"id":   user.ID,
		"name": user.Name,
		"domain": map[string]any{
			"id":   user.DomainID,
			"name": userDomainName,
		},
	}

	// Add project and catalog if scoped
	if projectID != "" {
		// Query project's domain name
		var projectDomainName string
		err = database.DB.QueryRow(ctx,
			"SELECT name FROM domains WHERE id = $1",
			project.DomainID,
		).Scan(&projectDomainName)
		if err != nil {
			projectDomainName = "Default" // fallback
		}

		resp.Token.Project = &map[string]any{
			"id":   s.tokenProjectID(project.ID),
			"name": project.Name,
			"domain": map[string]any{
				"id":   project.DomainID,
				"name": projectDomainName,
			},
		}

		// Add roles
		for _, roleName := range roles {
			resp.Token.Roles = append(resp.Token.Roles, map[string]any{
				"id":   roleName,
				"name": roleName,
			})
		}

		// Add service catalog
		resp.Token.Catalog = BuildServiceCatalog(projectID, s.cache, s.glanceS3Endpoint)
	}

	return resp, tokenString, nil
}

// AuthenticateToken handles token-based authentication (re-scoping)
func (s *AuthService) AuthenticateToken(ctx context.Context, req *AuthRequest) (*AuthResponse, string, error) {
	if req.Auth.Identity.Token == nil || req.Auth.Identity.Token.ID == "" {
		return nil, "", common.NewBadRequestError("token authentication required")
	}

	// Validate the provided token
	claims, err := s.ValidateToken(req.Auth.Identity.Token.ID)
	if err != nil {
		return nil, "", err
	}

	// Fetch user from database
	var user database.User
	err = database.DB.QueryRow(ctx,
		"SELECT id, name, password_hash, enabled, domain_id FROM users WHERE id = $1",
		claims.UserID,
	).Scan(&user.ID, &user.Name, &user.PasswordHash, &user.Enabled, &user.DomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", common.NewUnauthorizedError("user not found")
	}
	if err != nil {
		return nil, "", fmt.Errorf("database error: %w", err)
	}

	if !user.Enabled {
		return nil, "", common.NewUnauthorizedError("user is disabled")
	}

	// Handle scoping (same logic as password auth)
	var projectID string
	var roles []string
	var project *database.Project

	if req.Auth.Scope == nil || (req.Auth.Scope != nil && req.Auth.Scope.IsUnscoped) {
		// Unscoped token - no project/roles
		projectID = ""
	} else if req.Auth.Scope != nil && req.Auth.Scope.Project != nil {
		// Scoped authentication
		projectName := req.Auth.Scope.Project.Name
		projectIDParam := s.fromAdvertisedProjectID(req.Auth.Scope.Project.ID)

		// Same non-UUID guard as the password path: a malformed id is a 401,
		// not a 500 from Postgres's uuid parser.
		if projectIDParam != "" && uuid.Validate(projectIDParam) != nil {
			return nil, "", common.NewUnauthorizedError("project not found")
		}

		// Fetch project
		var proj database.Project
		var query string
		var params []any
		if projectIDParam != "" {
			query = "SELECT id, name, description, enabled, domain_id FROM projects WHERE id = $1 AND domain_id = $2"
			params = []any{projectIDParam, user.DomainID}
		} else {
			query = "SELECT id, name, description, enabled, domain_id FROM projects WHERE name = $1 AND domain_id = $2"
			params = []any{projectName, user.DomainID}
		}

		err := database.DB.QueryRow(ctx, query, params...).Scan(
			&proj.ID, &proj.Name, &proj.Description, &proj.Enabled, &proj.DomainID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", common.NewUnauthorizedError("project not found")
		}
		if err != nil {
			return nil, "", fmt.Errorf("database error: %w", err)
		}

		if !proj.Enabled {
			return nil, "", common.NewUnauthorizedError("project is disabled")
		}

		projectID = proj.ID
		project = &proj

		// Fetch roles
		rows, err := database.DB.Query(ctx, `
			SELECT r.name
			FROM role_assignments ra
			JOIN roles r ON ra.role_id = r.id
			WHERE ra.user_id = $1 AND ra.project_id = $2
		`, user.ID, projectID)
		if err != nil {
			return nil, "", fmt.Errorf("failed to fetch roles: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var roleName string
			if err := rows.Scan(&roleName); err != nil {
				return nil, "", fmt.Errorf("failed to scan role: %w", err)
			}
			roles = append(roles, roleName)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("rows iteration error")
			return nil, "", fmt.Errorf("failed to iterate roles: %w", err)
		}

		if len(roles) == 0 {
			return nil, "", common.NewForbiddenError("user has no roles on this project")
		}
	}

	// Generate new JWT token
	now := time.Now()
	expiresAt := now.Add(s.tokenTTL)
	tokenClaims := &TokenClaims{
		UserID:    user.ID,
		UserName:  user.Name,
		ProjectID: projectID,
		Roles:     roles,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Subject:   user.ID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, tokenClaims)
	tokenString, err := token.SignedString(s.jwtSecret)
	if err != nil {
		return nil, "", fmt.Errorf("failed to sign token: %w", err)
	}

	// Build response
	resp := &AuthResponse{}
	resp.Token.ExpiresAt = keystoneTimestamp(expiresAt)
	resp.Token.IssuedAt = keystoneTimestamp(now)
	resp.Token.Methods = req.Auth.Identity.Methods

	// Query user's domain name
	var userDomainName string
	err = database.DB.QueryRow(ctx,
		"SELECT name FROM domains WHERE id = $1",
		user.DomainID,
	).Scan(&userDomainName)
	if err != nil {
		userDomainName = "Default" // fallback
	}

	resp.Token.User = map[string]any{
		"id":   user.ID,
		"name": user.Name,
		"domain": map[string]any{
			"id":   user.DomainID,
			"name": userDomainName,
		},
	}

	// Add project and catalog if scoped
	if projectID != "" {
		// Query project's domain name
		var projectDomainName string
		err = database.DB.QueryRow(ctx,
			"SELECT name FROM domains WHERE id = $1",
			project.DomainID,
		).Scan(&projectDomainName)
		if err != nil {
			projectDomainName = "Default" // fallback
		}

		resp.Token.Project = &map[string]any{
			"id":   s.tokenProjectID(project.ID),
			"name": project.Name,
			"domain": map[string]any{
				"id":   project.DomainID,
				"name": projectDomainName,
			},
		}

		// Add roles
		for _, roleName := range roles {
			resp.Token.Roles = append(resp.Token.Roles, map[string]any{
				"id":   roleName,
				"name": roleName,
			})
		}

		// Add service catalog
		resp.Token.Catalog = BuildServiceCatalog(projectID, s.cache, s.glanceS3Endpoint)
	}

	return resp, tokenString, nil
}

// BuildTokenResponse builds the standard Keystone v3 token envelope for an
// already-issued token (GET /v3/auth/tokens) from validated claims, mirroring
// the rendering of the two token-issue paths above as closely as practical.
// Ceph RGW validates every user token through this endpoint and parses the
// response with rgw::keystone::TokenEnvelope::decode_json
// (src/rgw/rgw_keystone.cc), whose mandatory fields are user{id,name},
// expires_at, roles[]{name} and, for scoped tokens, project{id,name} — the
// old flat {"project_id": ...} shape with roles as bare strings was
// unparseable by RGW (and by standard clients like gophercloud's tokens.Get).
//
// Rendering notes, mirroring the issue paths:
//   - expires_at/issued_at come from the JWT RegisteredClaims, rendered as
//     UTC RFC3339 via keystoneTimestamp (Ceph RGW's parse_iso8601 rejects
//     offset tails).
//   - project.id goes through tokenProjectID (keystone.rgw_compat_tenant_ids)
//     exactly like the issue responses — this validation response is where
//     RGW reads the tenant from.
//   - project name/domain and the user domain are looked up in the DB; a
//     failed lookup degrades gracefully (domain names fall back to "Default";
//     a missing project or user row omits the project/domain key).
//   - roles render as [{"id": name, "name": name}] objects.
//   - the catalog is built from the CANONICAL project id, like the issue path.
//   - methods are omitted: the JWT claims do not carry the original identity
//     methods, and RGW does not require them.
func (s *AuthService) BuildTokenResponse(ctx context.Context, claims *TokenClaims) *AuthResponse {
	resp := &AuthResponse{}
	if claims.ExpiresAt != nil {
		resp.Token.ExpiresAt = keystoneTimestamp(claims.ExpiresAt.Time)
	}
	if claims.IssuedAt != nil {
		resp.Token.IssuedAt = keystoneTimestamp(claims.IssuedAt.Time)
	}

	// User: id/name from the claims; domain looked up so the shape matches the
	// issue paths (id + name, "Default" name fallback). The domain key is
	// omitted when the user row is gone.
	user := map[string]any{
		"id":   claims.UserID,
		"name": claims.UserName,
	}
	var userDomainID string
	err := database.DB.QueryRow(ctx,
		"SELECT domain_id FROM users WHERE id = $1",
		claims.UserID,
	).Scan(&userDomainID)
	switch {
	case err == nil:
		userDomainName := "Default" // fallback, like the issue paths
		if err := database.DB.QueryRow(ctx,
			"SELECT name FROM domains WHERE id = $1",
			userDomainID,
		).Scan(&userDomainName); err != nil {
			userDomainName = "Default"
		}
		user["domain"] = map[string]any{
			"id":   userDomainID,
			"name": userDomainName,
		}
	case errors.Is(err, pgx.ErrNoRows):
		// nothing to look up — render id/name from the claims only
	default:
		log.Error().Err(err).Str("operation", "validate_token").Str("user_id", claims.UserID).Msg("Failed to query user domain")
	}
	resp.Token.User = user

	// Project, roles and catalog only for scoped tokens, like the issue paths.
	if claims.ProjectID != "" {
		var projectName, projectDomainID string
		err := database.DB.QueryRow(ctx,
			"SELECT name, domain_id FROM projects WHERE id = $1",
			claims.ProjectID,
		).Scan(&projectName, &projectDomainID)
		switch {
		case err == nil:
			projectDomainName := "Default" // fallback, like the issue paths
			if err := database.DB.QueryRow(ctx,
				"SELECT name FROM domains WHERE id = $1",
				projectDomainID,
			).Scan(&projectDomainName); err != nil {
				projectDomainName = "Default"
			}
			resp.Token.Project = &map[string]any{
				"id":   s.tokenProjectID(claims.ProjectID),
				"name": projectName,
				"domain": map[string]any{
					"id":   projectDomainID,
					"name": projectDomainName,
				},
			}
		case errors.Is(err, pgx.ErrNoRows):
			// The token's project no longer exists: render no project — the
			// closest safe shape (RGW's parser rejects a scoped token without
			// a project, which is the correct outcome for a dead scope).
			log.Warn().Err(err).Str("operation", "validate_token").Str("project_id", claims.ProjectID).Msg("Token project not found; omitting project from validation response")
		default:
			log.Error().Err(err).Str("operation", "validate_token").Str("project_id", claims.ProjectID).Msg("Failed to query token project; omitting project from validation response")
		}

		// Roles, exactly like the issue paths: objects with id + name.
		for _, roleName := range claims.Roles {
			resp.Token.Roles = append(resp.Token.Roles, map[string]any{
				"id":   roleName,
				"name": roleName,
			})
		}

		// Service catalog from the CANONICAL project id, like the issue paths.
		resp.Token.Catalog = BuildServiceCatalog(claims.ProjectID, s.cache, s.glanceS3Endpoint)
	}

	return resp
}

// ValidateToken validates and parses a JWT token
func (s *AuthService) ValidateToken(tokenString string) (*TokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return s.jwtSecret, nil
	})

	if err != nil {
		return nil, common.NewUnauthorizedError("invalid token")
	}

	if claims, ok := token.Claims.(*TokenClaims); ok && token.Valid {
		// Keystone tokens always carry an expiry. The JWT library only
		// enforces exp when the claim is present, so a signed token without
		// one would otherwise pass validation and later render an empty
		// expires_at in the validation envelope — mandatory for Ceph RGW's
		// TokenEnvelope decoder, which rejects the whole response on it.
		// Treat a missing expiry as an invalid token.
		if claims.ExpiresAt == nil {
			return nil, common.NewUnauthorizedError("token missing expiry")
		}
		return claims, nil
	}

	return nil, common.NewUnauthorizedError("invalid token claims")
}

// HashPassword hashes a password using bcrypt
func (s *AuthService) HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

// CheckPassword verifies a password against a bcrypt hash
func (s *AuthService) CheckPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// BuildServiceCatalog builds the OpenStack service catalog from database
func BuildServiceCatalog(projectID string, cacheInstance *cache.Cache, s3Endpoint string) []CatalogEntry {
	ctx := context.Background()

	// Try cache first (service catalog is immutable, 24h TTL)
	if cacheInstance != nil {
		cacheKey := "service_catalog:" + projectID
		var cached []CatalogEntry
		if err := cacheInstance.Get(ctx, cacheKey, &cached); err == nil {
			return cached
		}
	}

	catalog := []CatalogEntry{}

	// Query services and their endpoints from database
	rows, err := database.DB.Query(ctx, `
		SELECT s.id, s.type, s.name, e.id, e.interface, e.url, e.region
		FROM services s
		LEFT JOIN endpoints e ON s.id = e.service_id
		WHERE s.enabled = true AND (e.enabled = true OR e.enabled IS NULL)
		ORDER BY s.type, e.interface
	`)
	if err != nil {
		// Fall back to hardcoded catalog on error
		return buildHardcodedCatalog(projectID, s3Endpoint)
	}
	defer rows.Close()

	// Group endpoints by service
	serviceMap := make(map[string]*CatalogEntry)
	for rows.Next() {
		var (
			serviceID  string
			svcType    string
			svcName    string
			endpointID *string
			iface      *string
			url        *string
			region     *string
		)

		if err := rows.Scan(&serviceID, &svcType, &svcName, &endpointID, &iface, &url, &region); err != nil {
			continue
		}

		// The `s3` (Ceph RGW) service is an external endpoint configured out-of-band.
		// Serve the resolved config value (cfg.Glance.S3Endpoint — YAML or env, wired
		// via SetGlanceS3Endpoint, same source the S3 client uses), never migration
		// 062's seeded placeholder (substituteURLTemplates rewrites only `http://lightstack:`,
		// never `localhost`) nor the Lightstack host; omit it when RGW isn't configured.
		// Matches the fallback catalog (PR #73 review r3558646680).
		if svcType == "s3" && s3Endpoint == "" {
			continue
		}

		// Create service entry if not exists
		if _, exists := serviceMap[serviceID]; !exists {
			serviceMap[serviceID] = &CatalogEntry{
				Type:      svcType,
				Name:      svcName,
				Endpoints: []Endpoint{},
			}
		}

		// Add endpoint if present
		if endpointID != nil && iface != nil && url != nil {
			// Substitute URL templates
			substitutedURL := substituteURLTemplates(*url, projectID)
			if svcType == "s3" {
				substitutedURL = s3Endpoint // external RGW; serve the resolved config URL as-is
			}

			endpoint := Endpoint{
				Interface: *iface,
				URL:       substitutedURL,
			}
			if region != nil {
				endpoint.Region = *region
			}
			serviceMap[serviceID].Endpoints = append(serviceMap[serviceID].Endpoints, endpoint)
		}
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}

	// Convert map to slice
	for _, entry := range serviceMap {
		catalog = append(catalog, *entry)
	}

	// Fall back to hardcoded if query returned nothing
	if len(catalog) == 0 {
		return buildHardcodedCatalog(projectID, s3Endpoint)
	}

	// Store in cache (24h TTL per config)
	if cacheInstance != nil {
		cacheInstance.TrySet(ctx, "service_catalog:"+projectID, catalog, 24*time.Hour)
	}

	return catalog
}

// substituteURLTemplates replaces template placeholders in endpoint URLs
func substituteURLTemplates(url, projectID string) string {
	// Replace {project_id} placeholder
	url = strings.ReplaceAll(url, "{project_id}", projectID)
	// Also handle $(project_id)s format (OpenStack convention)
	url = strings.ReplaceAll(url, "$(project_id)s", projectID)
	// Also handle %(project_id)s format (Python string formatting)
	url = strings.ReplaceAll(url, "%(project_id)s", projectID)
	// Also handle plain %s format (legacy)
	url = strings.ReplaceAll(url, "%s", projectID)

	// Replace hostname based on LIGHTSTACK_ENDPOINT_HOST environment variable
	// This allows using 'localhost' in CI and 'lightstack' in docker-compose
	baseHost := os.Getenv("LIGHTSTACK_ENDPOINT_HOST")
	if baseHost == "" {
		baseHost = "localhost"
	}
	// Replace http://lightstack: with the configured host
	url = strings.ReplaceAll(url, "http://lightstack:", "http://"+baseHost+":")
	// Also handle https://lightstack: for future SSL support
	url = strings.ReplaceAll(url, "https://lightstack:", "https://"+baseHost+":")

	// Apply the configured advertised scheme (SPEC-007 WS-3, FR-010/012/013).
	url = common.ForceScheme(url, common.EndpointScheme())

	return url
}

// buildHardcodedCatalog provides fallback catalog (previous implementation)
func buildHardcodedCatalog(projectID, s3Endpoint string) []CatalogEntry {
	// Use LIGHTSTACK_ENDPOINT_HOST env var, default to "localhost" for CI compatibility
	baseHost := os.Getenv("LIGHTSTACK_ENDPOINT_HOST")
	if baseHost == "" {
		baseHost = "localhost"
	}
	// Honor the configured advertised scheme (SPEC-007 WS-3, FR-010).
	baseURL := common.EndpointScheme() + "://" + baseHost

	entries := []CatalogEntry{
		{
			Type: "identity",
			Name: "keystone",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:35357/v3", baseURL)},
			},
		},
		{
			Type: "compute",
			Name: "nova",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:8774/v2.1/%s", baseURL, projectID)},
			},
		},
		{
			Type: "placement",
			Name: "placement",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:8778", baseURL)},
			},
		},
		{
			Type: "network",
			Name: "neutron",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:9696", baseURL)},
			},
		},
		{
			Type: "volumev3",
			Name: "cinderv3",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:8776/v3/%s", baseURL, projectID)},
			},
		},
		{
			Type: "image",
			Name: "glance",
			Endpoints: []Endpoint{
				{Interface: "public", Region: "RegionOne", URL: fmt.Sprintf("%s:9292", baseURL)},
			},
		},
	}
	// Tenant object storage (Ceph RGW S3): the endpoint is the external RGW, taken
	// from the resolved Glance S3 config (cfg.Glance.S3Endpoint, wired via
	// SetGlanceS3Endpoint). Only advertised when configured (SPEC-014 FR-009); the
	// DB catalog (migration 062) is primary, this is the fallback.
	if s3Endpoint != "" {
		entries = append(entries, CatalogEntry{
			Type:      "s3",
			Name:      "rgw",
			Endpoints: []Endpoint{{Interface: "public", Region: "RegionOne", URL: s3Endpoint}},
		})
	}
	return entries
}
