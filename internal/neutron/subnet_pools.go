package neutron

import (
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// ListSubnetPools lists subnet pools
func (svc *Service) ListSubnetPools(c *gin.Context) {
	projectID := c.GetString("project_id")

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, prefixes, min_prefixlen, max_prefixlen, default_prefixlen,
		       shared, is_default, ip_version, created_at, updated_at
		FROM subnet_pools
		WHERE project_id = $1 OR shared = true
		ORDER BY created_at DESC
	`, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_subnet_pools").Msg("failed to query subnet pools")
		common.SendError(c, common.NewInternalServerError("failed to list subnet pools"))
		return
	}
	defer rows.Close()

	pools := []gin.H{}
	for rows.Next() {
		var id, name string
		var prefixes []string
		var minPrefixlen, maxPrefixlen, ipVersion int
		var defaultPrefixlen *int
		var shared, isDefault bool
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &name, &prefixes, &minPrefixlen, &maxPrefixlen, &defaultPrefixlen,
			&shared, &isDefault, &ipVersion, &createdAt, &updatedAt)
		if err != nil {
			continue
		}

		pool := gin.H{
			"id":            id,
			"name":          name,
			"prefixes":      prefixes,
			"min_prefixlen": minPrefixlen,
			"max_prefixlen": maxPrefixlen,
			"shared":        shared,
			"is_default":    isDefault,
			"ip_version":    ipVersion,
			"tenant_id":     projectID,
			"created_at":    createdAt.Format(time.RFC3339),
			"updated_at":    updatedAt.Format(time.RFC3339),
		}

		if defaultPrefixlen != nil {
			pool["default_prefixlen"] = *defaultPrefixlen
		}

		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"subnetpools": pools})
}

// CreateSubnetPool creates a new subnet pool
func (svc *Service) CreateSubnetPool(c *gin.Context) {
	var req struct {
		SubnetPool struct {
			Name             string   `json:"name" binding:"required"`
			Prefixes         []string `json:"prefixes" binding:"required"`
			MinPrefixlen     *int     `json:"min_prefixlen"`
			MaxPrefixlen     *int     `json:"max_prefixlen"`
			DefaultPrefixlen *int     `json:"default_prefixlen"`
			Shared           *bool    `json:"shared"`
			IsDefault        *bool    `json:"is_default"`
			IPVersion        *int     `json:"ip_version"`
		} `json:"subnetpool"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	poolID := uuid.New().String()

	minPrefixlen := 8
	if req.SubnetPool.MinPrefixlen != nil {
		minPrefixlen = *req.SubnetPool.MinPrefixlen
	}

	maxPrefixlen := 32
	if req.SubnetPool.MaxPrefixlen != nil {
		maxPrefixlen = *req.SubnetPool.MaxPrefixlen
	}

	shared := false
	if req.SubnetPool.Shared != nil {
		shared = *req.SubnetPool.Shared
	}

	isDefault := false
	if req.SubnetPool.IsDefault != nil {
		isDefault = *req.SubnetPool.IsDefault
	}

	ipVersion := 4
	if req.SubnetPool.IPVersion != nil {
		ipVersion = *req.SubnetPool.IPVersion
	}

	now := time.Now()

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO subnet_pools (id, project_id, name, prefixes, min_prefixlen, max_prefixlen,
		                          default_prefixlen, shared, is_default, ip_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, poolID, projectID, req.SubnetPool.Name, req.SubnetPool.Prefixes, minPrefixlen, maxPrefixlen,
		req.SubnetPool.DefaultPrefixlen, shared, isDefault, ipVersion, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_subnet_pool").Msg("failed to create subnet pool")
		common.SendError(c, common.NewInternalServerError("failed to create subnet pool"))
		return
	}

	pool := gin.H{
		"id":            poolID,
		"name":          req.SubnetPool.Name,
		"prefixes":      req.SubnetPool.Prefixes,
		"min_prefixlen": minPrefixlen,
		"max_prefixlen": maxPrefixlen,
		"shared":        shared,
		"is_default":    isDefault,
		"ip_version":    ipVersion,
		"tenant_id":     projectID,
		"created_at":    now.Format(time.RFC3339),
		"updated_at":    now.Format(time.RFC3339),
	}

	if req.SubnetPool.DefaultPrefixlen != nil {
		pool["default_prefixlen"] = *req.SubnetPool.DefaultPrefixlen
	}

	c.JSON(http.StatusCreated, gin.H{"subnetpool": pool})
}

// GetSubnetPool retrieves a subnet pool by ID
func (svc *Service) GetSubnetPool(c *gin.Context) {
	poolID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name string
	var prefixes []string
	var minPrefixlen, maxPrefixlen, ipVersion int
	var defaultPrefixlen *int
	var shared, isDefault bool
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, prefixes, min_prefixlen, max_prefixlen, default_prefixlen,
		       shared, is_default, ip_version, created_at, updated_at
		FROM subnet_pools
		WHERE id = $1 AND (project_id = $2 OR shared = true)
	`, poolID, projectID).Scan(&id, &name, &prefixes, &minPrefixlen, &maxPrefixlen, &defaultPrefixlen,
		&shared, &isDefault, &ipVersion, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet pool"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_subnet_pool").Msg("failed to get subnet pool")
		common.SendError(c, common.NewInternalServerError("failed to get subnet pool"))
		return
	}

	pool := gin.H{
		"id":            id,
		"name":          name,
		"prefixes":      prefixes,
		"min_prefixlen": minPrefixlen,
		"max_prefixlen": maxPrefixlen,
		"shared":        shared,
		"is_default":    isDefault,
		"ip_version":    ipVersion,
		"tenant_id":     projectID,
		"created_at":    createdAt.Format(time.RFC3339),
		"updated_at":    updatedAt.Format(time.RFC3339),
	}

	if defaultPrefixlen != nil {
		pool["default_prefixlen"] = *defaultPrefixlen
	}

	c.JSON(http.StatusOK, gin.H{"subnetpool": pool})
}

// UpdateSubnetPool updates a subnet pool
func (svc *Service) UpdateSubnetPool(c *gin.Context) {
	poolID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		SubnetPool struct {
			Name             *string  `json:"name"`
			Prefixes         []string `json:"prefixes"`
			MinPrefixlen     *int     `json:"min_prefixlen"`
			MaxPrefixlen     *int     `json:"max_prefixlen"`
			DefaultPrefixlen *int     `json:"default_prefixlen"`
		} `json:"subnetpool"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	u := &common.SQLUpdate{}

	if req.SubnetPool.Name != nil {
		u.Set("name", *req.SubnetPool.Name)
	}

	if len(req.SubnetPool.Prefixes) > 0 {
		u.Set("prefixes", req.SubnetPool.Prefixes)
	}

	if req.SubnetPool.MinPrefixlen != nil {
		u.Set("min_prefixlen", *req.SubnetPool.MinPrefixlen)
	}

	if req.SubnetPool.MaxPrefixlen != nil {
		u.Set("max_prefixlen", *req.SubnetPool.MaxPrefixlen)
	}

	if req.SubnetPool.DefaultPrefixlen != nil {
		u.Set("default_prefixlen", *req.SubnetPool.DefaultPrefixlen)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	u.Set("updated_at", time.Now())

	query := "UPDATE subnet_pools SET " + u.Assignments() +
		" WHERE id = " + u.Arg(poolID) + " AND project_id = " + u.Arg(projectID)

	result, err := svc.pool.Exec(c.Request.Context(), query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet_pool").Msg("failed to update subnet pool")
		common.SendError(c, common.NewInternalServerError("failed to update subnet pool"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("subnet pool"))
		return
	}

	// Return updated pool
	svc.GetSubnetPool(c)
}

// DeleteSubnetPool deletes a subnet pool
func (svc *Service) DeleteSubnetPool(c *gin.Context) {
	poolID := c.Param("id")
	projectID := c.GetString("project_id")

	result, err := svc.pool.Exec(c.Request.Context(), `
		DELETE FROM subnet_pools
		WHERE id = $1 AND project_id = $2
	`, poolID, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_subnet_pool").Msg("failed to delete subnet pool")
		common.SendError(c, common.NewInternalServerError("failed to delete subnet pool"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("subnet pool"))
		return
	}

	c.Status(http.StatusNoContent)
}
