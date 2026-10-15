package neutron

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
		v2.GET("/networks/:id/dhcp-agents", svc.ListDHCPAgentsOnNetwork)
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
				// Absolute, from the shared rule: a client reading this during
				// discovery has nothing to resolve a bare path against
				// (SPEC-025 FR-007a/FR-007b).
				{"rel": "self", "href": fmt.Sprintf("%s/v2.0", common.BaseURL(c, 9696))},
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
				// Horizon's admin Network list assigns its Project column inside a block guarded by
				// this alias — an unrelated capability, but the same loop — so without it the
				// ownership column rendered "-" for every network while the API returned the right
				// owner (SPEC-024 host pass, finding 2). The guard also fetches each network's
				// agents, so the alias is only honest alongside GET /networks/{id}/dhcp-agents;
				// Lightstack hosts none and reports an empty list.
				"alias":       "dhcp_agent_scheduler",
				"name":        "DHCP Agent Scheduler",
				"description": "Schedule networks among dhcp agents",
				"updated":     "2013-02-07T10:00:00-00:00",
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

// CreateNetworkRequest represents a network creation request
type CreateNetworkRequest struct {
	Network struct {
		Name         string `json:"name" binding:"required"`
		AdminStateUp *bool  `json:"admin_state_up"`
		Shared       *bool  `json:"shared"`
		// Both spellings of the owning project. Neither was declared before, so the value a client
		// sent was discarded by ShouldBindJSON and never reached the handler: the request was
		// answered 201 with the network in the CALLER's project (cloud-storage#546 B11). Nothing was
		// being ignored — the field did not exist.
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
		// Setting it true is administrator-only, matching upstream's `create_network:router:external`
		// -> ADMIN (see CreateNetwork for why false is not). Was hard-coded false in every response and
		// unsettable, so no network could serve as a router gateway or floating-IP pool and Horizon's
		// Set Gateway dropdown was always empty (F11).
		External *bool `json:"router:external"`
	} `json:"network"`
}

// resolveNetworkOwner decides which project a new network belongs to, and writes the refusal itself
// when the request is not allowed — so the caller only has to `return`.
//
// Mirrors upstream, read from neutron_lib/api/attributes.py (its API reference documents none of
// this):
//
//   - populate_project_info: one spelling populates the other; both present and different is a 400.
//   - _validate_privileges: a project differing from the authenticated one is a 400 unless
//     context.can_set_project_id, which is `is_admin or is_service_role or …`.
//
// Lightstack has no service-account concept, so administrator is the whole privileged set. All three ways of
// getting this attribute wrong return the SAME status (400), so a client has one rule rather than
// three, and none of them discloses whether another project exists (FR-012-FR-016).
//
// This deliberately differs from the compute service, which refuses cross-project server creation
// outright: upstream Nova has no such attribute to honour while upstream Neutron does. Fidelity is
// per-service (spec A10/A11) — do not "correct" the asymmetry.
func (svc *Service) resolveNetworkOwner(c *gin.Context, req CreateNetworkRequest, caller string) (string, bool) {
	requested := req.Network.ProjectID
	if requested == nil {
		requested = req.Network.TenantID
	} else if req.Network.TenantID != nil && *req.Network.TenantID != *requested {
		common.SendError(c, common.NewBadRequestError("'project_id' and 'tenant_id' do not match"))
		return "", false
	}

	if requested == nil || *requested == "" || *requested == caller {
		return caller, true
	}

	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewBadRequestError(
			"Specifying 'project_id' other than the authenticated project in request is not allowed for the current user."))
		return "", false
	}

	// Resolve before the insert so the foreign key never surfaces as a 500, and so a malformed id is
	// a bad request naming the field rather than a Postgres type error.
	if uuid.Validate(*requested) != nil {
		common.SendError(c, common.NewBadRequestError("invalid project_id"))
		return "", false
	}
	var exists bool
	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)", *requested).Scan(&exists); err != nil {
		log.Error().Err(err).Str("operation", "resolve_network_owner").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create network"))
		return "", false
	}
	if !exists {
		common.SendError(c, common.NewBadRequestError("invalid project_id"))
		return "", false
	}
	return *requested, true
}

// CreateNetwork creates a new network
func (svc *Service) CreateNetwork(c *gin.Context) {
	var req CreateNetworkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	caller := c.GetString("project_id")
	projectID, ok := svc.resolveNetworkOwner(c, req, caller)
	if !ok {
		return
	}
	if projectID != caller {
		log.Info().Str("caller_project", caller).Str("target_project", projectID).
			Msg("administrator creating a network into another project")
	}
	networkID := uuid.New().String()
	bridgeName := networking.BridgeNameForNetwork(networkID)

	adminStateUp := true
	if req.Network.AdminStateUp != nil {
		adminStateUp = *req.Network.AdminStateUp
	}

	shared := false
	if req.Network.Shared != nil {
		shared = *req.Network.Shared
	}

	// Designating a network as the deployment's boundary to the outside is an operator decision, not
	// a tenant one — administrator-only even for the owner (FR-033). Only `true` is gated, unlike
	// UpdateNetwork, and on purpose: upstream skips an attribute's policy on create when the value
	// sent equals its default (neutron/policy.py, _is_attribute_explicitly_set), and router:external
	// defaults to false — which is what `openstack network create --internal` sends.
	external := false
	if req.Network.External != nil {
		if *req.Network.External && !common.HasRole(c, common.RoleAdmin) {
			common.SendError(c, common.NewForbiddenError("only an administrator may set router:external"))
			return
		}
		external = *req.Network.External
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
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, external, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, networkID, req.Network.Name, projectID, adminStateUp, "ACTIVE", shared, external, networkType, mtu, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_network").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create network"))
		return
	}

	// Create bridge in the default namespace (not the project namespace) so libvirt can reach it.
	//
	// This is also why a port's TAP goes there — see CreatePort. Keeping the two apart is what broke
	// port creation in real mode (SPEC-024 F10); do not move one without the other.
	//
	// After the row, never before: the reconciler removes any bridge whose network it cannot read, so
	// a bridge created before its row is committed could be taken away in between (see
	// pruneDeletedNetworks). A bridge that cannot be created takes the row back with it.
	if err := svc.brManager.CreateBridge(bridgeName); err != nil {
		log.Error().Err(err).Str("operation", "create_bridge").Str("bridge", bridgeName).Msg("failed to create bridge")
		if _, derr := svc.pool.Exec(c.Request.Context(), "DELETE FROM networks WHERE id = $1", networkID); derr != nil {
			log.Warn().Err(derr).Str("network_id", networkID).Msg("rollback: failed to delete network after bridge failure")
		}
		common.SendError(c, common.NewInternalServerError("failed to create bridge"))
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
			"router:external": external,
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
// shared-filter mode and an empty flag. Honouring these filters is what makes Horizon's
// project/shared/external sub-queries disjoint so each network renders exactly once
// (F4, FR-009/010/012).
//
// `router:external` used to force an empty result because Lightstack modelled no external networks; it is a
// real predicate since SPEC-024 F11, applied in buildNetworkListFilter.
func networkListDecision(sharedParam, routerExternalParam string) (sf sharedFilter, empty bool) {
	// router:external is now a real predicate applied by buildNetworkListFilter, so it no longer
	// forces an empty result. Kept in the signature because the two filters are read together and
	// Horizon sends them together (F11, FR-032).
	_ = routerExternalParam
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
// (F4). It honors `shared` (only/exclude shared), `router:external` (a real predicate since
// SPEC-024 F11), and `tenant_id`/`project_id` (narrowing within the
// caller's authorized scope), plus the id/name filters the Terraform provider uses.
// Placeholders are numbered from $1; `empty` means the query must return nothing. The
// caller continues numbering marker/pagination args from $(len(args)+1). Pure — no DB.
// buildNetworkListFilter builds the WHERE conditions and positional args for the
// networks listing. isAdmin matters only together with tenantParam: an
// administrator filtering by tenant gets that tenant's networks outright
// (upstream honours the filter for admin tokens), so the caller's read scope
// must not be ANDed in — it would leave only the shared/external subset and
// answer [] for another project's private networks (SPEC-028). A non-admin
// keeps the scope AND tenant intersection.
func buildNetworkListFilter(caller, sharedParam, routerExternalParam, tenantParam, idParam, nameParam string, isAdmin bool) (f *common.SQLFilter, empty bool) {
	sf, empty := networkListDecision(sharedParam, routerExternalParam)
	if empty {
		return nil, true
	}
	f = &common.SQLFilter{}
	switch sf {
	case sharedOnly:
		f.Cond("shared = true")
	case nonSharedOnly:
		f.Cond("(project_id = " + f.Arg(caller) + " AND shared = false)")
	default: // the caller's own networks, OR any shared network, OR any external network
		// `external` is a THIRD INDEPENDENT disjunct — deliberately not `AND shared`. An external
		// network is the deployment's boundary to the outside and every tenant needs to see it to
		// select it as a router gateway; requiring `shared` as well would leave the tenant-facing
		// Set Gateway dropdown empty and fix F11 for administrators only (FR-033a). Read-only:
		// visibility confers no authority (FR-033b, and #545's rule for shared networks).
		//
		// Skipped when an admin explicitly filters by tenant: the tenant filter
		// is then the authoritative owner scope.
		if !(isAdmin && tenantParam != "") {
			f.Cond(networkReadScope(f.Arg(caller)))
		}
	}

	if ext, ok := neutronBool(routerExternalParam); ok {
		f.Eq("external", ext)
	}
	if tenantParam != "" {
		f.Eq("project_id", tenantParam)
	}
	if idParam != "" {
		f.Eq("id::text", idParam)
	}
	if nameParam != "" {
		f.Eq("name", nameParam)
	}
	return f, false
}

// optionalString decodes a JSON field that may be absent, explicitly null, or a string, keeping the
// three apart — which `*string` alone cannot do, because both absent and null decode to nil.
//
// Returns (value, present, err): present is false only when the key was omitted entirely.
func optionalString(raw json.RawMessage) (value *string, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, err
	}
	return &v, true, nil
}

// validateSubnetGateway checks a gateway against its subnet as upstream does (_validate_subnet and
// ipam's check_gateway_invalid_in_subnet): an address of the subnet's IP version, and neither its
// network address nor, for IPv4, its broadcast address. Two differences remain. Upstream accepts a
// gateway outside the subnet; Lightstack refuses it, because the network's bridge carries the gateway
// address and can only carry one on the subnet. And upstream refuses a gateway inside an allocation
// pool (GatewayConflictWithAllocationPools); Lightstack keeps no per-subnet pools to check against.
func validateSubnetGateway(gateway, cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("the subnet's cidr %q is not valid", cidr)
	}
	ip := net.ParseIP(gateway)
	if ip == nil {
		return fmt.Errorf("%q is not an IP address", gateway)
	}
	ipv4 := ipNet.IP.To4() != nil
	switch {
	case (ip.To4() != nil) != ipv4:
		return fmt.Errorf("%s is not of the subnet's IP version", gateway)
	case !ipNet.Contains(ip):
		return fmt.Errorf("%s is outside the subnet %s", gateway, cidr)
	case ip.Equal(ipNet.IP):
		return fmt.Errorf("%s is the subnet's network address", gateway)
	case ipv4 && ip.Equal(broadcast(ipNet)):
		return fmt.Errorf("%s is the subnet's broadcast address", gateway)
	}
	return nil
}

// subnetFilterParams carries the raw query values the subnet listing honours, so the builder below
// stays pure and unit-testable without a gin context (SPEC-024 B6).
type subnetFilterParams struct {
	NetworkID  string
	ID         string
	Name       string
	CIDR       string
	IPVersion  string
	EnableDHCP string
	ProjectID  string
}

// buildSubnetListFilter turns the contracted subnet filters into WHERE conditions and positional
// args, numbered from startIdx. Pure — no DB.
//
// ListSubnets previously parsed only limit/offset/marker and applied NO client filter at all, so
// `openstack subnet list --network X` returned every visible subnet and the client had to filter
// locally (cloud-storage#546 B6, FR-019/FR-020). This mirrors buildNetworkListFilter.
//
// Only these seven are advertised. An unrecognized parameter must leave the result UNCHANGED rather
// than becoming an accidental predicate: quietly returning fewer rows than asked for is the defect
// class this feature exists to remove (FR-020a). The same applies to a value that does not parse —
// an unparseable ip_version is ignored, not treated as a match-nothing.
func addSubnetListFilter(f *common.SQLFilter, p subnetFilterParams) {
	// id and network_id are compared as text so a non-UUID identifier cannot error the whole query.
	if p.NetworkID != "" {
		f.Eq("s.network_id::text", p.NetworkID)
	}
	if p.ID != "" {
		f.Eq("s.id::text", p.ID)
	}
	if p.Name != "" {
		f.Eq("s.name", p.Name)
	}
	if p.CIDR != "" {
		f.Eq("s.cidr", p.CIDR)
	}
	if p.IPVersion != "" {
		if v, err := strconv.Atoi(p.IPVersion); err == nil {
			f.Eq("s.ip_version", v)
		}
	}
	if b, ok := neutronBool(p.EnableDHCP); ok {
		f.Eq("s.enable_dhcp", b)
	}
	if p.ProjectID != "" {
		f.Eq("s.project_id::text", p.ProjectID)
	}
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
	f, empty := buildNetworkListFilter(projectID, c.Query("shared"), c.Query("router:external"), tenant, c.Query("id"), c.Query("name"), common.HasRole(c, common.RoleAdmin))
	if empty {
		// No filter combination produces this today; kept so a future filter can short-circuit
		// without re-plumbing the caller.
		c.JSON(http.StatusOK, gin.H{"networks": []gin.H{}})
		return
	}

	// Marker-based pagination (see resolveListMarker).
	m, ok := svc.resolveListMarker(c, limit, "network",
		"SELECT created_at, id FROM networks WHERE id::text = $1 AND "+networkReadScope("$2"))
	if !ok {
		return
	}
	if m != nil {
		f.Cond(fmt.Sprintf("(created_at, id) < (%s, %s)", f.Arg(m.createdAt), f.Arg(m.id)))
	}

	// Rendered in statement order — predicates, then paging — so the arguments are appended in the
	// order the placeholders read them. Written inline as one Sprintf it would depend on Go's
	// left-to-right argument evaluation to come out right, which is guaranteed but not obvious.
	where := f.Conds()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external
		FROM networks
		WHERE `+where+`
		ORDER BY created_at DESC, id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_networks").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list networks"))
		return
	}
	defer rows.Close()

	var networks []gin.H
	for rows.Next() {
		var id, name, status, owner string
		var adminStateUp, shared, external bool
		var mtu int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external); err != nil {
			log.Warn().Err(err).Str("network_id", id).Msg("skipping unreadable network row")
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
			"router:external": external,
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
// The caller is part of the key because GetNetwork reads the cache BEFORE applying its visibility
// predicate (owner OR shared OR external). Keyed on the network alone, the first fetch by an owner
// populated an entry that any authenticated caller knowing the id could then read for the whole TTL,
// where the uncached answer is 404 — and since #546 made the body report the row's real owner rather
// than the caller's, that leak names the owner too (branch review 2026-08-13, SRC-1).
//
// Scoping the key rather than re-checking on hit keeps a hit free of database work, at the cost of
// one entry per (caller, network) pair — a shared or external network is cached once per project
// that asks for it, which is the price of the lookup preceding the check.
func networkCacheKey(projectID, networkID string) string {
	return "network:" + projectID + ":" + networkID
}

// GetNetwork returns a single network
func (svc *Service) GetNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Try cache first. The key is scoped to the caller (see networkCacheKey): this lookup happens
	// BEFORE the visibility predicate below, so a key naming only the network would serve one
	// tenant's entry to another.
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
	var adminStateUp, shared, external bool
	var mtu int
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(ctx, `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external
		FROM networks
		WHERE (id::text = $1 OR name = $1) AND `+networkReadScope("$2")+`
		LIMIT 1
	`, networkID, projectID).Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external)

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
	// another project (B11, FR-017).
	network := gin.H{
		"id":              id,
		"name":            name,
		"tenant_id":       owner,
		"project_id":      owner,
		"admin_state_up":  adminStateUp,
		"status":          status,
		"shared":          shared,
		"router:external": external,
		"mtu":             mtu,
		"created_at":      createdAt.Format(time.RFC3339),
		"updated_at":      updatedAt.Format(time.RFC3339),
	}

	// Store in cache (30min TTL per config) before adding the dynamic subnet
	// list, so cached entries never carry a stale subnets field. Written under the SAME
	// caller-scoped key the read above uses, so an entry can only ever be served back to a caller
	// entitled to it.
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

	// Resolve and authorise first, and from here on act on the stored id, never the path parameter.
	// The host teardown used to run before anything confirmed ownership, keyed on the raw parameter:
	// a caller who could merely SEE a network (shared, or external per FR-033a) removed its bridge
	// and was then told 404, and an id that is not a UUID made the in-use counts below fail — an
	// error they ignored — so `<first 8 hex>-x` named a live bridge in use. Visibility confers no
	// authority (FR-033b). 404 rather than 403, matching the non-disclosure rule used elsewhere, and
	// before the in-use checks, whose 409 would otherwise confirm that the network exists.
	var id string
	err := svc.pool.QueryRow(ctx,
		"SELECT id::text FROM networks WHERE id::text = $1 AND project_id = $2",
		networkID, projectID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}

	inUse, err := svc.networkInUse(ctx, id)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", id).Msg("failed to check whether the network is in use")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}
	if inUse != "" {
		common.SendError(c, common.NewResourceInUseError("network", "delete network", inUse))
		return
	}

	// Delete from database
	tag, err := svc.pool.Exec(ctx,
		"DELETE FROM networks WHERE id = $1 AND project_id = $2",
		id, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}
	// Ownership was settled above, so matching no row here means a concurrent delete got there first.
	// Reporting 204 for a delete that deleted nothing is the dishonest-success defect this feature
	// exists to remove.
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}

	// Host teardown only once the row is gone.
	svc.teardownNetworkHost(id)

	// Invalidate cache. Entries are keyed per caller (networkCacheKey), so evicting this
	// network means evicting every project's view of it, not just the deleter's.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(ctx, "network:*:"+id)
		svc.cache.TryDeletePattern(ctx, "networks:*")
	}

	c.Status(http.StatusNoContent)
}

// networkInUse reports why a network may not be deleted, or "" when it may.
//
// Upstream refuses while any port remains other than its own automatic ones (DHCP and the like, which
// Lightstack does not create), and a floating IP or a router gateway is such a port there. Here each is
// a row of its own, so each is checked directly — the floating IP would otherwise cascade away with
// the network, and the router keep a gateway on a network that no longer exists. Across every
// project, as upstream's ports are. Subnets are not a reason: as upstream, they are deleted with the
// network. A check that cannot be read is an error, not a "no", since the cascade would take the ports
// and floating IPs with it.
func (svc *Service) networkInUse(ctx context.Context, networkID string) (string, error) {
	var hasPorts, hasFloatingIPs, isGateway bool
	if err := svc.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ports WHERE network_id = $1),
		       EXISTS (SELECT 1 FROM floating_ips WHERE floating_network_id = $1),
		       EXISTS (SELECT 1 FROM routers WHERE external_gateway_info->>'network_id' = $2)`,
		networkID, networkID,
	).Scan(&hasPorts, &hasFloatingIPs, &isGateway); err != nil {
		return "", fmt.Errorf("check whether network %s is in use: %w", networkID, err)
	}
	switch {
	case hasPorts:
		return "network has active ports", nil
	case hasFloatingIPs:
		return "network has floating IPs allocated from it", nil
	case isGateway:
		return "network is the external gateway of a router", nil
	}
	return "", nil
}

// teardownNetworkHost removes what the host runs for a network whose row is gone: its DHCP server, its
// metadata DNAT and its bridge. Both network delete paths call it, so neither can again leave behind
// what the other removes. Best-effort: whatever a failed step leaves behind, the reconciler prunes,
// since it removes bridges, DHCP servers and metadata rules whose network no longer exists.
func (svc *Service) teardownNetworkHost(networkID string) {
	bridgeName := networking.BridgeNameForNetwork(networkID)

	// The subnets went with the row, so nothing else stops the network's DHCP server: DeleteSubnet
	// does when the last DHCP subnet goes, and no subnet delete ran. Before the bridge, which
	// dnsmasq is bound to.
	if err := svc.dhcpManager.StopDHCP(networkID); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("failed to stop DHCP")
	}

	// Remove the metadata DNAT before the bridge, so the rule never outlives the interface it is
	// keyed on. Left behind it is inert until a later network draws a UUID with the same first 8
	// hex chars and reuses the bridge name — then the stale rule, being earlier in the ordered
	// PREROUTING chain, wins and sends that network's guests to a gateway that no longer exists
	// (SPEC-024 host pass, finding 1). The subnets are already gone by now, which is why this is
	// keyed on the bridge rather than on a gateway address.
	if err := svc.mdRedirect.RemoveForBridge(bridgeName); err != nil {
		log.Warn().Err(err).Str("bridge", bridgeName).Msg("failed to remove metadata DNAT")
	}

	// Delete the bridge from the DEFAULT namespace, where it was created (real
	// mode). Deleting it also drops its gateway IP — leaving it behind would
	// leak a duplicate 10.x gateway onto the host and split routing across stale
	// bridges (SPEC-009).
	if err := svc.brManager.DeleteBridge(bridgeName); err != nil {
		log.Warn().Err(err).Str("bridge", bridgeName).Msg("failed to delete network bridge")
	}
}

// UpdateNetwork updates a network
func (svc *Service) UpdateNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Network struct {
			Name         *string `json:"name"`
			AdminStateUp *bool   `json:"admin_state_up"`
			External     *bool   `json:"router:external"`
		} `json:"network"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Any value, unlike CreateNetwork: on update upstream enforces an attribute's policy whenever the
	// attribute is present.
	isAdmin := common.HasRole(c, common.RoleAdmin)
	if req.Network.External != nil && !isAdmin {
		common.SendError(c, common.NewForbiddenError("only an administrator may set router:external"))
		return
	}

	u := &common.SQLUpdate{}

	if req.Network.External != nil {
		u.Set("external", *req.Network.External)
	}

	if req.Network.Name != nil {
		u.Set("name", *req.Network.Name)
	}

	if req.Network.AdminStateUp != nil {
		u.Set("admin_state_up", *req.Network.AdminStateUp)
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	u.Set("updated_at", time.Now())

	// The owner predicate is what scopes this to the caller — but an administrator setting
	// router:external on a TENANT's network would then match zero rows and still be answered 200,
	// the same dishonest-success defect as B11 inside the fix for a different one (research R6.4).
	// Marking a network external is a deployment-level decision, so an administrator is exempt.
	query := "UPDATE networks SET " + u.Assignments() + " WHERE id::text = " + u.Arg(networkID)
	if !isAdmin {
		query += " AND project_id = " + u.Arg(projectID)
	}

	tag, err := svc.pool.Exec(c.Request.Context(), query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update network"))
		return
	}
	// As in DeleteNetwork: an update that matched no row must not be answered 200. Reachable now that
	// a caller can see a network they do not own (FR-033a/b).
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}

	// The response must be built with the same authority that performed the write.
	//
	// Delegating to GetNetwork looks natural and is wrong: its scope is
	// (project_id = caller OR shared OR external) with no administrator exemption, so an
	// administrator's successful write to another project's network came back 404 — the row updated
	// and the caller was told it did not exist. Only `router:external = true` escaped it, because that
	// write happens to make the row match the `external` disjunct; renaming, and unmarking external,
	// both applied and reported not-found. That is the mirror image of the zero-rows defect above:
	// there, a write that did nothing reported success; here, a write that did something reported
	// failure. Both are the dishonest answers this feature exists to remove.
	//
	// Reading the row back by id is not a general "administrators may read any network" grant — the
	// question of a cross-project network read stays open. It is narrower: the answer to your own
	// successful write.
	//
	// Evicting first is still required even though the read-back skips the cache: the entry is
	// keyed per caller (see networkCacheKey), so every OTHER caller's view — and the list cache —
	// would serve the pre-update body for the whole TTL. The pattern spans callers for the same
	// reason DeleteNetwork's does.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(c.Request.Context(), "network:*:"+networkID)
		svc.cache.TryDeletePattern(c.Request.Context(), "networks:*")
	}
	svc.writeNetworkResponse(c, networkID)
}

// writeNetworkResponse emits a network by id with no visibility predicate, for the caller that just
// wrote it (see UpdateNetwork). Ownership was already decided by the write.
func (svc *Service) writeNetworkResponse(c *gin.Context, networkID string) {
	var id, name, status, owner string
	var adminStateUp, shared, external bool
	var mtu int
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external
		FROM networks
		WHERE id::text = $1
		LIMIT 1
	`, networkID).Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "read_back_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read network"))
		return
	}

	subnets, err := svc.networkSubnetIDs(c.Request.Context(), id)
	if err != nil {
		log.Error().Err(err).Str("operation", "read_back_network").Str("network_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read network"))
		return
	}

	network := gin.H{
		"id":              id,
		"name":            name,
		"tenant_id":       owner,
		"project_id":      owner,
		"admin_state_up":  adminStateUp,
		"status":          status,
		"shared":          shared,
		"router:external": external,
		"mtu":             mtu,
		"created_at":      createdAt.Format(time.RFC3339),
		"updated_at":      updatedAt.Format(time.RFC3339),
		"subnets":         subnets,
	}
	c.JSON(http.StatusOK, gin.H{"network": network})
}

// CreateSubnetRequest represents a subnet creation request
type CreateSubnetRequest struct {
	Subnet struct {
		Name      string `json:"name"`
		NetworkID string `json:"network_id" binding:"required"`
		CIDR      string `json:"cidr" binding:"required"`
		// Raw, so an explicit null is distinguishable from an omitted attribute: omitted means
		// "choose one for me", null means "no gateway". As a plain string the two were identical and
		// both got an invented address, so `--gateway none` was silently overwritten and a gatewayless
		// subnet was unreachable through the API (SPEC-024 B6, FR-023).
		GatewayIP      json.RawMessage `json:"gateway_ip"`
		IPVersion      int             `json:"ip_version"`
		EnableDHCP     *bool           `json:"enable_dhcp"`
		DNSNameservers []string        `json:"dns_nameservers"`
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

	// Three states, per FR-023: omitted -> choose the first host address (unchanged behaviour);
	// explicit null -> no gateway; explicit value -> as given.
	gatewayIP, present, err := optionalString(req.Subnet.GatewayIP)
	if err != nil {
		common.SendError(c, common.NewBadRequestError("invalid gateway_ip"))
		return
	}
	// Stored as text and handed verbatim to the bridge, the metadata DNAT and dnsmasq, so a value
	// nothing checked could take DHCP away from the whole network.
	if gatewayIP != nil {
		if err := validateSubnetGateway(*gatewayIP, req.Subnet.CIDR); err != nil {
			common.SendError(c, common.NewBadRequestError("invalid gateway_ip: "+err.Error()))
			return
		}
	}
	if !present {
		auto := incrementIP(ipNet.IP, 1).String()
		gatewayIP = &auto
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
		if _, _, err := svc.applyDHCPForNetwork(c.Request.Context(), req.Subnet.NetworkID, true); err != nil {
			log.Warn().Err(err).Str("network_id", req.Subnet.NetworkID).Msg("failed to start DHCP for the new subnet")
		}
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

// subnetReadScope is the predicate under which a caller may READ a subnet: their own, one on a network
// they own, or one on a network every project can see — shared, or external (FR-033a). It follows the
// network read scope because a network response lists every subnet of that network (networkSubnetIDs
// applies no project predicate), so anything visible there must be resolvable here too — otherwise
// Horizon expands the network's subnets array, fails to resolve an id, and renders a null row that
// breaks the launch wizard (SPEC-020 FR-007). Upstream reads subnets through the network's RBAC
// entries with access_as_external as well as access_as_shared (neutron/db/external_net_db.py).
// Writes are narrower: DeleteSubnet and UpdateSubnet admit only the two owners.
//
// caller is the placeholder carrying the caller's project id; the query must alias the subnet as s
// and its network as n. ListSubnets and GetSubnet share it so the two cannot drift apart again.
func subnetReadScope(caller string) string {
	return "(s.project_id = " + caller + " OR n.project_id = " + caller + " OR n.shared = true OR n.external = true)"
}

// networkReadScope is the predicate under which a caller may READ a network: their own, a shared one,
// or an external one — external being a third independent way to be visible (FR-033a). GetNetwork,
// the listing and the listing's marker share it so the three cannot disagree. caller is the
// placeholder carrying the caller's project id.
func networkReadScope(caller string) string {
	return "(project_id = " + caller + " OR shared = true OR external = true)"
}

// listMarker is a resolved pagination marker: the position a page continues after.
type listMarker struct {
	createdAt time.Time
	id        string
}

// resolveListMarker resolves a listing's marker as upstream Neutron does (neutron-lib
// db/utils.get_marker_obj): only when a limit is given — without one the marker is not used — and
// through the resource's own read, so an id the caller may not read resolves exactly like an id that
// names nothing. query selects (created_at, id) of the row with id $1 the caller may read, the
// caller's project id being $2. A marker that does not resolve is the resource's 404, written here so
// the handler only has to return; a nil marker with ok true means none is in effect.
//
// The networks, subnets and ports listings used to read the marker with no visibility and ignore one
// that did not resolve, so another project's private row narrowed the page where an absent id did
// not — telling the caller it exists.
func (svc *Service) resolveListMarker(c *gin.Context, limit any, resource, query string) (*listMarker, bool) {
	marker := c.Query("marker")
	if marker == "" || limit == nil {
		return nil, true
	}
	var m listMarker
	err := svc.pool.QueryRow(c.Request.Context(), query, marker, c.GetString("project_id")).Scan(&m.createdAt, &m.id)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError(resource))
		return nil, false
	}
	if err != nil {
		log.Error().Err(err).Str("marker", marker).Str("resource", resource).Msg("failed to resolve pagination marker")
		common.SendError(c, common.NewInternalServerError("failed to list "+resource+"s"))
		return nil, false
	}
	return &m, true
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

	f := &common.SQLFilter{}

	// Scope by the owning NETWORK's visibility, not only the subnet's own project_id (see
	// subnetReadScope). One argument read by two placeholders, so it is appended once and the
	// placeholder reused.
	caller := f.Arg(projectID)
	f.Cond(subnetReadScope(caller))

	// Client filters (B6). Both spellings of the project attribute map onto project_id.
	tenant := c.Query("tenant_id")
	if tenant == "" {
		tenant = c.Query("project_id")
	}
	addSubnetListFilter(f, subnetFilterParams{
		NetworkID:  c.Query("network_id"),
		ID:         c.Query("id"),
		Name:       c.Query("name"),
		CIDR:       c.Query("cidr"),
		IPVersion:  c.Query("ip_version"),
		EnableDHCP: c.Query("enable_dhcp"),
		ProjectID:  tenant,
	})

	// Marker-based pagination on a TOTAL order. `created_at` alone is not unique, so ties made the
	// page boundary unstable and a walk could repeat or skip rows — the same defect #549 found in the
	// compute listings. ListNetworks already pages on (created_at, id); FR-025 requires the same here.
	// The marker itself resolves as resolveListMarker describes.
	m, ok := svc.resolveListMarker(c, limit, "subnet",
		"SELECT s.created_at, s.id FROM subnets s JOIN networks n ON s.network_id = n.id WHERE s.id::text = $1 AND "+subnetReadScope("$2"))
	if !ok {
		return
	}
	if m != nil {
		f.Cond(fmt.Sprintf("(s.created_at, s.id) < (%s, %s)", f.Arg(m.createdAt), f.Arg(m.id)))
	}

	// Rendered in statement order so the arguments land in placeholder order.
	where := f.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp, s.dns_nameservers, s.created_at, s.updated_at, s.project_id
		FROM subnets s
		JOIN networks n ON s.network_id = n.id`+where+`
		ORDER BY s.created_at DESC, s.id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_subnets").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list subnets"))
		return
	}
	defer rows.Close()

	var subnets []gin.H
	for rows.Next() {
		var id, name, networkID, cidr string
		// gateway_ip is nullable (migration 001) and a subnet may legitimately have none
		// (cloud-storage#546 B6, FR-021/FR-022).
		var gatewayIP *string
		// Nullable: subnets.project_id carries no NOT NULL, and pgx refuses a NULL into a
		// plain string — which ends the whole result set, not just this row. A NULL owner
		// reads as tenant_id null.
		var owner *string
		var ipVersion int
		var enableDHCP bool
		var dnsNameservers []string
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &networkID, &cidr, &gatewayIP, &ipVersion, &enableDHCP, &dnsNameservers, &createdAt, &updatedAt, &owner); err != nil {
			// Log and keep serving: one unreadable row must not blank every caller's subnet list,
			// including the dashboard's, and must not vanish silently either (FR-024).
			log.Warn().Err(err).Str("subnet_id", id).Msg("skipping unreadable subnet row")
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

	var id, name, networkID, cidr string
	var gatewayIP *string // nullable: a subnet may have no gateway (B6)
	var owner *string     // nullable, see ListSubnets
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
		  AND `+subnetReadScope("$2")+`
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

	svc.reconcileNetworkDHCPAfterSubnetDelete(c.Request.Context(), networkID)

	c.Status(http.StatusNoContent)
}

// reconcileNetworkDHCPAfterSubnetDelete brings a network's DHCP state back in line with its subnets
// after one of them has been removed. Called AFTER the DELETE, so the count below is the post-state.
//
// dnsmasq and the metadata DNAT are both per-NETWORK, while the delete is per-SUBNET, so neither may
// simply be torn down because one subnet went. DeleteSubnet used to call StopDHCP(networkID)
// unconditionally and before the row was even removed, which took DHCP away from every other subnet
// on that network — its guests stopped renewing until the reconciler's next pass, up to 30s later.
//
// One count answers both questions:
//
//   - subnets remain: dnsmasq is restarted rather than left alone, because its config names ONE
//     subnet (applyDHCPForNetwork takes LIMIT 1) and that may have been the one just deleted. The
//     restart regenerates it from a survivor. The metadata redirect stays; it is still needed.
//   - none remain: there is nothing left to serve, so dnsmasq stops and the redirect is reclaimed —
//     iptables outlives the pod, the release and the database (SPEC-024 host pass, finding 1).
//
// A failed count leaves both in place. The reconciler converges either way, whereas acting on an
// unknown state risks removing something live.
func (svc *Service) reconcileNetworkDHCPAfterSubnetDelete(ctx context.Context, networkID string) {
	var remaining int
	if err := svc.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM subnets WHERE network_id = $1 AND enable_dhcp = true", networkID,
	).Scan(&remaining); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).
			Msg("failed to count remaining DHCP subnets; leaving DHCP and metadata DNAT as they are")
		return
	}

	if remaining > 0 {
		if _, _, err := svc.applyDHCPForNetwork(ctx, networkID, true); err != nil {
			log.Warn().Err(err).Str("network_id", networkID).Msg("failed to re-apply DHCP for the remaining subnets")
		}
		return
	}

	if err := svc.dhcpManager.StopDHCP(networkID); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("failed to stop DHCP server")
	}
	if len(networkID) >= 8 {
		if err := svc.mdRedirect.RemoveForBridge(networking.BridgeNameForNetwork(networkID)); err != nil {
			log.Warn().Err(err).Str("network_id", networkID).Msg("failed to remove metadata DNAT")
		}
	}
}

// routerPortHolding returns the id of the router port holding ip on the subnet, or "" when none does —
// upstream's get_alloc_routerports. A router interface is a port carrying the subnet's gateway in its
// fixed_ips, recorded in router_interfaces.
func (svc *Service) routerPortHolding(ctx context.Context, subnetID, ip string) (string, error) {
	var portID string
	err := svc.pool.QueryRow(ctx, `
		SELECT p.id::text
		FROM router_interfaces ri
		JOIN ports p ON p.id = ri.port_id
		WHERE ri.subnet_id = $1
		  AND p.fixed_ips @> jsonb_build_array(jsonb_build_object('ip_address', $2::text))
		LIMIT 1
	`, subnetID, ip).Scan(&portID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return portID, err
}

// reconcileNetworkDHCPAfterGatewayChange moves the host side of a DHCP-enabled subnet whose gateway
// changed. The bridge carries the gateway address, the metadata DNAT points at it, and dnsmasq
// advertises it as the router and as the metadata next hop. Nothing else would move them — a
// reconciler pass adds the new address and rule but never removes an address, and never restarts a
// dnsmasq that is already running — so the old address and the redirect are removed here and DHCP
// re-applied with a restart, which asserts the new address and rule and reloads dnsmasq.
//
// Removing the old address cuts no guest's way out: a gateway a router interface holds cannot change
// (GatewayIpInUse, see UpdateSubnet), and guest egress only runs through a router here — the egress
// SNAT is installed for router-attached subnets alone. A guest still using the old address as its
// metadata next hop moves on its next lease renewal.
func (svc *Service) reconcileNetworkDHCPAfterGatewayChange(ctx context.Context, networkID, cidr string, oldGateway, newGateway *string) {
	bridgeName := networking.BridgeNameForNetwork(networkID)
	if oldAddr := bridgeAddrFor(oldGateway, cidr); oldAddr != "" && oldAddr != bridgeAddrFor(newGateway, cidr) {
		if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
			ones, _ := ipNet.Mask.Size()
			if err := svc.brManager.RemoveBridgeIP(bridgeName, fmt.Sprintf("%s/%d", oldAddr, ones)); err != nil {
				log.Warn().Err(err).Str("bridge", bridgeName).Str("address", oldAddr).Msg("failed to remove the old gateway address")
			}
		}
	}
	if err := svc.mdRedirect.RemoveForBridge(bridgeName); err != nil {
		log.Warn().Err(err).Str("bridge", bridgeName).Msg("failed to remove metadata DNAT")
	}
	if _, _, err := svc.applyDHCPForNetwork(ctx, networkID, true); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("failed to re-apply DHCP after a gateway change")
	}
}

// UpdateSubnet updates a subnet
func (svc *Service) UpdateSubnet(c *gin.Context) {
	subnetID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Subnet struct {
			Name *string `json:"name"`
			// Raw for the same reason as CreateSubnet: an explicit null clears the gateway, an
			// omitted key leaves it alone (FR-023, contract S14).
			GatewayIP json.RawMessage `json:"gateway_ip"`
		} `json:"subnet"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	newGateway, gatewayGiven, err := optionalString(req.Subnet.GatewayIP)
	if err != nil {
		common.SendError(c, common.NewBadRequestError("invalid gateway_ip"))
		return
	}

	// Resolve subnet by UUID or name to its canonical ID. Scoped like DeleteSubnet —
	// the subnet's own project or the owner of the network it sits on, no `shared`
	// disjunct — because upstream's update_subnet and delete_subnet carry the same rule
	// (`PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`). Renaming is strictly weaker than
	// removing, so a scope that allows the delete and refuses the rename would be an
	// odd place to draw a line (SPEC-022 FR-013a).
	var resolvedID, currentName, subnetNetwork, subnetCIDR string
	var currentGateway *string
	var subnetDHCP bool
	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.enable_dhcp
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE (s.id::text = $1 OR s.name = $1)
		  AND (s.project_id = $2 OR n.project_id = $2)
		LIMIT 1
	`, subnetID, projectID).Scan(&resolvedID, &currentName, &subnetNetwork, &subnetCIDR, &currentGateway, &subnetDHCP)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return
	}

	// The gateway is checked as on create, and — as upstream's GatewayIpInUse — may not change while a
	// router interface holds it: the router's port would keep an address the subnet no longer
	// advertises.
	if gatewayGiven && newGateway != nil {
		if err := validateSubnetGateway(*newGateway, subnetCIDR); err != nil {
			common.SendError(c, common.NewBadRequestError("invalid gateway_ip: "+err.Error()))
			return
		}
	}
	gatewayChanged := gatewayGiven && derefOr(newGateway, "") != derefOr(currentGateway, "")
	if gatewayChanged && currentGateway != nil {
		holder, err := svc.routerPortHolding(c.Request.Context(), resolvedID, *currentGateway)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", resolvedID).Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to update subnet"))
			return
		}
		if holder != "" {
			common.SendError(c, common.NewResourceInUseError("gateway ip", "update subnet",
				fmt.Sprintf("Current gateway ip %s already in use by port %s. Unable to update.", *currentGateway, holder)))
			return
		}
	}

	// Apply updates
	if req.Subnet.Name != nil {
		currentName = *req.Subnet.Name
	}

	// By resolved id only — the entitlement query above has already decided. Carrying
	// project_id here too would make a network owner's rename a silent no-op: 200 with
	// the old name still stored, because the two predicates disagree (the same trap
	// DeleteSubnet had).
	// gateway_ip is only touched when the key was supplied; an explicit null clears it (S14).
	switch {
	case gatewayGiven:
		_, err = svc.pool.Exec(c.Request.Context(),
			"UPDATE subnets SET name = $1, gateway_ip = $2 WHERE id = $3",
			currentName, newGateway, resolvedID,
		)
	case req.Subnet.Name != nil:
		_, err = svc.pool.Exec(c.Request.Context(),
			"UPDATE subnets SET name = $1 WHERE id = $2",
			currentName, resolvedID,
		)
	default:
		// Nothing was supplied, so nothing is written. This used to issue
		// `UPDATE subnets SET name = <the name just read>` — a statement that changed no column and
		// did not even touch updated_at (second review, AGY-2).
		//
		// The request still SUCCEEDS: upstream Neutron accepts an empty update and answers 200 with
		// the unchanged resource, and Terraform and Horizon both send updates that can reduce to
		// nothing. Refusing with 400 — the second review's suggestion — would have been an API
		// compatibility regression, which this project's first principle forbids.
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", resolvedID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return
	}
	if gatewayChanged && subnetDHCP {
		svc.reconcileNetworkDHCPAfterGatewayChange(c.Request.Context(), subnetNetwork, subnetCIDR, currentGateway, newGateway)
	}

	// Return updated subnet. tenant_id comes from the row, not from the caller: now
	// that a network's owner may rename a subnet belonging to another project, the two
	// differ, and reporting the caller would tell them the resource is theirs.
	var networkID, cidr string
	var gatewayIP *string // nullable: a subnet may have no gateway (B6)
	var owner *string     // nullable, see ListSubnets
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
// network whose subnets were deleted, and answering 500 is the honest outcome. Every
// caller assembles a response and can report it.
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

func generateMAC() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	buf[0] = (buf[0] | 2) & 0xfe // Set local bit, clear multicast bit
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}
