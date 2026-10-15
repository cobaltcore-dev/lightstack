package keystone

import (
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListDomains handles GET /v3/domains
//
// The `name` filter is honoured; every other query parameter is still ignored. That
// one matters because standard clients resolve a `--domain` argument client-side by
// name when retrieval by id fails, and an unfiltered list silently handed them every
// domain instead of the one they asked for (SPEC-022 FR-001a). Names are matched
// exactly — upstream treats domain names as case-sensitive.
func (svc *Service) ListDomains(c *gin.Context) {
	query := `SELECT id, name, description, enabled FROM domains`
	args := []any{}
	if name := c.Query("name"); name != "" {
		query += ` WHERE name = $1`
		args = append(args, name)
	}
	query += ` ORDER BY name ASC`

	rows, err := svc.pool.Query(c.Request.Context(), query, args...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_domains").Msg("Failed to query domains")
		common.SendError(c, common.NewInternalServerError("failed to query domains"))
		return
	}
	defer rows.Close()

	domains := []map[string]any{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		var description *string
		var enabled bool

		err := rows.Scan(&id, &name, &description, &enabled)
		if err != nil {
			continue
		}

		domain := map[string]any{
			"id":      id.String(),
			"name":    name,
			"enabled": enabled,
		}

		if description != nil {
			domain["description"] = *description
		}

		domains = append(domains, domain)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(200, gin.H{"domains": domains})
}

// CreateDomain handles POST /v3/domains
func (svc *Service) CreateDomain(c *gin.Context) {
	var req struct {
		Domain struct {
			Name        string `json:"name" binding:"required"`
			Description string `json:"description"`
			Enabled     *bool  `json:"enabled"`
		} `json:"domain" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	domainID := uuid.New()
	enabled := true
	if req.Domain.Enabled != nil {
		enabled = *req.Domain.Enabled
	}

	var description *string
	if req.Domain.Description != "" {
		description = &req.Domain.Description
	}

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO domains (id, name, description, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, domainID, req.Domain.Name, description, enabled, time.Now(), time.Now())

	if err != nil {
		common.SendError(c, common.NewConflictError("domain already exists"))
		return
	}

	domain := map[string]any{
		"id":      domainID.String(),
		"name":    req.Domain.Name,
		"enabled": enabled,
	}

	if description != nil {
		domain["description"] = *description
	}
	// Upstream always embeds links.self; the pinned OSC 7 formatter aborts
	// with KeyError 'links' without it (SPEC-028).
	domain["links"] = identityLink(c, "domains", domainID.String())

	c.JSON(201, gin.H{"domain": domain})
}

// GetDomain handles GET /v3/domains/:id
func (svc *Service) GetDomain(c *gin.Context) {
	domainID := c.Param("id")

	// This route is registered on the non-validating group precisely so the `default`
	// alias reaches here (see the note in RegisterRoutes). Resolve it to a real id
	// first; the resolver also rejects any other non-identifier value without a query,
	// which is what lets the verbatim 404 below stand in for the middleware's
	// (SPEC-022 FR-001a).
	resolved, outcome := resolveDomainRef(c.Request.Context(), svc.pool, domainID, anyDomainState)
	switch outcome {
	case domainNotFound:
		common.SendError(c, common.NewIdentityNotFoundError("domain", domainID))
		return
	case domainQueryFailed:
		common.SendError(c, common.NewInternalServerError("failed to resolve domain"))
		return
	}

	var id uuid.UUID
	var name string
	var description *string
	var enabled bool

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, enabled
		FROM domains
		WHERE id = $1
	`, resolved).Scan(&id, &name, &description, &enabled)

	if err != nil {
		notFoundOr500(c, err, "domain", domainID)
		return
	}

	domain := map[string]any{
		"id":      id.String(),
		"name":    name,
		"enabled": enabled,
	}

	if description != nil {
		domain["description"] = *description
	}
	// See CreateDomain: links.self is mandatory for the CLI's formatter.
	domain["links"] = identityLink(c, "domains", id.String())

	c.JSON(200, gin.H{"domain": domain})
}

// UpdateDomain handles PATCH /v3/domains/:id
func (svc *Service) UpdateDomain(c *gin.Context) {
	domainID := c.Param("id")

	var req struct {
		Domain map[string]any `json:"domain" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify domain exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM domains WHERE id = $1)
	`, domainID).Scan(&exists)

	if existsOr404(c, err, exists, "domain", domainID) {
		return
	}

	// Build update query dynamically
	u := &common.SQLUpdate{}

	// The placeholders here used to be built as `"$" + string(rune('0'+argCount))`, which produces a
	// digit only while the count stays below ten — the tenth argument would have rendered "$:" and
	// the query would have failed to parse. Three assignments never reached it, but the next one
	// added would have. Arg has no such ceiling.
	if name, ok := req.Domain["name"].(string); ok {
		u.Set("name", name)
	}

	if description, ok := req.Domain["description"].(string); ok {
		u.Set("description", description)
	}

	if enabled, ok := req.Domain["enabled"].(bool); ok {
		u.Set("enabled", enabled)
	}

	// Counted before updated_at is added, so "nothing was asked for" stays distinguishable from
	// "only the timestamp would change" — the condition the guard below relies on.
	changed := !u.Empty()
	u.Set("updated_at", time.Now())

	if changed { // more than just updated_at
		query := "UPDATE domains SET " + u.Assignments() + " WHERE id = " + u.Arg(domainID)

		_, err = svc.pool.Exec(c.Request.Context(), query, u.Args()...)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_domain").Str("domain_id", domainID).Msg("Failed to update domain")
			common.SendError(c, common.NewInternalServerError("failed to update domain"))
			return
		}
	}

	// Fetch updated domain
	var id uuid.UUID
	var name string
	var description *string
	var enabled bool

	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, enabled
		FROM domains
		WHERE id = $1
	`, domainID).Scan(&id, &name, &description, &enabled)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_domain_fetch").Str("domain_id", domainID).Msg("Failed to fetch updated domain")
		common.SendError(c, common.NewInternalServerError("failed to fetch updated domain"))
		return
	}

	domain := map[string]any{
		"id":      id.String(),
		"name":    name,
		"enabled": enabled,
	}

	if description != nil {
		domain["description"] = *description
	}

	c.JSON(200, gin.H{"domain": domain})
}

// DeleteDomain handles DELETE /v3/domains/:id
func (svc *Service) DeleteDomain(c *gin.Context) {
	domainID := c.Param("id")

	// Check if domain is enabled
	var enabled bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT enabled FROM domains WHERE id = $1
	`, domainID).Scan(&enabled)

	if err != nil {
		notFoundOr500(c, err, "domain", domainID)
		return
	}

	if enabled {
		common.SendError(c, common.NewForbiddenError("cannot delete enabled domain; disable it first"))
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM domains WHERE id = $1",
		domainID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_domain").Str("domain_id", domainID).Msg("Failed to delete domain")
		common.SendError(c, common.NewInternalServerError("failed to delete domain"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("domain", domainID))
		return
	}

	c.Status(204)
}

// GetDomainConfig handles GET /v3/domains/:id/config
func (svc *Service) GetDomainConfig(c *gin.Context) {
	domainID := c.Param("id")

	// Verify domain exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM domains WHERE id = $1)
	`, domainID).Scan(&exists)

	if existsOr404(c, err, exists, "domain", domainID) {
		return
	}

	// Return empty config (stub implementation)
	config := map[string]any{
		"identity": map[string]any{},
		"ldap":     map[string]any{},
	}

	c.JSON(200, gin.H{"config": config})
}
