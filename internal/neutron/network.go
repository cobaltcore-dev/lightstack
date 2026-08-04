package neutron

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Service handles Neutron API endpoints
type Service struct {
	pool             *pgxpool.Pool
	mode             string
	nsManager        *networking.NetworkNamespaceManager
	brManager        *networking.BridgeManager
	tapManager       *networking.TAPDeviceManager
	dhcpManager      *networking.DHCPManager
	sgManager        *networking.SecurityGroupManager
	routerManager    *networking.RouterManager
	mdRedirect       *networking.MetadataRedirect
	vxlanCoordinator *VXLANCoordinator
	cache            *cache.Cache
}

// NewService creates a new Neutron service
func NewService(pool *pgxpool.Pool, mode string, cacheInstance *cache.Cache) *Service {
	sgManager, err := networking.NewSecurityGroupManager(mode, "")
	if err != nil {
		log.Error().Err(err).Str("mode", mode).Msg("failed to initialize security group manager")
	}
	return &Service{
		pool:          pool,
		mode:          mode,
		nsManager:     networking.NewNetworkNamespaceManager(mode),
		brManager:     networking.NewBridgeManager(mode),
		tapManager:    networking.NewTAPDeviceManager(mode),
		dhcpManager:   networking.NewDHCPManager(mode),
		sgManager:     sgManager,
		routerManager: networking.NewRouterManager(mode),
		mdRedirect:    networking.NewMetadataRedirect(mode),
		cache:         cacheInstance,
	}
}

// RegisterRoutes registers Neutron routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Version discovery
	r.GET("/", svc.ListVersions)
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
		v2.GET("/security-group-rules/:id", svc.GetSecurityGroupRule)
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
		v2.GET("/quotas/:id/default", svc.GetQuotaDefault)
		// python-neutronclient appends a .json suffix to the quota sub-resources,
		// so the same handlers are registered under both spellings (FR-002).
		v2.GET("/quotas/:id/details.json", svc.GetQuotaDetails)
		v2.GET("/quotas/:id/default.json", svc.GetQuotaDefault)

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

// ListVersions serves the neutron version list at the root ("/") for version
// discovery — standard neutron behavior (Keystone/Nova/Glance already serve theirs;
// neutron previously 404'd at "/"). SPEC-009.
func (svc *Service) ListVersions(c *gin.Context) {
	c.JSON(200, gin.H{
		"versions": []gin.H{
			{
				"id":     "v2.0",
				"status": "CURRENT",
				"links": []gin.H{
					{"rel": "self", "href": fmt.Sprintf("%s/v2.0", common.BaseURL(c, 9696))},
				},
			},
		},
	})
}

// ListExtensions lists available API extensions
func (svc *Service) ListExtensions(c *gin.Context) {
	// Return list of supported extensions
	// Horizon uses this to determine feature availability.
	//
	// Every entry carries the full upstream shape {alias, name, description, updated, links}.
	// `links` is part of the entry in the Network v2 api-ref and is empty for all of these —
	// a client that reads it unconditionally gets [] rather than a missing key (SPEC-020 T043).
	c.JSON(200, gin.H{
		"extensions": []gin.H{
			{
				"alias":       "security-group",
				"name":        "security-group",
				"description": "The security groups extension",
				"updated":     "2012-10-05T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				"alias":       "router",
				"name":        "Neutron L3 Router",
				"description": "Router abstraction for basic L3 forwarding",
				"updated":     "2012-07-20T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				"alias":       "port-security",
				"name":        "Port Security",
				"description": "Port security extension",
				"updated":     "2012-07-23T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				"alias":       "binding",
				"name":        "Port Binding",
				"description": "Expose port bindings of a virtual port to external application",
				"updated":     "2014-02-03T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				"alias":       "provider",
				"name":        "Provider Network",
				"description": "Provider network extension",
				"updated":     "2012-09-07T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				"alias":       "quotas",
				"name":        "Quota management support",
				"description": "Expose resource quotas",
				"updated":     "2012-07-29T10:00:00-00:00",
				"links":       []gin.H{},
			},
			{
				// Advertised because /quotas/{project}/details is served. Horizon 2025.2
				// calls that endpoint without checking for this alias, so the alias is for
				// API fidelity, not what makes the Overview widget work (SPEC-020 FR-013).
				"alias":       "quota_details",
				"name":        "Quota details management support",
				"description": "Expose functions for quotas usage statistics per project",
				"updated":     "2017-02-10T10:00:00-00:00",
				"links":       []gin.H{},
			},
		},
	})
}

// neutronQuotaDefaults holds the default network limits.
//
// Lightstack models no per-project network quota table, so these code defaults are the
// limits every network-quota view reports. Keeping them in ONE place is what stops
// GetQuota, GetQuotaDetails and GetQuotaDefault from drifting apart (SPEC-020
// FR-003) — they were previously written out twice with the values duplicated.
var neutronQuotaDefaults = map[string]int{
	"network":             100,
	"subnet":              100,
	"port":                500,
	"router":              10,
	"floatingip":          50,
	"security_group":      100,
	"security_group_rule": 500,
}

// neutronQuotaLimits returns a fresh copy of the default network limits.
func neutronQuotaLimits() gin.H {
	quota := gin.H{}
	for resource, limit := range neutronQuotaDefaults {
		quota[resource] = limit
	}
	return quota
}

// GetQuota returns network quotas for a project (limits only; use
// GetQuotaDetails for limits + usage).
func (svc *Service) GetQuota(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}
	quota := neutronQuotaLimits()
	// Upstream Neutron reports both spellings of the owning project.
	quota["project_id"] = projectID
	quota["tenant_id"] = projectID
	c.JSON(200, gin.H{"quota": quota})
}

// GetQuotaDefault returns the default network quotas for a project (code defaults
// only, no usage), served at /v2.0/quotas/{project_id}/default.
func (svc *Service) GetQuotaDefault(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}
	quota := neutronQuotaLimits()
	quota["project_id"] = projectID
	quota["tenant_id"] = projectID
	c.JSON(200, gin.H{"quota": quota})
}

// GetQuotaDetails returns network quotas with usage details
func (svc *Service) GetQuotaDetails(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// Query current usage. A count that fails answers 500, never zero usage.
	var networksUsed, subnetsUsed, portsUsed, routersUsed, floatingIPsUsed, securityGroupsUsed, securityGroupRulesUsed int

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
		{"security_group_rules", "SELECT COUNT(*) FROM security_group_rules WHERE security_group_id IN (SELECT id FROM security_groups WHERE project_id = $1)", &securityGroupRulesUsed},
	}
	for _, q := range usageQueries {
		if err := svc.pool.QueryRow(c.Request.Context(), q.sql, projectID).Scan(q.dst); err != nil {
			log.Error().Err(err).Str("resource", q.label).Str("project_id", projectID).Msg("failed to query quota usage")
			common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
			return
		}
	}

	// Return quota details with usage. Limits come from the same shared defaults as
	// GetQuota and GetQuotaDefault so the three views cannot disagree (FR-003).
	// `reserved` is always 0 — Lightstack completes operations synchronously and models no
	// reservations (Constitution Art. II).
	usage := map[string]int{
		"network":             networksUsed,
		"subnet":              subnetsUsed,
		"port":                portsUsed,
		"router":              routersUsed,
		"floatingip":          floatingIPsUsed,
		"security_group":      securityGroupsUsed,
		"security_group_rule": securityGroupRulesUsed,
	}
	quota := gin.H{}
	for resource, limit := range neutronQuotaDefaults {
		quota[resource] = gin.H{
			"limit":    limit,
			"used":     usage[resource],
			"reserved": 0,
		}
	}
	c.JSON(200, gin.H{"quota": quota})
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

	_, err := svc.pool.Exec(c.Request.Context(), `
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
			"id":              networkID,
			"name":            req.Network.Name,
			"tenant_id":       projectID,
			"project_id":      projectID,
			"admin_state_up":  adminStateUp,
			"status":          "ACTIVE",
			"shared":          shared,
			"router:external": false,
			"mtu":             mtu,
			"subnets":         []string{},
			"created_at":      now.Format(time.RFC3339),
			"updated_at":      now.Format(time.RFC3339),
		},
	})
}

// sharedFilter selects how the `shared` neutron list filter constrains results.
type sharedFilter int

const (
	sharedAny     sharedFilter = iota // caller's own networks OR any shared (no filter)
	sharedOnly                        // shared=true: only shared networks
	nonSharedOnly                     // shared=false: exclude shared networks
)

// neutronBool parses a neutron-style boolean query parameter, distinguishing an
// absent parameter from an explicit false (F4).
func neutronBool(s string) (val bool, present bool) {
	if s == "" {
		return false, false
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, false
	}
	return b, true
}

// networkListDecision maps the `shared` and `router:external` query filters to a
// shared-filter mode and an empty flag. router:external=true yields empty because
// Lightstack models no external/provider networks; honoring these filters is what makes
// Horizon's project/shared/external sub-queries disjoint so each network renders
// exactly once (F4, FR-009/010/012).
func networkListDecision(sharedParam, routerExternalParam string) (sf sharedFilter, empty bool) {
	if ext, ok := neutronBool(routerExternalParam); ok && ext {
		return sharedAny, true
	}
	switch b, ok := neutronBool(sharedParam); {
	case ok && b:
		return sharedOnly, false
	case ok && !b:
		return nonSharedOnly, false
	default:
		return sharedAny, false
	}
}

// buildNetworkListFilter builds the WHERE conditions and positional args for the
// network list from the caller's project and the neutron list filters Horizon sends
// (F4). It honors `shared` (only/exclude shared), `router:external` (empty result —
// Lightstack models no external networks), and `tenant_id`/`project_id` (narrowing within the
// caller's authorized scope), plus the id/name filters the Terraform provider uses.
// Placeholders are numbered from $1; `empty` means the query must return nothing. The
// caller continues numbering marker/pagination args from $(len(args)+1). Pure — no DB.
func buildNetworkListFilter(caller, sharedParam, routerExternalParam, tenantParam, idParam, nameParam string) (conds []string, args []any, empty bool) {
	sf, empty := networkListDecision(sharedParam, routerExternalParam)
	if empty {
		return nil, nil, true
	}
	argIdx := 1
	switch sf {
	case sharedOnly:
		conds = append(conds, "shared = true")
	case nonSharedOnly:
		conds = append(conds, fmt.Sprintf("(project_id = $%d AND shared = false)", argIdx))
		args = append(args, caller)
		argIdx++
	default: // sharedAny — the caller's own networks OR any shared network
		conds = append(conds, fmt.Sprintf("(project_id = $%d OR shared = true)", argIdx))
		args = append(args, caller)
		argIdx++
	}
	if tenantParam != "" {
		conds = append(conds, fmt.Sprintf("project_id = $%d", argIdx))
		args = append(args, tenantParam)
		argIdx++
	}
	if idParam != "" {
		conds = append(conds, fmt.Sprintf("id::text = $%d", argIdx))
		args = append(args, idParam)
		argIdx++
	}
	if nameParam != "" {
		conds = append(conds, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, nameParam)
	}
	return conds, args, false
}

// ListNetworks lists all networks
func (svc *Service) ListNetworks(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters. No default cap: without `limit` every visible row is returned,
	// as upstream does (pagination_max_limit defaults to -1); a nil limit binds LIMIT NULL, which
	// Postgres reads as no limit.
	var limit any
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

	// Neutron list filters (F4): honoring shared / router:external / tenant_id makes
	// Horizon's project+shared+external sub-queries disjoint, so each network renders
	// exactly once instead of being merged in from multiple overlapping queries.
	tenant := c.Query("tenant_id")
	if tenant == "" {
		tenant = c.Query("project_id")
	}
	conds, queryArgs, empty := buildNetworkListFilter(projectID, c.Query("shared"), c.Query("router:external"), tenant, c.Query("id"), c.Query("name"))
	if empty {
		// router:external=true — Lightstack models no external networks.
		c.JSON(http.StatusOK, gin.H{"networks": []gin.H{}})
		return
	}
	argIdx := len(queryArgs) + 1

	// Marker-based pagination
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at, id FROM networks WHERE id = $1",
			marker,
		).Scan(&markerCreatedAt, &markerID)
		if err == nil {
			conds = append(conds, fmt.Sprintf("(created_at, id) < ($%d, $%d)", argIdx, argIdx+1))
			queryArgs = append(queryArgs, markerCreatedAt, markerID)
			argIdx += 2
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := svc.pool.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id
		FROM networks
		WHERE %s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d
	`, strings.Join(conds, " AND "), argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_networks").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list networks"))
		return
	}
	defer rows.Close()

	var networks []gin.H
	for rows.Next() {
		var id, name, status, owner string
		var adminStateUp, shared bool
		var mtu int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner); err != nil {
			log.Warn().Err(err).Str("operation", "ListNetworks").Msg("failed to scan row")
			continue
		}

		// tenant_id from the row, not the caller: this listing can legitimately contain a SHARED
		// network owned by another project, and reporting the caller would tell them a foreign
		// resource is theirs (moved here from cloud-storage#546 B11 on PR #79 review).
		networks = append(networks, gin.H{
			"id":              id,
			"name":            name,
			"tenant_id":       owner,
			"project_id":      owner,
			"admin_state_up":  adminStateUp,
			"status":          status,
			"shared":          shared,
			"router:external": false,
			"mtu":             mtu,
			"created_at":      createdAt.Format(time.RFC3339),
			"updated_at":      updatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if networks == nil {
		networks = []gin.H{}
	}

	// Populate subnet IDs for each network (rows above are fully consumed).
	for _, n := range networks {
		subnetIDs, err := svc.networkSubnetIDs(c.Request.Context(), n["id"].(string))
		if err != nil {
			log.Error().Err(err).Str("operation", "list_networks").Msg("failed to resolve subnet ids")
			common.SendError(c, common.NewInternalServerError("failed to list networks"))
			return
		}
		n["subnets"] = subnetIDs
	}

	c.JSON(http.StatusOK, gin.H{"networks": networks})
}

// networkCacheKey names a cached network entry, scoped to the caller.
//
// The caller is part of the key because GetNetwork reads the cache BEFORE applying its
// visibility predicate (owner OR shared). Keyed on the network alone, the first fetch by an
// owner populated an entry that any authenticated caller knowing the id could then read for
// the whole TTL, where the uncached answer is 404 (raised on PR #79; the same defect is
// SRC-1 on the #546 branch, whose fix this matches).
//
// Scoping the key rather than re-checking on hit keeps a hit free of database work, at the
// cost of one entry per (caller, network) pair — a shared network is cached once per project
// that asks for it, which is the price of the lookup preceding the check.
func networkCacheKey(projectID, networkID string) string {
	return "network:" + projectID + ":" + networkID
}

// GetNetwork returns a single network
func (svc *Service) GetNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Try cache first. The key is scoped to the caller (see networkCacheKey): this lookup
	// happens before the visibility predicate below, so an unscoped key would serve one
	// tenant's network to another.
	if svc.cache != nil {
		var cached gin.H
		if err := svc.cache.Get(ctx, networkCacheKey(projectID, networkID), &cached); err == nil {
			// subnets are queried fresh so they reflect later subnet changes
			if id, ok := cached["id"].(string); ok {
				subnetIDs, subErr := svc.networkSubnetIDs(ctx, id)
				if subErr != nil {
					log.Error().Err(subErr).Str("operation", "get_network_cached").Msg("failed to resolve subnet ids")
					common.SendError(c, common.NewInternalServerError("failed to get network"))
					return
				}
				cached["subnets"] = subnetIDs
			}
			c.JSON(http.StatusOK, gin.H{"network": cached})
			return
		}
	}

	// Cache miss - query database
	var id, name, status, owner string
	var adminStateUp, shared bool
	var mtu int
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(ctx, `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id
		FROM networks
		WHERE (id::text = $1 OR name = $1) AND (project_id = $2 OR shared = true)
		LIMIT 1
	`, networkID, projectID).Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get network"))
		return
	}

	// tenant_id from the row, not the caller — this handler can return a SHARED network owned by
	// another project.
	network := gin.H{
		"id":              id,
		"name":            name,
		"tenant_id":       owner,
		"project_id":      owner,
		"admin_state_up":  adminStateUp,
		"status":          status,
		"shared":          shared,
		"router:external": false,
		"mtu":             mtu,
		"created_at":      createdAt.Format(time.RFC3339),
		"updated_at":      updatedAt.Format(time.RFC3339),
	}

	// Store in cache (30min TTL per config) before adding the dynamic subnet
	// list, so cached entries never carry a stale subnets field.
	if svc.cache != nil {
		svc.cache.TrySet(ctx, networkCacheKey(projectID, id), network, 30*time.Minute)
	}

	subnetIDs, err := svc.networkSubnetIDs(ctx, id)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_network").Msg("failed to resolve subnet ids")
		common.SendError(c, common.NewInternalServerError("failed to get network"))
		return
	}
	network["subnets"] = subnetIDs

	c.JSON(http.StatusOK, gin.H{"network": network})
}

// DeleteNetwork deletes a network
func (svc *Service) DeleteNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Check for active ports or subnets before deleting
	var portCount int
	if err := svc.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM ports WHERE network_id = $1", networkID,
	).Scan(&portCount); err == nil && portCount > 0 {
		common.SendError(c, common.NewOperationConflictError("delete network", "network has active ports"))
		return
	}

	var subnetCount int
	if err := svc.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM subnets WHERE network_id = $1", networkID,
	).Scan(&subnetCount); err == nil && subnetCount > 0 {
		common.SendError(c, common.NewOperationConflictError("delete network", "network has subnets; delete subnets first"))
		return
	}

	bridgeName := "br-" + networkID[:8]

	// Delete the bridge from the DEFAULT namespace, where it was created (real
	// mode). Deleting it also drops its gateway IP — leaving it behind would
	// leak a duplicate 10.x gateway onto the host and split routing across stale
	// bridges (SPEC-009). Best-effort: the network row is about to be deleted.
	if err := svc.brManager.DeleteBridge(bridgeName, false, ""); err != nil {
		log.Warn().Err(err).Str("bridge", bridgeName).Msg("failed to delete network bridge")
	}

	// Delete from database
	_, err := svc.pool.Exec(ctx,
		"DELETE FROM networks WHERE id = $1 AND project_id = $2",
		networkID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}

	// Invalidate cache. Entries are keyed per caller (networkCacheKey), so evicting this
	// network means evicting every project's view of it, not just the deleter's.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(ctx, "network:*:"+networkID)
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

	_, err := svc.pool.Exec(c.Request.Context(), query, args...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update network"))
		return
	}

	// Evict before re-reading. GetNetwork below tries the cache first, so without this the
	// response is the pre-update body and every caller's view stays stale for the whole TTL.
	// The pattern spans callers for the same reason DeleteNetwork's does.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(c.Request.Context(), "network:*:"+networkID)
		svc.cache.TryDeletePattern(c.Request.Context(), "networks:*")
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

// callerMaySubnetNetwork reports whether the caller is entitled to attach a subnet to
// the given network, and writes the refusal itself when they are not — so the caller
// only has to `return`.
//
// The rule is upstream Neutron's `create_subnet: rule:admin_or_network_owner`: the
// network's owner, or an administrator. **A shared network grants nothing here**, and
// that is a deliberate difference from CreatePort a few files over, which does admit
// shared networks (`WHERE id = $1 AND (project_id = $2 OR shared = true)`). Sharing a
// network lets other tenants *attach to* it; it never lets them define its address
// ranges, gateway or DHCP behaviour. The two handlers therefore read alike and decide
// differently on purpose — do not "unify" them (SPEC-022 FR-010, A5).
//
// The two refusals are distinguished on purpose (FR-010a): a network the caller cannot
// see at all is "not found", because confirming its existence would leak it; a shared
// network they can legitimately see in their own network list is "forbidden", because
// answering "not found" for something they can plainly see would be a detectable lie.
//
// Selecting project_id and shared, rather than filtering on them the way CreatePort
// does, is what makes those two answers separable at all — a WHERE clause that folds
// the decision in can only report "no row" (research R5).
func (svc *Service) callerMaySubnetNetwork(c *gin.Context, networkID, projectID string) bool {
	// A malformed id is a bad request naming the field, not a not-found: it is a body
	// attribute, so #515's rule for malformed *path* ids does not reach it. Checking
	// here also keeps the value away from the uuid-typed column, where Postgres would
	// raise 22P02 and surface as the 500 this feature exists to remove (FR-011a).
	if uuid.Validate(networkID) != nil {
		common.SendError(c, common.NewBadRequestError("invalid network_id"))
		return false
	}

	// `owner` is a plain string although networks.project_id is nullable (migration
	// 001:76 declares no NOT NULL). Every writer sets it — CreateNetwork (network.go:498)
	// and the auto-allocated topology (auto_allocated_topology.go:82) are the only two
	// INSERTs, and both take the project from the token — so a NULL is reachable only by
	// editing the table by hand. If one ever appears, Scan fails and the error limb below
	// answers 500: this fails closed, which is the direction a security check should fail.
	// A *string here would buy nothing and make the ownership comparison harder to read.
	//
	// That the Scan *fails* rather than yielding "" is the load-bearing half, so: pgx's
	// scanPlanString returns `cannot scan NULL into *string` on a nil src
	// (pgtype/pgtype.go:410 in v5.10.0), and UUIDCodec.PlanScan offers no plan for a plain
	// *string, so that generic plan is what runs here. A NULL therefore never reaches the
	// ownership comparison below. Spelled out because the claim is easy to doubt, and
	// reading it backwards makes this look like a bypass that it is not.
	var owner string
	var shared bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id, shared FROM networks WHERE id = $1",
		networkID,
	).Scan(&owner, &shared)

	if errors.Is(err, pgx.ErrNoRows) {
		// FR-011: no network of that id. Previously this reached the foreign key and
		// surfaced as a 500.
		common.SendError(c, common.NewNotFoundError("network"))
		return false
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "create_subnet_network_lookup").
			Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create subnet"))
		return false
	}

	if owner == projectID || common.HasRole(c, common.RoleAdmin) {
		return true
	}

	// A refused cross-tenant write is the event an operator goes looking for later —
	// a misconfigured client, or a probe. Named parties, or it cannot answer that
	// question (FR-011b). A legitimate creation logs nothing.
	log.Warn().
		Str("caller_project", projectID).
		Str("network_id", networkID).
		Str("network_owner", owner).
		Bool("network_shared", shared).
		Msg("refused cross-tenant subnet write")

	if shared {
		common.SendError(c, common.NewForbiddenError("only the owner of a network may add subnets to it"))
		return false
	}
	common.SendError(c, common.NewNotFoundError("network"))
	return false
}

// CreateSubnet creates a new subnet
func (svc *Service) CreateSubnet(c *gin.Context) {
	var req CreateSubnetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")

	// Only the target network's owner — or an administrator — may add a subnet to it.
	// Without this the network_id came straight from the request body and was never
	// checked against the caller's project, so any tenant could define the address
	// range, gateway and DHCP behaviour on any other tenant's network, and the victim's
	// own views then advertised it as theirs (cloud-storage#545 B8, FR-009/FR-010).
	//
	// This runs before the INSERT and before applyDHCPForNetwork, so a refused request
	// leaves no subnet row and starts no address assignment on the target network.
	if !svc.callerMaySubnetNetwork(c, req.Subnet.NetworkID, projectID) {
		return
	}

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
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, dns_nameservers, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, subnetID, req.Subnet.Name, req.Subnet.NetworkID, projectID, req.Subnet.CIDR, gatewayIP, ipVersion, enableDHCP, req.Subnet.DNSNameservers, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_subnet").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create subnet"))
		return
	}

	// Start DHCP server (+ bridge gateway IP + metadata DNAT) if enabled. The
	// shared path also applies any existing port reservations and runs dnsmasq in
	// the bridge's default namespace (SPEC-009 WS-2/WS-4).
	if enableDHCP {
		svc.applyDHCPForNetwork(c.Request.Context(), req.Subnet.NetworkID, true)
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

	// Parse pagination parameters. No default cap: without `limit` every visible row is returned,
	// as upstream does (pagination_max_limit defaults to -1); a nil limit binds LIMIT NULL, which
	// Postgres reads as no limit.
	var limit any
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
		err := svc.pool.QueryRow(c.Request.Context(),
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

	rows, err := svc.pool.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp, s.dns_nameservers, s.created_at, s.updated_at, s.project_id
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		-- Scope by the owning NETWORK's visibility, not only the subnet's own
		-- project_id. A network response lists every subnet of that network
		-- (networkSubnetIDs applies no project predicate), so anything visible there
		-- must be listable here too — otherwise Horizon expands the network's
		-- subnets array against this listing, fails to resolve an id, and renders a
		-- null row that breaks the launch wizard (SPEC-020 FR-007).
		WHERE (s.project_id = $1 OR n.project_id = $1 OR n.shared = true)%s
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
		// Nullable: subnets.project_id carries no NOT NULL, and pgx refuses a NULL into a
		// plain string — which ends the whole result set, not just this row. A NULL owner
		// reads as tenant_id null.
		var owner *string
		var ipVersion int
		var enableDHCP bool
		var dnsNameservers []string
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &createdAt, &updatedAt, &owner); err != nil {
			log.Warn().Err(err).Str("operation", "ListSubnets").Msg("failed to scan row")
			continue
		}

		if dnsNameservers == nil {
			dnsNameservers = []string{}
		}

		// tenant_id from the row, not the caller: the WHERE above deliberately matches
		// subnets on the caller's networks and on shared networks, so this listing can
		// legitimately contain rows the caller does not own.
		subnets = append(subnets, gin.H{
			"id":               id,
			"name":             name,
			"network_id":       networkID,
			"tenant_id":        owner,
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
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
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
	var owner *string // nullable, see ListSubnets
	var ipVersion int
	var enableDHCP bool
	var dnsNameservers []string
	var createdAt, updatedAt time.Time

	// id::text comparison resolves by UUID or name without crashing on a
	// non-UUID identifier (the CLI looks up by name this way).
	//
	// The visibility predicate must match ListSubnets: the owning NETWORK's project
	// counts as well as the subnet's own, or an id returned by the listing 404s here
	// (SPEC-022 FR-013a). This is also upstream's rule — neutron's get_subnet policy is
	// `PROJECT_READER or rule:shared or ... or ADMIN_OR_NET_OWNER_READER`, where
	// NET_OWNER_READER is `project_id:%(network:project_id)s`.
	//
	// Consequently s.project_id is selected, not assumed: this handler can return a subnet
	// the caller does not own — one on their network, or on a shared network — so reporting
	// the caller as tenant_id would tell them a resource is theirs when it is not.
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp, s.dns_nameservers, s.created_at, s.updated_at, s.project_id
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.id::text = $1 OR s.name = $1)
		  AND (s.project_id = $2 OR n.project_id = $2 OR n.shared = true)
		LIMIT 1
	`, subnetID, projectID).Scan(&id, &name, &networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &createdAt, &updatedAt, &owner)

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
			"tenant_id":        owner,
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
	// Scope: the subnet's own project, or the owner of the network it sits on —
	// upstream's delete_subnet is `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`, so a
	// network's owner may remove a subnet another project attached to it. Deliberately
	// **no** `shared` disjunct, unlike GetSubnet: upstream puts `rule:shared` in
	// get_subnet and not in delete_subnet, so sharing a network lets others read its
	// subnets, never delete them (SPEC-022 FR-013a).
	var resolvedID, networkID string
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT s.id, s.network_id
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.id::text = $1 OR s.name = $1)
		  AND (s.project_id = $2 OR n.project_id = $2)
		LIMIT 1
	`, subnetID, projectID).Scan(&resolvedID, &networkID)

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

	// Delete by the id the entitlement check above already resolved. Re-filtering on
	// project_id here would silently no-op for a network owner removing another
	// project's subnet — 204 with the row still present — because the two predicates
	// would disagree. One decision, one place.
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM subnets WHERE id = $1",
		resolvedID,
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

	// Resolve subnet by UUID or name to its canonical ID. Scoped like DeleteSubnet —
	// the subnet's own project or the owner of the network it sits on, no `shared`
	// disjunct — because upstream's update_subnet and delete_subnet carry the same rule
	// (`PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`). Renaming is strictly weaker than
	// removing, so a scope that allows the delete and refuses the rename would be an
	// odd place to draw a line (SPEC-022 FR-013a).
	var resolvedID, currentName string
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT s.id, s.name
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.id::text = $1 OR s.name = $1)
		  AND (s.project_id = $2 OR n.project_id = $2)
		LIMIT 1
	`, subnetID, projectID).Scan(&resolvedID, &currentName)

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

	// By resolved id only — the entitlement query above has already decided. Carrying
	// project_id here too would make a network owner's rename a silent no-op: 200 with
	// the old name still stored, because the two predicates disagree (the same trap
	// DeleteSubnet had).
	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE subnets SET name = $1 WHERE id = $2",
		currentName, resolvedID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", resolvedID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return
	}

	// Return updated subnet. tenant_id comes from the row, not from the caller: now
	// that a network's owner may rename a subnet belonging to another project, the two
	// differ, and reporting the caller would tell them the resource is theirs.
	var networkID, cidr, gatewayIP string
	var owner *string // nullable, see ListSubnets
	var ipVersion int
	var enableDHCP bool
	var dnsNameservers []string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT network_id, cidr, gateway_ip, ip_version, enable_dhcp, dns_nameservers, project_id FROM subnets WHERE id = $1",
		resolvedID,
	).Scan(&networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &owner)

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
			"tenant_id":        owner,
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
//
// Every failure is returned rather than logged. An earlier version treated this list as
// advisory and logged a truncated result, on the reasoning that the callers had no error
// channel — but the list is not advisory: Horizon expands each id against the subnet
// listing and renders a null row for any it cannot resolve, which is the launch-wizard
// failure FR-007 exists to fix. A short list is therefore indistinguishable from a
// network whose subnets were deleted, and answering 500 is the honest outcome. All three
// callers assemble a response and can report it.
func (svc *Service) networkSubnetIDs(ctx context.Context, networkID string) ([]string, error) {
	ids := []string{}
	rows, err := svc.pool.Query(ctx,
		"SELECT id FROM subnets WHERE network_id = $1 ORDER BY created_at DESC", networkID)
	if err != nil {
		return nil, fmt.Errorf("list subnets for network %s: %w", networkID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan subnet id for network %s: %w", networkID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subnets for network %s: %w", networkID, err)
	}
	return ids, nil
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
