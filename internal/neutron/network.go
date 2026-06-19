package neutron

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/cache"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// Service handles Neutron API endpoints
type Service struct {
	mode             string
	nsManager        *networking.NetworkNamespaceManager
	brManager        *networking.BridgeManager
	tapManager       *networking.TAPDeviceManager
	dhcpManager      *networking.DHCPManager
	sgManager        *networking.SecurityGroupManager
	routerManager    *networking.RouterManager
	vxlanCoordinator *VXLANCoordinator
	cache            *cache.Cache
}

// NewService creates a new Neutron service
func NewService(mode string, cacheInstance *cache.Cache) *Service {
	sgManager, err := networking.NewSecurityGroupManager(mode, "")
	if err != nil {
		log.Error().Err(err).Str("mode", mode).Msg("failed to initialize security group manager")
	}
	return &Service{
		mode:          mode,
		nsManager:     networking.NewNetworkNamespaceManager(mode),
		brManager:     networking.NewBridgeManager(mode),
		tapManager:    networking.NewTAPDeviceManager(mode),
		dhcpManager:   networking.NewDHCPManager(mode),
		sgManager:     sgManager,
		routerManager: networking.NewRouterManager(mode),
		cache:         cacheInstance,
	}
}

// RegisterRoutes registers Neutron routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Version discovery
	r.GET("/v2.0", svc.GetVersion)

	v2 := r.Group("/v2.0")

	// Add request logging middleware for debugging
	v2.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		method := c.Request.Method

		// Log incoming request
		c.Set("neutron_request_start", time.Now())
		log.Debug().
			Str("method", method).
			Str("path", path).
			Str("project_id", c.GetString("project_id")).
			Str("query", c.Request.URL.RawQuery).
			Msg("NEUTRON: Incoming request")

		c.Next()

		// Log response
		status := c.Writer.Status()
		duration := time.Since(c.GetTime("neutron_request_start"))
		log.Debug().
			Int("status", status).
			Str("method", method).
			Str("path", path).
			Dur("duration", duration).
			Msg("NEUTRON: Response")
	})
	{
		// Extensions
		v2.GET("/extensions", svc.ListExtensions)

		// Availability Zones
		v2.GET("/availability_zones", svc.ListAvailabilityZones)

		// Service Providers
		v2.GET("/service-providers", svc.ListServiceProviders)

		// Networks
		v2.GET("/networks", svc.ListNetworks)
		v2.POST("/networks", svc.CreateNetwork)
		v2.GET("/networks/:id", svc.GetNetwork)
		v2.DELETE("/networks/:id", svc.DeleteNetwork)
		v2.PUT("/networks/:id", svc.UpdateNetwork)
		v2.PATCH("/networks/:id", svc.UpdateNetwork)

		// Subnets
		v2.GET("/subnets", svc.ListSubnets)
		v2.POST("/subnets", svc.CreateSubnet)
		v2.GET("/subnets/:id", svc.GetSubnet)
		v2.DELETE("/subnets/:id", svc.DeleteSubnet)
		v2.PUT("/subnets/:id", svc.UpdateSubnet)
		v2.PATCH("/subnets/:id", svc.UpdateSubnet)

		// Ports
		v2.GET("/ports", svc.ListPorts)
		v2.POST("/ports", svc.CreatePort)
		v2.GET("/ports/:id", svc.GetPort)
		v2.DELETE("/ports/:id", svc.DeletePort)
		v2.PUT("/ports/:id", svc.UpdatePort)
		v2.PATCH("/ports/:id", svc.UpdatePort)

		// Security Groups
		v2.GET("/security-groups", svc.ListSecurityGroups)
		v2.POST("/security-groups", svc.CreateSecurityGroup)
		v2.GET("/security-groups/:id", svc.GetSecurityGroup)
		v2.DELETE("/security-groups/:id", svc.DeleteSecurityGroup)
		v2.PUT("/security-groups/:id", svc.UpdateSecurityGroup)
		v2.PATCH("/security-groups/:id", svc.UpdateSecurityGroup)

		// Security Group Rules
		v2.GET("/security-group-rules", svc.ListSecurityGroupRules)
		v2.POST("/security-group-rules", svc.CreateSecurityGroupRule)
		v2.DELETE("/security-group-rules/:id", svc.DeleteSecurityGroupRule)

		// Routers
		v2.GET("/routers", svc.ListRouters)
		v2.POST("/routers", svc.CreateRouter)
		v2.GET("/routers/:id", svc.GetRouter)
		v2.DELETE("/routers/:id", svc.DeleteRouter)
		v2.PUT("/routers/:id", svc.UpdateRouter)

		// Router Interfaces
		v2.PUT("/routers/:id/add_router_interface", svc.AddRouterInterface)
		v2.PUT("/routers/:id/remove_router_interface", svc.RemoveRouterInterface)

		// Floating IPs
		v2.GET("/floatingips", svc.ListFloatingIPs)
		v2.POST("/floatingips", svc.CreateFloatingIP)
		v2.GET("/floatingips/:id", svc.GetFloatingIP)
		v2.PUT("/floatingips/:id", svc.UpdateFloatingIP)
		v2.DELETE("/floatingips/:id", svc.DeleteFloatingIP)

		// Port Forwarding (nested under floatingips)
		v2.GET("/floatingips/:id/port_forwardings", svc.ListPortForwardings)
		v2.POST("/floatingips/:id/port_forwardings", svc.CreatePortForwarding)
		v2.GET("/floatingips/:id/port_forwardings/:pf_id", svc.GetPortForwarding)
		v2.PUT("/floatingips/:id/port_forwardings/:pf_id", svc.UpdatePortForwarding)
		v2.DELETE("/floatingips/:id/port_forwardings/:pf_id", svc.DeletePortForwarding)
		// Quotas
		v2.GET("/quotas/:id", svc.GetQuota)
		v2.GET("/quotas/:id/details", svc.GetQuotaDetails)

		// QoS Policies
		v2.GET("/qos/policies", svc.ListQoSPolicies)
		v2.POST("/qos/policies", svc.CreateQoSPolicy)
		v2.GET("/qos/policies/:id", svc.GetQoSPolicy)
		v2.PUT("/qos/policies/:id", svc.UpdateQoSPolicy)
		v2.DELETE("/qos/policies/:id", svc.DeleteQoSPolicy)

		// QoS Bandwidth Limit Rules
		v2.GET("/qos/policies/:id/bandwidth_limit_rules", svc.ListBandwidthLimitRules)
		v2.POST("/qos/policies/:id/bandwidth_limit_rules", svc.CreateBandwidthLimitRule)
		v2.GET("/qos/policies/:id/bandwidth_limit_rules/:rule_id", svc.GetBandwidthLimitRule)
		v2.PUT("/qos/policies/:id/bandwidth_limit_rules/:rule_id", svc.UpdateBandwidthLimitRule)
		v2.DELETE("/qos/policies/:id/bandwidth_limit_rules/:rule_id", svc.DeleteBandwidthLimitRule)

		// RBAC Policies
		v2.POST("/rbac-policies", svc.CreateRBACPolicy)
		v2.GET("/rbac-policies", svc.ListRBACPolicies)
		v2.GET("/rbac-policies/:id", svc.GetRBACPolicy)
		v2.PUT("/rbac-policies/:id", svc.UpdateRBACPolicy)
		v2.DELETE("/rbac-policies/:id", svc.DeleteRBACPolicy)

		// Trunk Ports
		v2.GET("/trunks", svc.ListTrunks)
		v2.POST("/trunks", svc.CreateTrunk)
		v2.GET("/trunks/:id", svc.GetTrunk)
		v2.PUT("/trunks/:id", svc.UpdateTrunk)
		v2.DELETE("/trunks/:id", svc.DeleteTrunk)
		v2.PUT("/trunks/:id/add_subports", svc.AddSubports)
		v2.PUT("/trunks/:id/remove_subports", svc.RemoveSubports)

		// Address Scopes
		v2.GET("/address-scopes", svc.ListAddressScopes)
		v2.POST("/address-scopes", svc.CreateAddressScope)
		v2.GET("/address-scopes/:id", svc.GetAddressScope)
		v2.PUT("/address-scopes/:id", svc.UpdateAddressScope)
		v2.DELETE("/address-scopes/:id", svc.DeleteAddressScope)

		// Subnet Pools
		v2.GET("/subnetpools", svc.ListSubnetPools)
		v2.POST("/subnetpools", svc.CreateSubnetPool)
		v2.GET("/subnetpools/:id", svc.GetSubnetPool)
		v2.PUT("/subnetpools/:id", svc.UpdateSubnetPool)
		v2.DELETE("/subnetpools/:id", svc.DeleteSubnetPool)

		// Auto-Allocated Topology
		v2.GET("/auto-allocated-topology/:project", svc.GetAutoAllocatedTopology)
		v2.POST("/auto-allocated-topology/:project", svc.CreateAutoAllocatedTopology)
		v2.DELETE("/auto-allocated-topology/:project", svc.DeleteAutoAllocatedTopology)

		// Network IP Availability
		v2.GET("/network-ip-availabilities", svc.ListNetworkIPAvailabilities)
		v2.GET("/network-ip-availabilities/:id", svc.GetNetworkIPAvailability)

		// Metering
		v2.GET("/metering/metering-labels", svc.ListMeteringLabels)
		v2.POST("/metering/metering-labels", svc.CreateMeteringLabel)
		v2.GET("/metering/metering-labels/:id", svc.GetMeteringLabel)
		v2.DELETE("/metering/metering-labels/:id", svc.DeleteMeteringLabel)
		v2.GET("/metering/metering-label-rules", svc.ListMeteringLabelRules)
		v2.POST("/metering/metering-label-rules", svc.CreateMeteringLabelRule)
		v2.DELETE("/metering/metering-label-rules/:id", svc.DeleteMeteringLabelRule)

		// Agents
		v2.GET("/agents", svc.ListAgents)
		v2.GET("/agents/:id", svc.GetAgent)
		v2.GET("/routers/:id/l3-agents", svc.ListL3AgentsOnRouter)
		v2.POST("/routers/:id/l3-agents", svc.AddL3AgentToRouter)
	}
}

// SetVXLANCoordinator sets the VXLAN coordinator for this service
func (svc *Service) SetVXLANCoordinator(coordinator *VXLANCoordinator) {
	svc.vxlanCoordinator = coordinator
}

// GetVersion returns Neutron version information
func (svc *Service) GetVersion(c *gin.Context) {
	c.JSON(200, gin.H{
		"version": gin.H{
			"id":     "v2.0",
			"status": "CURRENT",
			"links": []gin.H{
				{"rel": "self", "href": "/v2.0"},
			},
		},
	})
}

// ListExtensions lists available API extensions
func (svc *Service) ListExtensions(c *gin.Context) {
	// Return list of supported extensions
	// Horizon uses this to determine feature availability
	c.JSON(200, gin.H{
		"extensions": []gin.H{
			{
				"alias":       "security-group",
				"name":        "security-group",
				"description": "The security groups extension",
				"updated":     "2012-10-05T10:00:00-00:00",
			},
			{
				"alias":       "router",
				"name":        "Neutron L3 Router",
				"description": "Router abstraction for basic L3 forwarding",
				"updated":     "2012-07-20T10:00:00-00:00",
			},
			{
				"alias":       "port-security",
				"name":        "Port Security",
				"description": "Port security extension",
				"updated":     "2012-07-23T10:00:00-00:00",
			},
			{
				"alias":       "binding",
				"name":        "Port Binding",
				"description": "Expose port bindings of a virtual port to external application",
				"updated":     "2014-02-03T10:00:00-00:00",
			},
			{
				"alias":       "provider",
				"name":        "Provider Network",
				"description": "Provider network extension",
				"updated":     "2012-09-07T10:00:00-00:00",
			},
			{
				"alias":       "quotas",
				"name":        "Quota management support",
				"description": "Expose resource quotas",
				"updated":     "2012-07-29T10:00:00-00:00",
			},
		},
	})
}

// GetQuota returns network quotas for a project (limits only; use
// GetQuotaDetails for limits + usage).
func (svc *Service) GetQuota(c *gin.Context) {
	// Return quota response
	c.JSON(200, gin.H{
		"quota": gin.H{
			"network":             100,
			"subnet":              100,
			"port":                500,
			"router":              10,
			"floatingip":          50,
			"security_group":      100,
			"security_group_rule": 500,
		},
	})
}

// GetQuotaDetails returns network quotas with usage details
func (svc *Service) GetQuotaDetails(c *gin.Context) {
	projectID := c.Param("id")

	// Query current usage. Scan failures degrade to zero (the quota
	// response still ships) and are logged for diagnosis.
	var networksUsed, subnetsUsed, portsUsed, routersUsed, floatingIPsUsed, securityGroupsUsed int

	usageQueries := []struct {
		label string
		sql   string
		dst   *int
	}{
		{"networks", "SELECT COUNT(*) FROM networks WHERE project_id = $1", &networksUsed},
		{"subnets", "SELECT COUNT(*) FROM subnets WHERE project_id = $1", &subnetsUsed},
		{"ports", "SELECT COUNT(*) FROM ports WHERE project_id = $1", &portsUsed},
		{"routers", "SELECT COUNT(*) FROM routers WHERE project_id = $1", &routersUsed},
		{"floating_ips", "SELECT COUNT(*) FROM floating_ips WHERE project_id = $1", &floatingIPsUsed},
		{"security_groups", "SELECT COUNT(*) FROM security_groups WHERE project_id = $1", &securityGroupsUsed},
	}
	for _, q := range usageQueries {
		if err := database.DB.QueryRow(c.Request.Context(), q.sql, projectID).Scan(q.dst); err != nil {
			log.Warn().Err(err).Str("resource", q.label).Str("project_id", projectID).Msg("failed to query quota usage")
		}
	}

	// Return quota details with usage
	c.JSON(200, gin.H{
		"quota": gin.H{
			"network": gin.H{
				"limit": 100,
				"used":  networksUsed,
			},
			"subnet": gin.H{
				"limit": 100,
				"used":  subnetsUsed,
			},
			"port": gin.H{
				"limit": 500,
				"used":  portsUsed,
			},
			"router": gin.H{
				"limit": 10,
				"used":  routersUsed,
			},
			"floatingip": gin.H{
				"limit": 50,
				"used":  floatingIPsUsed,
			},
			"security_group": gin.H{
				"limit": 100,
				"used":  securityGroupsUsed,
			},
			"security_group_rule": gin.H{
				"limit": 500,
				"used":  0,
			},
		},
	})
}

// GetNamespaceManager returns the namespace manager (for VXLAN coordinator)
func (svc *Service) GetNamespaceManager() *networking.NetworkNamespaceManager {
	return svc.nsManager
}

// CreateNetworkRequest represents a network creation request
type CreateNetworkRequest struct {
	Network struct {
		Name         string `json:"name" binding:"required"`
		AdminStateUp *bool  `json:"admin_state_up"`
		Shared       *bool  `json:"shared"`
	} `json:"network"`
}

// CreateNetwork creates a new network
func (svc *Service) CreateNetwork(c *gin.Context) {
	var req CreateNetworkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	networkID := uuid.New().String()
	bridgeName := "br-" + networkID[:8]

	adminStateUp := true
	if req.Network.AdminStateUp != nil {
		adminStateUp = *req.Network.AdminStateUp
	}

	shared := false
	if req.Network.Shared != nil {
		shared = *req.Network.Shared
	}

	// Create bridge in default namespace (not project namespace) for libvirt access
	if err := svc.brManager.CreateBridge(bridgeName, false, ""); err != nil {
		log.Error().Err(err).Str("operation", "create_bridge").Str("bridge", bridgeName).Msg("failed to create bridge")
		common.SendError(c, common.NewInternalServerError("failed to create bridge"))
		return
	}

	// Insert into database
	now := time.Now()

	// Determine network type based on VXLAN coordinator
	networkType := "flat"
	mtu := 1500
	if svc.vxlanCoordinator != nil {
		networkType = "vxlan"
		mtu = 1450 // Account for VXLAN overhead
	}

	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, networkID, req.Network.Name, projectID, adminStateUp, "ACTIVE", shared, networkType, mtu, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_network").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create network"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"network": gin.H{
			"id":             networkID,
			"name":           req.Network.Name,
			"tenant_id":      projectID,
			"admin_state_up": adminStateUp,
			"status":         "ACTIVE",
			"shared":         shared,
			"mtu":            mtu,
			"subnets":        []string{},
			"created_at":     now.Format(time.RFC3339),
			"updated_at":     now.Format(time.RFC3339),
		},
	})
}

// ListNetworks lists all networks
func (svc *Service) ListNetworks(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000
	offset := 0
	if limitParam := c.Query("limit"); limitParam != "" {
		if parsedLimit, err := strconv.Atoi(limitParam); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if offsetParam := c.Query("offset"); offsetParam != "" {
		if parsedOffset, err := strconv.Atoi(offsetParam); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT created_at, id FROM networks WHERE id = $1",
			marker,
		).Scan(&markerCreatedAt, &markerID)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", argIdx, argIdx+1)
			queryArgs = append(queryArgs, markerCreatedAt, markerID)
			argIdx += 2
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := database.DB.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at
		FROM networks
		WHERE (project_id = $1 OR shared = true)%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_networks").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list networks"))
		return
	}
	defer rows.Close()

	var networks []gin.H
	for rows.Next() {
		var id, name, status string
		var adminStateUp, shared bool
		var mtu int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt); err != nil {
			continue
		}

		networks = append(networks, gin.H{
			"id":             id,
			"name":           name,
			"tenant_id":      projectID,
			"admin_state_up": adminStateUp,
			"status":         status,
			"shared":         shared,
			"mtu":            mtu,
			"created_at":     createdAt.Format(time.RFC3339),
			"updated_at":     updatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}

	if networks == nil {
		networks = []gin.H{}
	}

	// Populate subnet IDs for each network (rows above are fully consumed).
	for _, n := range networks {
		n["subnets"] = svc.networkSubnetIDs(c.Request.Context(), n["id"].(string))
	}

	c.JSON(http.StatusOK, gin.H{"networks": networks})
}

// GetNetwork returns a single network
func (svc *Service) GetNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Try cache first
	if svc.cache != nil {
		cacheKey := "network:" + networkID
		var cached gin.H
		if err := svc.cache.Get(ctx, cacheKey, &cached); err == nil {
			// subnets are queried fresh so they reflect later subnet changes
			if id, ok := cached["id"].(string); ok {
				cached["subnets"] = svc.networkSubnetIDs(ctx, id)
			}
			c.JSON(http.StatusOK, gin.H{"network": cached})
			return
		}
	}

	// Cache miss - query database
	var id, name, status string
	var adminStateUp, shared bool
	var mtu int
	var createdAt, updatedAt time.Time

	err := database.DB.QueryRow(ctx, `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at
		FROM networks
		WHERE (id::text = $1 OR name = $1) AND (project_id = $2 OR shared = true)
		LIMIT 1
	`, networkID, projectID).Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get network"))
		return
	}

	network := gin.H{
		"id":             id,
		"name":           name,
		"tenant_id":      projectID,
		"admin_state_up": adminStateUp,
		"status":         status,
		"shared":         shared,
		"mtu":            mtu,
		"created_at":     createdAt.Format(time.RFC3339),
		"updated_at":     updatedAt.Format(time.RFC3339),
	}

	// Store in cache (30min TTL per config) before adding the dynamic subnet
	// list, so cached entries never carry a stale subnets field.
	if svc.cache != nil {
		svc.cache.TrySet(ctx, "network:"+id, network, 30*time.Minute)
	}

	network["subnets"] = svc.networkSubnetIDs(ctx, id)

	c.JSON(http.StatusOK, gin.H{"network": network})
}

// DeleteNetwork deletes a network
func (svc *Service) DeleteNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Check for active ports or subnets before deleting
	var portCount int
	if err := database.DB.QueryRow(ctx,
		"SELECT COUNT(*) FROM ports WHERE network_id = $1", networkID,
	).Scan(&portCount); err == nil && portCount > 0 {
		common.SendError(c, common.NewOperationConflictError("delete network", "network has active ports"))
		return
	}

	var subnetCount int
	if err := database.DB.QueryRow(ctx,
		"SELECT COUNT(*) FROM subnets WHERE network_id = $1", networkID,
	).Scan(&subnetCount); err == nil && subnetCount > 0 {
		common.SendError(c, common.NewOperationConflictError("delete network", "network has subnets; delete subnets first"))
		return
	}

	bridgeName := "br-" + networkID[:8]
	nsName := svc.nsManager.GetNamespaceName(projectID)

	// Delete bridge (best-effort: a leftover bridge is hygiene, not
	// correctness — the network row is about to be deleted).
	if err := svc.brManager.DeleteBridge(bridgeName, true, nsName); err != nil {
		log.Warn().Err(err).Str("bridge", bridgeName).Str("namespace", nsName).Msg("failed to delete network bridge")
	}

	// Delete from database
	_, err := database.DB.Exec(ctx,
		"DELETE FROM networks WHERE id = $1 AND project_id = $2",
		networkID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}

	// Invalidate cache
	if svc.cache != nil {
		svc.cache.TryDelete(ctx, "network:"+networkID)
		svc.cache.TryDeletePattern(ctx, "networks:*")
	}

	c.Status(http.StatusNoContent)
}

// UpdateNetwork updates a network
func (svc *Service) UpdateNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Network struct {
			Name         *string `json:"name"`
			AdminStateUp *bool   `json:"admin_state_up"`
		} `json:"network"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	updates := []string{}
	args := []any{}
	argID := 1

	if req.Network.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", argID))
		args = append(args, *req.Network.Name)
		argID++
	}

	if req.Network.AdminStateUp != nil {
		updates = append(updates, fmt.Sprintf("admin_state_up = $%d", argID))
		args = append(args, *req.Network.AdminStateUp)
		argID++
	}

	if len(updates) == 0 {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	updates = append(updates, fmt.Sprintf("updated_at = $%d", argID))
	args = append(args, time.Now())
	argID++

	args = append(args, networkID, projectID)

	query := fmt.Sprintf("UPDATE networks SET %s WHERE id = $%d AND project_id = $%d",
		updateString(updates), argID, argID+1)

	_, err := database.DB.Exec(c.Request.Context(), query, args...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update network"))
		return
	}

	// Return updated network
	svc.GetNetwork(c)
}

// CreateSubnetRequest represents a subnet creation request
type CreateSubnetRequest struct {
	Subnet struct {
		Name           string   `json:"name"`
		NetworkID      string   `json:"network_id" binding:"required"`
		CIDR           string   `json:"cidr" binding:"required"`
		GatewayIP      string   `json:"gateway_ip"`
		IPVersion      int      `json:"ip_version"`
		EnableDHCP     *bool    `json:"enable_dhcp"`
		DNSNameservers []string `json:"dns_nameservers"`
	} `json:"subnet"`
}

// CreateSubnet creates a new subnet
func (svc *Service) CreateSubnet(c *gin.Context) {
	var req CreateSubnetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	subnetID := uuid.New().String()

	ipVersion := 4
	if req.Subnet.IPVersion > 0 {
		ipVersion = req.Subnet.IPVersion
	}

	enableDHCP := true
	if req.Subnet.EnableDHCP != nil {
		enableDHCP = *req.Subnet.EnableDHCP
	}

	// Parse CIDR to calculate gateway if not provided
	_, ipNet, err := net.ParseCIDR(req.Subnet.CIDR)
	if err != nil {
		common.SendError(c, common.NewBadRequestError("invalid CIDR"))
		return
	}

	gatewayIP := req.Subnet.GatewayIP
	if gatewayIP == "" {
		// Use first IP as gateway
		gatewayIP = incrementIP(ipNet.IP, 1).String()
	}

	// Insert into database
	now := time.Now()
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, dns_nameservers, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, subnetID, req.Subnet.Name, req.Subnet.NetworkID, projectID, req.Subnet.CIDR, gatewayIP, ipVersion, enableDHCP, req.Subnet.DNSNameservers, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_subnet").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create subnet"))
		return
	}

	// Start DHCP server if enabled
	if enableDHCP {
		bridgeName := "br-" + req.Subnet.NetworkID[:8]
		// The bridge lives in the default namespace (for libvirt access), so the
		// gateway IP must sit on it and dnsmasq must run there too — otherwise it
		// has no address in the subnet to serve (SPEC-009 WS-2).
		ones, _ := ipNet.Mask.Size()
		if err := svc.brManager.EnsureBridgeIP(bridgeName, fmt.Sprintf("%s/%d", gatewayIP, ones)); err != nil {
			log.Warn().Err(err).Str("bridge", bridgeName).Msg("failed to assign gateway IP to bridge")
		}

		dhcpConfig := networking.DHCPConfig{
			NetworkID:      req.Subnet.NetworkID,
			BridgeName:     bridgeName,
			SubnetCIDR:     req.Subnet.CIDR,
			GatewayIP:      gatewayIP,
			DNSServers:     req.Subnet.DNSNameservers,
			DHCPRangeStart: incrementIP(ipNet.IP, 10).String(),
			DHCPRangeEnd:   incrementIP(ipNet.IP, 250).String(),
			LeaseTime:      "24h",
		}

		go func() {
			if err := svc.dhcpManager.StartDHCP(dhcpConfig, ""); err != nil {
				log.Warn().Err(err).Str("network_id", dhcpConfig.NetworkID).Msg("failed to start DHCP server for subnet")
			}
		}()
	}

	// OpenStack clients format these as lists and fail on JSON null, so
	// default an omitted dns_nameservers to an empty slice.
	dnsNameservers := req.Subnet.DNSNameservers
	if dnsNameservers == nil {
		dnsNameservers = []string{}
	}

	c.JSON(http.StatusCreated, gin.H{
		"subnet": gin.H{
			"id":               subnetID,
			"name":             req.Subnet.Name,
			"network_id":       req.Subnet.NetworkID,
			"tenant_id":        projectID,
			"cidr":             req.Subnet.CIDR,
			"gateway_ip":       gatewayIP,
			"ip_version":       ipVersion,
			"enable_dhcp":      enableDHCP,
			"dns_nameservers":  dnsNameservers,
			"allocation_pools": subnetAllocationPools(req.Subnet.CIDR),
			"host_routes":      []gin.H{},
			"service_types":    []string{},
			"created_at":       now.Format(time.RFC3339),
			"updated_at":       now.Format(time.RFC3339),
		},
	})
}

// ListSubnets lists all subnets
func (svc *Service) ListSubnets(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000
	offset := 0
	if limitParam := c.Query("limit"); limitParam != "" {
		if parsedLimit, err := strconv.Atoi(limitParam); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if offsetParam := c.Query("offset"); offsetParam != "" {
		if parsedOffset, err := strconv.Atoi(offsetParam); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT created_at FROM subnets WHERE id = $1",
			marker,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND s.created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := database.DB.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp, s.dns_nameservers, s.created_at, s.updated_at
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.project_id = $1 OR n.shared = true)%s
		ORDER BY s.created_at DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_subnets").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list subnets"))
		return
	}
	defer rows.Close()

	var subnets []gin.H
	for rows.Next() {
		var id, name, networkID, cidr, gatewayIP string
		var ipVersion int
		var enableDHCP bool
		var dnsNameservers []string
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &createdAt, &updatedAt); err != nil {
			continue
		}

		if dnsNameservers == nil {
			dnsNameservers = []string{}
		}

		subnets = append(subnets, gin.H{
			"id":               id,
			"name":             name,
			"network_id":       networkID,
			"tenant_id":        projectID,
			"cidr":             cidr,
			"gateway_ip":       gatewayIP,
			"ip_version":       ipVersion,
			"enable_dhcp":      enableDHCP,
			"dns_nameservers":  dnsNameservers,
			"allocation_pools": subnetAllocationPools(cidr),
			"host_routes":      []gin.H{},
			"service_types":    []string{},
			"created_at":       createdAt.Format(time.RFC3339),
			"updated_at":       updatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}

	if subnets == nil {
		subnets = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"subnets": subnets})
}

// GetSubnet returns a single subnet
func (svc *Service) GetSubnet(c *gin.Context) {
	subnetID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, networkID, cidr, gatewayIP string
	var ipVersion int
	var enableDHCP bool
	var dnsNameservers []string
	var createdAt, updatedAt time.Time

	// id::text comparison resolves by UUID or name without crashing on a
	// non-UUID identifier (the CLI looks up by name this way).
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp, s.dns_nameservers, s.created_at, s.updated_at
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.id::text = $1 OR s.name = $1) AND (s.project_id = $2 OR n.shared = true)
		LIMIT 1
	`, subnetID, projectID).Scan(&id, &name, &networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_subnet").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get subnet"))
		return
	}

	if dnsNameservers == nil {
		dnsNameservers = []string{}
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet": gin.H{
			"id":               id,
			"name":             name,
			"network_id":       networkID,
			"tenant_id":        projectID,
			"cidr":             cidr,
			"gateway_ip":       gatewayIP,
			"ip_version":       ipVersion,
			"enable_dhcp":      enableDHCP,
			"dns_nameservers":  dnsNameservers,
			"allocation_pools": subnetAllocationPools(cidr),
			"host_routes":      []gin.H{},
			"service_types":    []string{},
			"created_at":       createdAt.Format(time.RFC3339),
			"updated_at":       updatedAt.Format(time.RFC3339),
		},
	})
}

// DeleteSubnet deletes a subnet
func (svc *Service) DeleteSubnet(c *gin.Context) {
	subnetID := c.Param("id")
	projectID := c.GetString("project_id")

	// Resolve subnet by UUID or name to its canonical ID (and network for DHCP).
	var resolvedID, networkID string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id, network_id FROM subnets WHERE (id::text = $1 OR name = $1) AND project_id = $2 LIMIT 1",
		subnetID, projectID,
	).Scan(&resolvedID, &networkID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_subnet").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete subnet"))
		return
	}

	// Stop DHCP server (best-effort: a leftover dnsmasq is hygiene,
	// not correctness — the subnet row is about to be deleted).
	if err := svc.dhcpManager.StopDHCP(networkID); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("failed to stop DHCP server")
	}

	// Delete from database
	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM subnets WHERE id = $1 AND project_id = $2",
		resolvedID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_subnet").Str("subnet_id", resolvedID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete subnet"))
		return
	}

	c.Status(http.StatusNoContent)
}

// UpdateSubnet updates a subnet
func (svc *Service) UpdateSubnet(c *gin.Context) {
	subnetID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Subnet struct {
			Name *string `json:"name"`
		} `json:"subnet"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Resolve subnet by UUID or name to its canonical ID.
	var resolvedID, currentName string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id, name FROM subnets WHERE (id::text = $1 OR name = $1) AND project_id = $2 LIMIT 1",
		subnetID, projectID,
	).Scan(&resolvedID, &currentName)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return
	}

	// Apply updates
	if req.Subnet.Name != nil {
		currentName = *req.Subnet.Name
	}

	_, err = database.DB.Exec(c.Request.Context(),
		"UPDATE subnets SET name = $1 WHERE id = $2 AND project_id = $3",
		currentName, resolvedID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", resolvedID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return
	}

	// Return updated subnet
	var networkID, cidr, gatewayIP string
	var ipVersion int
	var enableDHCP bool
	var dnsNameservers []string
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT network_id, cidr, gateway_ip, ip_version, enable_dhcp, dns_nameservers FROM subnets WHERE id = $1",
		resolvedID,
	).Scan(&networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet_fetch").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to fetch updated subnet"))
		return
	}

	if dnsNameservers == nil {
		dnsNameservers = []string{}
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet": gin.H{
			"id":               resolvedID,
			"name":             currentName,
			"network_id":       networkID,
			"tenant_id":        projectID,
			"cidr":             cidr,
			"gateway_ip":       gatewayIP,
			"ip_version":       ipVersion,
			"enable_dhcp":      enableDHCP,
			"dns_nameservers":  dnsNameservers,
			"allocation_pools": subnetAllocationPools(cidr),
			"host_routes":      []gin.H{},
			"service_types":    []string{},
		},
	})
}

// Helper functions

// ListServiceProviders returns available service providers for VPN, Firewall, etc.
func (svc *Service) ListServiceProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"service_providers": []gin.H{
			{
				"name":         "default",
				"service_type": "L3_ROUTER_NAT",
				"default":      true,
			},
		},
	})
}

// ListAvailabilityZones returns availability zones for network resources
func (svc *Service) ListAvailabilityZones(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"availability_zones": []gin.H{
			{
				"name":  "nova",
				"state": "available",
			},
		},
	})
}

func incrementIP(ip net.IP, inc uint) net.IP {
	result := make(net.IP, len(ip))
	copy(result, ip)
	for i := len(result) - 1; i >= 0 && inc > 0; i-- {
		sum := uint(result[i]) + inc
		result[i] = byte(sum)
		inc = sum / 256
	}
	return result
}

func decrementIP(ip net.IP, dec uint) net.IP {
	result := make(net.IP, len(ip))
	copy(result, ip)
	for i := len(result) - 1; i >= 0 && dec > 0; i-- {
		if uint(result[i]) >= dec {
			result[i] -= byte(dec)
			dec = 0
		} else {
			dec -= uint(result[i])
			result[i] = byte(256 - (dec % 256))
			dec = 1 // Borrow from next byte
		}
	}
	return result
}

func broadcast(ipNet *net.IPNet) net.IP {
	ip := ipNet.IP.To4()
	mask := ipNet.Mask
	broadcast := make(net.IP, len(ip))
	for i := range ip {
		broadcast[i] = ip[i] | ^mask[i]
	}
	return broadcast
}

// networkSubnetIDs returns the subnet IDs belonging to a network, newest first.
// It always returns a non-nil slice so the JSON response serializes as a list,
// and is queried fresh (never cached) so network responses reflect subnet
// additions/removals immediately.
func (svc *Service) networkSubnetIDs(ctx context.Context, networkID string) []string {
	ids := []string{}
	rows, err := database.DB.Query(ctx,
		"SELECT id FROM subnets WHERE network_id = $1 ORDER BY created_at DESC", networkID)
	if err != nil {
		log.Error().Err(err).Str("network_id", networkID).Msg("failed to list subnets for network")
		return ids
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// subnetAllocationPools computes the default allocation pool for a subnet CIDR
// (network address + 2 through broadcast - 1). It always returns a non-nil
// slice so the JSON response serializes as a list, never null — OpenStack
// clients format allocation_pools as a list and choke on null.
func subnetAllocationPools(cidr string) []gin.H {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil || ipNet.IP.To4() == nil {
		return []gin.H{}
	}
	return []gin.H{
		{
			"start": incrementIP(ipNet.IP, 2).String(),
			"end":   decrementIP(broadcast(ipNet), 1).String(),
		},
	}
}

func updateString(updates []string) string {
	result := ""
	for i, update := range updates {
		if i > 0 {
			result += ", "
		}
		result += update
	}
	return result
}

func generateMAC() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	buf[0] = (buf[0] | 2) & 0xfe // Set local bit, clear multicast bit
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}
