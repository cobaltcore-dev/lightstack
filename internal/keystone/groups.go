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

// ListGroups handles GET /v3/groups
func (svc *Service) ListGroups(c *gin.Context) {
	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}

	var f listFilter
	f.eq(c, "name", "name")
	f.eqDomain(c, "domain_id", "domain_id")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, name, domain_id, description, created_at FROM groups"+f.Where()+
			" ORDER BY created_at DESC", f.Args()...,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_groups").Msg("Failed to query groups")
		common.SendError(c, common.NewInternalServerError("failed to query groups"))
		return
	}
	defer rows.Close()

	var groups []gin.H
	for rows.Next() {
		var id, name, domainID, description string
		var createdAt time.Time

		if err := rows.Scan(&id, &name, &domainID, &description, &createdAt); err != nil {
			log.Warn().Err(err).Str("operation", "ListGroups").Msg("failed to scan row")
			continue
		}

		groups = append(groups, gin.H{
			"id":          id,
			"name":        name,
			"domain_id":   domainID,
			"description": description,
			"links":       identityLink(c, "groups", id),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if groups == nil {
		groups = []gin.H{}
	}

	c.JSON(200, gin.H{"groups": groups})
}

// CreateGroup handles POST /v3/groups
func (svc *Service) CreateGroup(c *gin.Context) {
	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}

	var req struct {
		Group struct {
			Name        string `json:"name" binding:"required"`
			DomainID    string `json:"domain_id"`
			Description string `json:"description"`
		} `json:"group" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Resolve the domain reference through the shared resolver. This handler used to store
	// whatever domain_id it was given — the column was a VARCHAR with no foreign key until
	// migration 072 — so `default` was kept verbatim, a string naming no domain was
	// accepted, and one domain could hold the same group name twice (SPEC-022 FR-006, A3).
	domainID, outcome := resolveDomainRef(c.Request.Context(), svc.pool, req.Group.DomainID, enabledDomainsOnly)
	switch outcome {
	case domainNotFound:
		common.SendError(c, common.NewBadRequestError("specified domain not found or disabled"))
		return
	case domainQueryFailed:
		common.SendError(c, common.NewInternalServerError("failed to resolve domain"))
		return
	}

	groupID := uuid.New().String()
	now := time.Now()

	_, err := svc.pool.Exec(c.Request.Context(),
		`INSERT INTO groups (id, name, domain_id, description, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		groupID, req.Group.Name, domainID, req.Group.Description, now, now,
	)
	if err != nil {
		// groups is UNIQUE(name, domain_id) (migration 010), and the violation used to
		// surface as a generic 500 — the same defect CreateProject just shed. The
		// message names the domain because, unlike projects.name, the constraint is
		// per-domain: the same group name is legitimate in another domain.
		if common.IsUniqueViolation(err) {
			common.SendError(c, common.NewConflictError("group with name '"+req.Group.Name+"' already exists in this domain"))
			return
		}
		log.Error().Err(err).Str("operation", "create_group").Msg("Failed to create group")
		common.SendError(c, common.NewInternalServerError("failed to create group"))
		return
	}

	c.JSON(201, gin.H{
		"group": gin.H{
			"id":          groupID,
			"name":        req.Group.Name,
			"domain_id":   domainID,
			"description": req.Group.Description,
			"links":       identityLink(c, "groups", groupID),
		},
	})
}

// GetGroup handles GET /v3/groups/:id
func (svc *Service) GetGroup(c *gin.Context) {
	groupID := c.Param("id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) {
		return
	}

	var name, domainID, description string
	var createdAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT name, domain_id, description, created_at FROM groups WHERE id = $1",
		groupID,
	).Scan(&name, &domainID, &description, &createdAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewIdentityNotFoundError("group", groupID))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_group").Str("group_id", groupID).Msg("Failed to query group")
		common.SendError(c, common.NewInternalServerError("failed to query group"))
		return
	}

	c.JSON(200, gin.H{
		"group": gin.H{
			"id":          groupID,
			"name":        name,
			"domain_id":   domainID,
			"description": description,
			"links":       identityLink(c, "groups", groupID),
		},
	})
}

// UpdateGroup handles PATCH /v3/groups/:id
func (svc *Service) UpdateGroup(c *gin.Context) {
	groupID := c.Param("id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) {
		return
	}

	var req struct {
		Group struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"group" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build dynamic update query
	u := &common.SQLUpdate{}

	if req.Group.Name != nil {
		u.Set("name", *req.Group.Name)
	}
	if req.Group.Description != nil {
		u.Set("description", *req.Group.Description)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	u.SetExpr("updated_at = NOW()")
	query := "UPDATE groups SET " + u.Assignments() + " WHERE id = " + u.Arg(groupID)
	_, err := svc.pool.Exec(c.Request.Context(), query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_group").Str("group_id", groupID).Msg("Failed to update group")
		common.SendError(c, common.NewInternalServerError("failed to update group"))
		return
	}

	// Fetch updated group
	var name, domainID, description string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT name, domain_id, description FROM groups WHERE id = $1",
		groupID,
	).Scan(&name, &domainID, &description)

	if err != nil {
		notFoundOr500(c, err, "group", groupID)
		return
	}

	c.JSON(200, gin.H{
		"group": gin.H{
			"id":          groupID,
			"name":        name,
			"domain_id":   domainID,
			"description": description,
			"links":       identityLink(c, "groups", groupID),
		},
	})
}

// DeleteGroup handles DELETE /v3/groups/:id
func (svc *Service) DeleteGroup(c *gin.Context) {
	groupID := c.Param("id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM groups WHERE id = $1",
		groupID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_group").Str("group_id", groupID).Msg("Failed to delete group")
		common.SendError(c, common.NewInternalServerError("failed to delete group"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("group", groupID))
		return
	}

	c.Status(204)
}

// ListGroupUsers handles GET /v3/groups/:id/users
func (svc *Service) ListGroupUsers(c *gin.Context) {
	groupID := c.Param("id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) {
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT u.id, u.name, u.domain_id, u.enabled
		 FROM users u
		 INNER JOIN group_members gu ON u.id = gu.user_id
		 WHERE gu.group_id = $1`,
		groupID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_group_users").Str("group_id", groupID).Msg("Failed to query group users")
		common.SendError(c, common.NewInternalServerError("failed to query group users"))
		return
	}
	defer rows.Close()

	var users []gin.H
	for rows.Next() {
		var id, name, domainID string
		var enabled bool

		if err := rows.Scan(&id, &name, &domainID, &enabled); err != nil {
			log.Warn().Err(err).Str("operation", "ListGroupUsers").Msg("failed to scan row")
			continue
		}

		users = append(users, gin.H{
			"id":        id,
			"name":      name,
			"domain_id": domainID,
			"enabled":   enabled,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if users == nil {
		users = []gin.H{}
	}

	c.JSON(200, gin.H{"users": users})
}

// AddUserToGroup handles PUT /v3/groups/:id/users/:user_id
func (svc *Service) AddUserToGroup(c *gin.Context) {
	groupID := c.Param("id")
	userID := c.Param("user_id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) || denyIfMalformedID(c, "user", userID) {
		return
	}

	_, err := svc.pool.Exec(c.Request.Context(),
		`INSERT INTO group_members (group_id, user_id)
		 VALUES ($1, $2)
		 ON CONFLICT (group_id, user_id) DO NOTHING`,
		groupID, userID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "add_user_to_group").Str("group_id", groupID).Str("user_id", userID).Msg("Failed to add user to group")
		common.SendError(c, common.NewInternalServerError("failed to add user to group"))
		return
	}

	c.Status(204)
}

// RemoveUserFromGroup handles DELETE /v3/groups/:id/users/:user_id
func (svc *Service) RemoveUserFromGroup(c *gin.Context) {
	groupID := c.Param("id")
	userID := c.Param("user_id")

	// Admin-only (review 2026-10-13 C-2): the identity management surface
	// must not honor a plain member — this is the role/assignment/catalog
	// authority.
	if denyIfNotAdmin(c) {
		return
	}
	// Authorization passed; validate the ids after it so 403 precedes the 404 (#515).
	if denyIfMalformedID(c, "group", groupID) || denyIfMalformedID(c, "user", userID) {
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM group_members WHERE group_id = $1 AND user_id = $2",
		groupID, userID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "remove_user_from_group").Str("group_id", groupID).Str("user_id", userID).Msg("Failed to remove user from group")
		common.SendError(c, common.NewInternalServerError("failed to remove user from group"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewNotFoundError("user in group"))
		return
	}

	c.Status(204)
}
