package keystone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/cache"
	"github.com/golang-jwt/jwt/v5"
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
// which the config layer resolves from YAML or O3K_GLANCE_S3_ENDPOINT) so the service
// catalog advertises the real external RGW rather than a placeholder. Set once at
// startup, mirroring nova.SetImageStore.
func (s *AuthService) SetGlanceS3Endpoint(url string) { s.glanceS3Endpoint = url }

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
		Methods   []string         `json:"methods"`
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
		projectIDParam := req.Auth.Scope.Project.ID

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
				log.Warn().Err(err).Str("operation", "AuthenticatePassword").Msg("failed to scan row")
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
	resp.Token.ExpiresAt = expiresAt.Format(time.RFC3339)
	resp.Token.IssuedAt = now.Format(time.RFC3339)
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
			"id":   project.ID,
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
		projectIDParam := req.Auth.Scope.Project.ID

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
				log.Warn().Err(err).Str("operation", "AuthenticateToken").Msg("failed to scan row")
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
	resp.Token.ExpiresAt = expiresAt.Format(time.RFC3339)
	resp.Token.IssuedAt = now.Format(time.RFC3339)
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
			"id":   project.ID,
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
			log.Warn().Err(err).Str("operation", "BuildServiceCatalog").Msg("failed to scan row")
			continue
		}

		// The `s3` (Ceph RGW) service is an external endpoint configured out-of-band.
		// Serve the resolved config value (cfg.Glance.S3Endpoint — YAML or env, wired
		// via SetGlanceS3Endpoint, same source the S3 client uses), never migration
		// 062's seeded placeholder nor the o3k host; omit it when RGW isn't configured.
		// substituteURLTemplates rewrites both the `o3k` and `localhost` hosts to the
		// configured endpoint host, so it would turn that seed into the O3K host — which
		// is still the wrong target for an external RGW, hence this special case.
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
	// A partial read must not become the served catalog: it would be cached for 24h
	// (below) and hand every token issued in that window a catalog with endpoints
	// missing. Fall back to the complete hardcoded catalog and do NOT cache.
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("service catalog rows iteration error; serving hardcoded catalog uncached")
		return buildHardcodedCatalog(projectID, s3Endpoint)
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

	// Replace hostname based on O3K_ENDPOINT_HOST environment variable
	// This allows using 'localhost' in CI and 'o3k' in docker-compose
	baseHost := os.Getenv("O3K_ENDPOINT_HOST")
	if baseHost == "" {
		baseHost = "localhost"
	}
	// Replace http://o3k: with the configured host
	url = strings.ReplaceAll(url, "http://o3k:", "http://"+baseHost+":")
	// Also handle https://o3k: for future SSL support
	url = strings.ReplaceAll(url, "https://o3k:", "https://"+baseHost+":")
	// Also rewrite localhost-seeded endpoints (e.g. the cinder `volume` alias from
	// migration 054, which migration 052's one-shot o3k rewrite never reached) so they
	// aren't pinned to localhost in non-local deployments (F5, FR-006/007). No-op when
	// the host is unset (baseHost defaults to localhost above).
	url = strings.ReplaceAll(url, "http://localhost:", "http://"+baseHost+":")
	url = strings.ReplaceAll(url, "https://localhost:", "https://"+baseHost+":")

	// Apply the configured advertised scheme (SPEC-007 WS-3, FR-010/012/013).
	url = common.ForceScheme(url, common.EndpointScheme())

	return url
}

// buildHardcodedCatalog provides fallback catalog (previous implementation)
func buildHardcodedCatalog(projectID, s3Endpoint string) []CatalogEntry {
	// Use O3K_ENDPOINT_HOST env var, default to "localhost" for CI compatibility
	baseHost := os.Getenv("O3K_ENDPOINT_HOST")
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
