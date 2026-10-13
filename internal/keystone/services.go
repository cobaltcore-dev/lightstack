package keystone

import (
	"errors"
	"fmt"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListServices returns all services in the catalog
func (svc *Service) ListServices(c *gin.Context) {
	var f listFilter
	f.eq(c, "type", "type")
	f.eq(c, "name", "name")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, type, name, description, enabled, created_at, updated_at FROM services"+
			f.Where()+" ORDER BY name", f.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_services").Msg("Failed to query services")
		common.SendError(c, common.NewInternalServerError("failed to query services"))
		return
	}
	defer rows.Close()

	services := []map[string]any{}
	for rows.Next() {
		var id, svcType, name string
		var description *string
		var enabled bool
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &svcType, &name, &description, &enabled, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Str("operation", "ListServices").Msg("failed to scan row")
			continue
		}

		service := map[string]any{
			"id":      id,
			"type":    svcType,
			"name":    name,
			"enabled": enabled,
		}

		if description != nil {
			service["description"] = *description
		}

		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(200, gin.H{"services": services})
}

// CreateService creates a new service in the catalog
func (svc *Service) CreateService(c *gin.Context) {
	var req struct {
		Service struct {
			Type        string `json:"type" binding:"required"`
			Name        string `json:"name" binding:"required"`
			Description string `json:"description"`
			Enabled     *bool  `json:"enabled"`
		} `json:"service"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	enabled := true
	if req.Service.Enabled != nil {
		enabled = *req.Service.Enabled
	}

	serviceID := uuid.New()
	now := time.Now()

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO services (id, type, name, description, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, serviceID, req.Service.Type, req.Service.Name, req.Service.Description, enabled, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_service").Msg("Failed to create service")
		common.SendError(c, common.NewInternalServerError("failed to create service"))
		return
	}

	service := map[string]any{
		"id":      serviceID.String(),
		"type":    req.Service.Type,
		"name":    req.Service.Name,
		"enabled": enabled,
	}

	if req.Service.Description != "" {
		service["description"] = req.Service.Description
	}

	c.JSON(201, gin.H{"service": service})
}

// GetService returns a specific service by ID
func (svc *Service) GetService(c *gin.Context) {
	serviceID := c.Param("id")

	var id, svcType, name string
	var description *string
	var enabled bool

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, type, name, description, enabled
		FROM services
		WHERE id = $1
	`, serviceID).Scan(&id, &svcType, &name, &description, &enabled)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewIdentityNotFoundError("service", serviceID))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_service").Str("service_id", serviceID).Msg("Failed to query service")
		common.SendError(c, common.NewInternalServerError("failed to query service"))
		return
	}

	service := map[string]any{
		"id":      id,
		"type":    svcType,
		"name":    name,
		"enabled": enabled,
	}

	if description != nil {
		service["description"] = *description
	}

	c.JSON(200, gin.H{"service": service})
}

// DeleteService deletes a service from the catalog
func (svc *Service) DeleteService(c *gin.Context) {
	serviceID := c.Param("id")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM services WHERE id = $1",
		serviceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_service").Str("service_id", serviceID).Msg("Failed to delete service")
		common.SendError(c, common.NewInternalServerError("failed to delete service"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("service", serviceID))
		return
	}

	c.Status(204)
}

// UpdateService updates a service (PATCH /v3/services/:id)
func (svc *Service) UpdateService(c *gin.Context) {
	serviceID := c.Param("id")

	// Parse request body
	var req struct {
		Service struct {
			Name        *string `json:"name,omitempty"`
			Description *string `json:"description,omitempty"`
			Enabled     *bool   `json:"enabled,omitempty"`
		} `json:"service"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Build dynamic UPDATE query
	u := &common.SQLUpdate{}

	if req.Service.Name != nil {
		u.Set("name", *req.Service.Name)
	}
	if req.Service.Description != nil {
		u.Set("description", *req.Service.Description)
	}
	if req.Service.Enabled != nil {
		u.Set("enabled", *req.Service.Enabled)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no fields to update"))
		return
	}

	// Always update updated_at
	u.Set("updated_at", time.Now())

	query := `
		UPDATE services
		SET ` + u.Assignments() + `
		WHERE id = ` + u.Arg(serviceID) + `
		RETURNING id, type, name, description, enabled, created_at, updated_at
	`

	// Execute update
	var id, svcType, name string
	var description *string
	var enabled bool
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), query, u.Args()...).Scan(
		&id, &svcType, &name, &description, &enabled, &createdAt, &updatedAt,
	)

	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			common.SendError(c, common.NewIdentityNotFoundError("service", serviceID))
			return
		}
		log.Error().Err(err).Str("operation", "update_service").Str("service_id", serviceID).Msg("Failed to update service")
		common.SendError(c, common.NewInternalServerError("failed to update service"))
		return
	}

	// Build response
	service := map[string]any{
		"id":      id,
		"type":    svcType,
		"name":    name,
		"enabled": enabled,
		"links": map[string]any{
			"self": fmt.Sprintf("%s/v3/services/%s", common.BaseURL(c, 35357), id),
		},
	}

	if description != nil {
		service["description"] = *description
	}

	c.JSON(200, gin.H{"service": service})
}

// ListEndpoints returns all endpoints in the catalog
func (svc *Service) ListEndpoints(c *gin.Context) {
	var f listFilter
	f.eq(c, "interface", "e.interface")
	f.eqID(c, "service_id", "e.service_id")
	f.eq(c, "region_id", "e.region")
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT e.id, e.service_id, e.interface, e.url, e.region, e.enabled, s.type, s.name "+
			"FROM endpoints e JOIN services s ON e.service_id = s.id"+f.Where()+
			" ORDER BY s.name, e.interface", f.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_endpoints").Msg("Failed to query endpoints")
		common.SendError(c, common.NewInternalServerError("failed to query endpoints"))
		return
	}
	defer rows.Close()

	endpoints := []map[string]any{}
	for rows.Next() {
		var id, serviceID, iface, url string
		var region *string
		var enabled bool
		var serviceType, serviceName string

		if err := rows.Scan(&id, &serviceID, &iface, &url, &region, &enabled, &serviceType, &serviceName); err != nil {
			log.Warn().Err(err).Str("operation", "ListEndpoints").Msg("failed to scan row")
			continue
		}

		endpoint := map[string]any{
			"id":         id,
			"service_id": serviceID,
			"interface":  iface,
			"url":        url,
			"enabled":    enabled,
		}

		if region != nil {
			endpoint["region"] = *region
		}

		endpoints = append(endpoints, endpoint)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(200, gin.H{"endpoints": endpoints})
}

// CreateEndpoint creates a new endpoint in the catalog
func (svc *Service) CreateEndpoint(c *gin.Context) {
	var req struct {
		Endpoint struct {
			ServiceID string `json:"service_id" binding:"required"`
			Interface string `json:"interface" binding:"required"`
			URL       string `json:"url" binding:"required"`
			Region    string `json:"region"`
			Enabled   *bool  `json:"enabled"`
		} `json:"endpoint"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	enabled := true
	if req.Endpoint.Enabled != nil {
		enabled = *req.Endpoint.Enabled
	}

	endpointID := uuid.New()

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO endpoints (id, service_id, interface, url, region, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, endpointID, req.Endpoint.ServiceID, req.Endpoint.Interface, req.Endpoint.URL, req.Endpoint.Region, enabled)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_endpoint").Msg("Failed to create endpoint")
		common.SendError(c, common.NewInternalServerError("failed to create endpoint"))
		return
	}

	endpoint := map[string]any{
		"id":         endpointID.String(),
		"service_id": req.Endpoint.ServiceID,
		"interface":  req.Endpoint.Interface,
		"url":        req.Endpoint.URL,
		"enabled":    enabled,
	}

	if req.Endpoint.Region != "" {
		endpoint["region"] = req.Endpoint.Region
	}

	c.JSON(201, gin.H{"endpoint": endpoint})
}

// DeleteEndpoint deletes an endpoint from the catalog
func (svc *Service) DeleteEndpoint(c *gin.Context) {
	endpointID := c.Param("id")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM endpoints WHERE id = $1",
		endpointID)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_endpoint").Str("endpoint_id", endpointID).Msg("Failed to delete endpoint")
		common.SendError(c, common.NewInternalServerError("failed to delete endpoint"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewIdentityNotFoundError("endpoint", endpointID))
		return
	}

	c.Status(204)
}
