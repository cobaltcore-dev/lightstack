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
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Service handles Neutron API endpoints
type Service struct {
	pool             database.DB
	mode             string
	brManager        bridgeDataplane
	vxlanCoordinator *VXLANCoordinator
	cache            *cache.Cache

	// nsManager deletes the per-project namespaces older deployments made, which teardown reclaims:
	// *networking.NetworkNamespaceManager, or a fake in tests.
	nsManager projectNamespaces
	// retired removes what earlier layouts left on the node and this one never writes (FR-044):
	// *networking.RetiredState, or a fake in tests.
	retired retiredState

	// uplink names the node's uplink: the default route's interface in real mode, "" in stub mode,
	// where nothing is programmed (R9). A function so that tests can make it fail.
	uplink func() (string, error)

	// nodes places this node in its deployment: its name, the network node, how to reach the others
	// (SPEC-033 R26, R27). A single node by default; SetNodes changes it.
	nodes NodeOptions
	// units are the unit locks of the units this node holds (R27).
	units unitLocks
	// caller asks the node that holds a unit to converge it: httpNodeCaller, or a fake in tests (R41).
	caller nodeCaller
	// routers converges the routers this node holds as the network node: *networking.RouterManager, or
	// a fake in tests (R28, R41).
	routers routerDataplane
	// dhcp converges the networks' DHCP this node holds as the network node: dhcpManager, or a fake in
	// tests (R29, R41).
	dhcp dhcpDataplane
	// listeners are the metadata listeners in the networks' DHCP namespaces (R29): the metadata
	// service's, set by SetMetadataListeners in real mode. Nil serves none.
	listeners MetadataListeners
	// firewall converges the firewall of the ports bound to this node: a FirewallManager, or a fake in
	// tests (R32, R41).
	firewall portFirewall
	// fdb writes a port's VXLAN forwarding row in the port's own transaction: the VXLAN coordinator
	// when VXLAN is on, nil otherwise, or a fake in tests (FR-045, R40).
	fdb fdbStore
}

// bridgeDataplane makes and removes the networks' bridges on this node: *networking.BridgeManager, or a
// fake in tests (R41).
type bridgeDataplane interface {
	CreateBridge(name string) error
	DeleteBridge(name string) error
	ListManagedBridges() ([]string, error)
}

// projectNamespaces deletes the per-project namespaces of older deployments (teardown, R48).
type projectNamespaces interface {
	DeleteNamespace(ctx context.Context, projectID string) error
}

// retiredState removes what earlier layouts left on the node, as far as it is recognisably Lightstack's
// (FR-044): *networking.RetiredState, or a fake in tests.
type retiredState interface {
	Remove(ctx context.Context, uplink string, records networking.RetiredRecords) error
}

// fdbStore writes a port's VXLAN forwarding row in the transaction that stores the port (FR-045).
type fdbStore interface {
	RecordFDB(ctx context.Context, tx database.Tx, networkID, portID, mac string) error
}

// nextZone is the lowest connection-tracking zone no network holds, from 4097, as upstream's bridge
// driver numbers them (SPEC-033 R32): a subquery for the statement that gives a network its zone. That
// statement runs in a transaction that took ctZoneLock first.
const nextZone = `(SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM networks WHERE ct_zone = 4097) THEN 4097
	ELSE (SELECT MIN(z.ct_zone) + 1 FROM networks z WHERE z.ct_zone >= 4097
	       AND NOT EXISTS (SELECT 1 FROM networks y WHERE y.ct_zone = z.ct_zone + 1)) END)`

// ctZoneLock serialises the statements that give a network nextZone: the lowest free zone is read and
// taken in one critical section, so two requests never compute the same one, and a statement that fails
// is answered, not run again (constitution Article III: the write lock is taken, never a retry). The
// unique index on networks.ct_zone stays the guarantee.
const ctZoneLock = "lightstack:ct-zone"

// NewService creates a new Neutron service
func NewService(pool database.DB, mode string, cacheInstance *cache.Cache) *Service {
	uplink := networking.DefaultUplinkInterface
	if mode == "stub" {
		uplink = func() (string, error) { return "", nil }
	}
	// A single node, its own network node, until SetNodes says otherwise. A host without a name is
	// still its own network node: the empty name is both.
	name, err := common.NodeName()
	if err != nil {
		log.Warn().Err(err).Msg("this node has no name; it is a single node")
	}
	svc := &Service{
		pool:      pool,
		mode:      mode,
		nsManager: networking.NewNetworkNamespaceManager(mode),
		brManager: networking.NewBridgeManager(mode),
		retired:   networking.NewRetiredState(mode),
		cache:     cacheInstance,
		uplink:    uplink,
		nodes:     NodeOptions{Name: name, NetworkNode: name},
		routers:   networking.NewRouterManager(mode),
		dhcp:      networking.NewDHCPManager(mode),
		firewall:  networking.NewFirewallManager(mode),
	}
	svc.caller = newHTTPNodeCaller(svc)
	return svc
}

// RegisterNodeRoutes registers the node-to-node call (internal/neutron/node_call.go), which authenticates
// with a node token, never a user's, and checks it itself. The server registers it before the user-token
// middleware: gin fixes a route's handlers when it is registered, so the middleware added later never runs
// for it, and no path is exempted from that middleware (SPEC-033 T210).
func (svc *Service) RegisterNodeRoutes(r *gin.RouterGroup) {
	r.POST(nodeCallPath, svc.ConvergeHandler)
}

// RegisterRoutes registers Neutron's API routes.
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
	if coordinator != nil {
		svc.fdb = coordinator
	}
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
				// SPEC-033 FR-050, FR-051: groups may be stateless. openstacksdk and the CLI check for
				// this alias before they send "stateful".
				"alias":       "stateful-security-group",
				"name":        "Stateful security group",
				"description": "Indicates if the security group is stateful or not",
				"updated":     "2019-11-26T09:00:00-00:00",
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
		// The standard resource annotation (migration 075). A pointer so an
		// omitted key and an explicit empty string differ, as upstream does.
		Description *string `json:"description"`
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

// CreateNetwork creates a new network
func (svc *Service) CreateNetwork(c *gin.Context) {
	var req CreateNetworkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	caller := c.GetString("project_id")
	projectID, ok := svc.ownerOf(c, req.Network.ProjectID, req.Network.TenantID)
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

	// Making a network shared exposes it to every tenant in the deployment —
	// an operator decision, like router:external below. A member-created
	// shared network was visible to, and attachable by, every project. Same
	// shape as the external gate: only `true` is gated, matching upstream's
	// explicit-set rule.
	shared := false
	if req.Network.Shared != nil {
		if *req.Network.Shared && !common.HasRole(c, common.RoleAdmin) {
			common.SendError(c, common.NewForbiddenError("only an administrator may create a shared network"))
			return
		}
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

	networkType, mtu := svc.networkTypeAndMTU()

	// With its connection-tracking zone, which its ports' and its bridge's traffic is tracked in on
	// every node (SPEC-033 R32), taken under the zone lock.
	ctx := c.Request.Context()
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, ctZoneLock); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, external, network_type, mtu, description, created_at, updated_at, ct_zone)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, `+nextZone+`)
	`, networkID, req.Network.Name, projectID, adminStateUp, "ACTIVE", shared, external, networkType, mtu, req.Network.Description, now, now)
		return err
	})

	if err != nil {
		log.Error().Err(err).Str("operation", "create_network").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create network"))
		return
	}

	// Create bridge in the default namespace (not the project namespace) so libvirt can reach it and put the
	// guests' TAPs on it, which it names for their ports (SPEC-024 F10, SPEC-033 FR-045).
	//
	// After the row, never before: the reconciler removes any bridge whose network it cannot read, so
	// a bridge created before its row is committed could be taken away in between (see
	// pruneDeletedNetworks). A bridge that cannot be created takes the row back with it.
	if err := svc.brManager.CreateBridge(bridgeName); err != nil {
		log.Error().Err(err).Str("operation", "create_bridge").Str("bridge", bridgeName).Msg("failed to create bridge")
		svc.undoNetworkCreate(c.Request.Context(), networkID, now, "")
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
			"description":     derefOr(req.Network.Description, ""),
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
	switch b, ok := common.ParseQueryBool(sharedParam); {
	case ok && b:
		return sharedOnly, false
	case ok && !b:
		return nonSharedOnly, false
	default:
		return sharedAny, false
	}
}

// networkListQuery is a network listing's filters as the query gives them: shared, router:external, id and
// name, and the merged project_id/tenant_id.
type networkListQuery struct {
	shared, routerExternal, id, name string
	projects                         []string
}

// buildNetworkListFilter builds the WHERE conditions and positional args for the
// network list from the caller's project and the neutron list filters Horizon sends
// (F4). It honors `shared` (only/exclude shared), `router:external` (a real predicate since
// SPEC-024 F11), and `tenant_id`/`project_id` (narrowing within the
// caller's authorized scope), plus the id/name filters the Terraform provider uses.
// Placeholders are numbered from $1; `empty` means the query must return nothing. The
// caller continues numbering marker/pagination args from $(len(args)+1). Pure — no DB.
//
// An administrator's listing spans every project, as upstream's (SPEC-033 FR-057): admin drops the
// caller's scope, and projects — the merged, repeatable project_id/tenant_id — narrow either kind of
// listing to the named projects.
func buildNetworkListFilter(c callerScope, q networkListQuery) (f *common.SQLFilter, empty bool) {
	caller, admin := c.project, c.admin
	sf, empty := networkListDecision(q.shared, q.routerExternal)
	if empty {
		return nil, true
	}
	f = &common.SQLFilter{}
	switch {
	case sf == sharedOnly:
		f.Cond("shared = true")
	case sf == nonSharedOnly && admin:
		f.Cond("shared = false")
	case sf == nonSharedOnly:
		f.Cond("(project_id = " + f.Arg(caller) + " AND shared = false)")
	case admin:
		// Every project's networks (FR-057).
	default: // the caller's own networks, OR any shared network, OR any external network
		// `external` is a THIRD INDEPENDENT disjunct — deliberately not `AND shared`. An external
		// network is the deployment's boundary to the outside and every tenant needs to see it to
		// select it as a router gateway; requiring `shared` as well would leave the tenant-facing
		// Set Gateway dropdown empty and fix F11 for administrators only (FR-033a). Read-only:
		// visibility confers no authority (FR-033b, and #545's rule for shared networks).
		f.Cond(networkReadScope(f.Arg(caller)))
	}

	if ext, ok := common.ParseQueryBool(q.routerExternal); ok {
		f.Eq("external", ext)
	}
	if len(q.projects) > 0 {
		f.Cond("project_id::text = ANY(" + f.Arg(q.projects) + ")")
	}
	if q.id != "" {
		f.Eq("id::text", q.id)
	}
	if q.name != "" {
		f.Eq("name", q.name)
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
// network address nor, for IPv4, its broadcast address. One difference remains: upstream accepts a
// gateway outside the subnet; Lightstack refuses it, because the router's interface on the network takes
// the gateway address, and its leg holds addresses on the subnet only. A gateway inside an allocation
// pool is refused as upstream refuses it, with the pools (validatePools, gatewayOutOfPools; FR-030).
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
	if b, ok := common.ParseQueryBool(p.EnableDHCP); ok {
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
	f, empty := buildNetworkListFilter(callerScope{project: projectID, admin: isAdmin(c)}, networkListQuery{shared: c.Query("shared"),
		routerExternal: c.Query("router:external"), id: c.Query("id"), name: c.Query("name"), projects: projectFilters(c)})
	if empty {
		// No filter combination produces this today; kept so a future filter can short-circuit
		// without re-plumbing the caller.
		c.JSON(http.StatusOK, gin.H{"networks": []gin.H{}})
		return
	}

	// Marker-based pagination (see resolveListMarker).
	m, ok := svc.resolveListMarker(c, limit, "network",
		"SELECT created_at, id FROM networks WHERE id::text = $1 AND "+scopeOrAdmin(networkReadScope("$2"), "$3"))
	if !ok {
		return
	}
	if m != nil {
		f.Cond(fmt.Sprintf("(created_at, id) < (%s, %s)", f.Arg(m.createdAt), f.Arg(m.id)))
	}

	// Rendered in statement order — predicates, then paging — so the arguments are appended in the
	// order the placeholders read them. Written inline as one Sprintf it would depend on Go's
	// left-to-right argument evaluation to come out right, which is guaranteed but not obvious.
	// An administrator's unfiltered listing has no condition at all, so the clause is Where's.
	where := f.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external, COALESCE(description, '')
		FROM networks`+where+`
		ORDER BY created_at DESC, id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_networks").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list networks"))
		return
	}
	defer rows.Close()

	var networks []gin.H
	for rows.Next() {
		var id, name, status, owner, description string
		var adminStateUp, shared, external bool
		var mtu int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external, &description); err != nil {
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
			"description":     description,
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
//
// Visibility also depends on the caller's role — an administrator may read any project's network by
// id (SPEC-033 FR-057) — so GetNetwork passes a scope naming the role as well (its cacheScope): an
// entry an administrator fetched is never served to the plain members of the administrator's project.
func networkCacheKey(projectID, networkID string) string {
	return "network:" + projectID + ":" + networkID
}

// GetNetwork returns a single network
func (svc *Service) GetNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()
	// The cache key's scope: the caller's project, and whether the caller is an administrator, who
	// may read any project's network by id (SPEC-033 FR-057). Without the role in it, an
	// administrator's read would be served to the members of the administrator's project.
	cacheScope := projectID
	if isAdmin(c) {
		cacheScope = "admin@" + projectID
	}

	// Try cache first. The key is scoped to the caller (see networkCacheKey): this lookup happens
	// BEFORE the visibility predicate below, so a key naming only the network would serve one
	// tenant's entry to another.
	if svc.cache != nil {
		var cached gin.H
		if err := svc.cache.Get(ctx, networkCacheKey(cacheScope, networkID), &cached); err == nil {
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
	var id, name, status, owner, description string
	var adminStateUp, shared, external bool
	var mtu int
	var createdAt, updatedAt time.Time

	// Administrators may address any network by id, but a name stays in the
	// caller's read scope so a duplicate name cannot select another tenant's
	// resource.
	query := `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external, COALESCE(description, '')
		FROM networks
		WHERE ((id::text = $1 AND ` + scopeOrAdmin(networkReadScope("$2"), "$3") + `) OR (name = $1 AND ` + networkReadScope("$2") + `))
		ORDER BY (id::text = $1) DESC
		LIMIT 1`
	err := svc.pool.QueryRow(ctx, query, networkID, projectID, isAdmin(c)).
		Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external, &description)

	if errors.Is(err, database.ErrNoRows) {
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
		"description":     description,
		"created_at":      createdAt.Format(time.RFC3339),
		"updated_at":      updatedAt.Format(time.RFC3339),
	}

	// Store in cache (30min TTL per config) before adding the dynamic subnet
	// list, so cached entries never carry a stale subnets field. Written under the SAME
	// caller-scoped key the read above uses, so an entry can only ever be served back to a caller
	// entitled to it.
	if svc.cache != nil {
		svc.cache.TrySet(ctx, networkCacheKey(cacheScope, id), network, 30*time.Minute)
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
		"SELECT id::text FROM networks WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		networkID, projectID, isAdmin(c),
	).Scan(&id)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete network"))
		return
	}

	if err := svc.removeNetwork(ctx, id); err != nil {
		sendChangeError(c, err, "delete", "network")
		return
	}
	c.Status(http.StatusNoContent)
}

// removeNetwork deletes a network in FR-040's order, for both requests that delete one: DeleteNetwork
// and DeleteAutoAllocatedTopology (SPEC-033 T185). It is refused while something uses the
// network; its DHCP leaves the network node; then the record goes, with its subnets and DHCP port, in a
// transaction that locks it and checks again, so a port created meanwhile keeps it. A failure or a
// refusal from there puts the DHCP back from the records. Last, this node's bridge and the cached
// views of the network go. A refusal is the *common.OpenStackError to answer with.
func (svc *Service) removeNetwork(ctx context.Context, id string) error {
	refuseInUse := func(q subnetQuerier) error {
		why, err := networkInUse(ctx, q, id)
		if err != nil {
			return err
		}
		if why != "" {
			return common.NewNeutronError(http.StatusConflict, "NetworkInUse",
				fmt.Sprintf("Unable to complete operation on network %s. %s.", id, why))
		}
		return nil
	}
	if err := refuseInUse(svc.pool); err != nil {
		return err
	}
	if err := svc.removeDHCPUnit(ctx, id); err != nil {
		svc.reapplyDHCP(ctx, id)
		return fmt.Errorf("remove the DHCP of network %s; the network stays: %w", id, err)
	}
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		// The caller found the network, so no row here means a concurrent delete got there first: 404,
		// not a 204 for a delete that deleted nothing.
		if err := lockNetwork(ctx, tx, id); errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("network")
		} else if err != nil {
			return err
		}
		if err := refuseInUse(tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM networks WHERE id = $1", id)
		return err
	})
	if err != nil {
		svc.reapplyDHCP(ctx, id)
		return err
	}
	svc.teardownNetworkHost(id)
	// Cached views are keyed per caller (networkCacheKey): evicting this network evicts every
	// project's view of it, not just the deleter's. Every key networkCacheKey writes ends in ":<id>",
	// so this one pattern is the complete eviction.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(ctx, "network:*:"+id)
	}
	return nil
}

// undoNetworkCreate takes back a network create that could not be applied (SPEC-033 FR-008, R40), in one
// statement: the row goes only while its updated_at holds what the create wrote and nothing another
// request built references it — a subnet other than ownSubnet, a port other than its DHCP port, a
// floating IP. Its own subnet and DHCP port go with it. A network another request changed or built on
// stays.
func (svc *Service) undoNetworkCreate(ctx context.Context, networkID string, created time.Time, ownSubnet string) {
	if _, err := svc.pool.Exec(context.WithoutCancel(ctx), `
		DELETE FROM networks WHERE id::text = $1 AND updated_at = $2
		   AND NOT EXISTS (SELECT 1 FROM subnets WHERE network_id::text = $1 AND id::text <> $3)
		   AND NOT EXISTS (SELECT 1 FROM ports WHERE network_id::text = $1 AND COALESCE(device_owner, '') <> '`+ownerDHCP+`')
		   AND NOT EXISTS (SELECT 1 FROM floating_ips WHERE floating_network_id::text = $1)`,
		networkID, created, ownSubnet); err != nil {
		log.Error().Err(err).Str("network_id", networkID).Msg("undo a network create; the network stays")
	}
}

// networkInUse is upstream's NetworkInUse reason for a network that may not be deleted, or "" when it
// may.
//
// Upstream refuses while any port remains other than its own automatic ones, the DHCP port among them,
// which goes with the network as upstream's does, and names them (_ensure_network_not_in_use); a
// floating IP or a router gateway is such a port there. Here a floating IP has no port, nor has a
// gateway set before gateways had one, so each is checked and named as what it is — the floating IP
// would otherwise cascade away with the network, and the router keep a gateway on a network that no
// longer exists. Across every project, as upstream's ports are. Subnets are not a reason: as upstream,
// they are deleted with the network. A check that cannot be read is an error, not a "no", since the
// cascade would take the ports and floating IPs with it.
func networkInUse(ctx context.Context, q subnetQuerier, networkID string) (string, error) {
	for _, check := range []struct{ query, reason string }{
		{`SELECT id::text FROM ports WHERE network_id = $1 AND COALESCE(device_owner, '') <> '` + ownerDHCP + `' ORDER BY created_at, id`,
			"There are one or more ports still in use on the network, id for these ports is: "},
		{`SELECT id::text FROM floating_ips WHERE floating_network_id = $1 ORDER BY created_at, id`,
			"There are one or more floating IPs allocated from the network, id for these floating IPs is: "},
		{`SELECT id::text FROM routers WHERE external_gateway_info->>'network_id' = $1 ORDER BY created_at, id`,
			"There are one or more routers using the network as their external gateway, id for these routers is: "},
	} {
		ids, err := queryStrings(ctx, q, check.query, networkID)
		if err != nil {
			return "", fmt.Errorf("check whether network %s is in use: %w", networkID, err)
		}
		if len(ids) > 0 {
			return check.reason + strings.Join(ids, ","), nil
		}
	}
	return "", nil
}

// teardownNetworkHost removes this node's bridge of a network whose row is gone. Its DHCP left the
// network node before the row went (DeleteNetwork); the other nodes' bridges go with their reconciliation
// (FR-040). Both network delete paths call it. Best-effort: a bridge a failed step leaves behind, the
// reconciler prunes, since it removes the bridges of networks that no longer exist.
func (svc *Service) teardownNetworkHost(networkID string) {
	bridgeName := networking.BridgeNameForNetwork(networkID)
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
			// The standard resource annotation (migration 075): 'network set
			// --description' PUTs it, and the handler had no column to
			// persist it to (SPEC-031).
			Description *string `json:"description"`
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

	if req.Network.Description != nil {
		u.Set("description", *req.Network.Description)
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

	// As in DeleteNetwork: an update that matched no row must not be answered 200. Reachable now that
	// a caller can see a network they do not own (FR-033a/b).
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		if req.Network.External != nil && !*req.Network.External {
			if err := requireNoGatewayPorts(c.Request.Context(), tx, networkID); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(c.Request.Context(), query, u.Args()...)
		if err == nil && tag.RowsAffected() == 0 {
			return common.NewNotFoundError("network")
		}
		return err
	})
	if err != nil {
		sendChangeError(c, err, "update", "network")
		return
	}

	// The response is built with the same authority that performed the write: the row is read back
	// by id with no visibility predicate. Delegating to GetNetwork once answered an administrator's
	// successful write to another project's network with 404, while GetNetwork's scope had no
	// administrator exemption. It has one now (SPEC-033 FR-057), but the answer to a write should not
	// hang on a read's scope.
	//
	// Evicting first is still required even though the read-back skips the cache: the entry is
	// keyed per caller (see networkCacheKey), so every OTHER caller's view — and the list cache —
	// would serve the pre-update body for the whole TTL. The pattern spans callers for the same
	// reason DeleteNetwork's does.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(c.Request.Context(), "network:*:"+networkID)
	}
	svc.writeNetworkResponse(c, networkID)
}

// writeNetworkResponse emits a network by id with no visibility predicate, for the caller that just
// wrote it (see UpdateNetwork). Ownership was already decided by the write.
func (svc *Service) writeNetworkResponse(c *gin.Context, networkID string) {
	var id, name, status, owner, description string
	var adminStateUp, shared, external bool
	var mtu int
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, admin_state_up, status, shared, mtu, created_at, updated_at, project_id, external, COALESCE(description, '')
		FROM networks
		WHERE id::text = $1
		LIMIT 1
	`, networkID).Scan(&id, &name, &adminStateUp, &status, &shared, &mtu, &createdAt, &updatedAt, &owner, &external, &description)

	if errors.Is(err, database.ErrNoRows) {
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
		"description":     description,
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
		// Raw: absent gives upstream's default pool, [] and null give none (SPEC-033 FR-030).
		AllocationPools json.RawMessage `json:"allocation_pools"`
		// The project the subnet belongs to: an administrator's to name (SPEC-033 FR-058).
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
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

	if errors.Is(err, database.ErrNoRows) {
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
	version, refusal := req.ipVersion()
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}
	// The DNS servers as upstream accepts them, before anything is read or written: they go into the
	// dnsmasq configuration of the network's DHCP, which starts as root (SPEC-033 FR-060).
	if refusal := checkNameservers(req.Subnet.DNSNameservers, version); refusal != nil {
		common.SendError(c, refusal)
		return
	}

	projectID := c.GetString("project_id")

	// Only the target network's owner — or an administrator — may add a subnet to it.
	// Without this the network_id came straight from the request body and was never
	// checked against the caller's project, so any tenant could define the address
	// range, gateway and DHCP behaviour on any other tenant's network, and the victim's
	// own views then advertised it as theirs (cloud-storage#545 B8, FR-009/FR-010).
	//
	// This runs before the INSERT and before the network's DHCP is converged, so a refused request
	// leaves no subnet row and starts no address assignment on the target network.
	if !svc.callerMaySubnetNetwork(c, req.Subnet.NetworkID, projectID) {
		return
	}
	projectID, ok := svc.ownerOf(c, req.Subnet.ProjectID, req.Subnet.TenantID)
	if !ok {
		return
	}
	n, refusal := parseSubnetCreate(req, version)
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}

	// Insert into database. A DHCP subnet gives the network's DHCP port an address on it, or the port
	// itself when it is the network's first, under the network's lock (SPEC-033 R29); then the network's
	// DHCP is converged on the network node, and a create it cannot apply is undone (FR-008).
	ctx := c.Request.Context()
	subnetID := uuid.New().String()
	dhcp := n.enableDHCP && n.ipVersion == 4
	now := time.Now()
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if dhcp {
			if err := lockNetwork(ctx, tx, req.Subnet.NetworkID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, dns_nameservers, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, subnetID, req.Subnet.Name, req.Subnet.NetworkID, projectID, req.Subnet.CIDR, n.gatewayIP, n.ipVersion, n.enableDHCP, req.Subnet.DNSNameservers, now, now); err != nil {
			return err
		}
		if err := storePools(ctx, tx, subnetID, n.pools); err != nil {
			return err
		}
		if dhcp {
			return syncDHCPPort(ctx, tx, req.Subnet.NetworkID, []string{subnetID})
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Str("operation", "create_subnet").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create subnet"))
		return
	}
	if dhcp {
		if err := svc.convergeDHCPUnit(ctx, req.Subnet.NetworkID); err != nil {
			log.Error().Err(err).Str("subnet_id", subnetID).Msg("create a subnet: its network's DHCP could not be converged; the create was not applied")
			svc.undoSubnetCreate(ctx, subnetID, req.Subnet.NetworkID)
			common.SendError(c, common.NewInternalServerError("failed to create subnet"))
			return
		}
	}
	svc.answerSubnet(c, http.StatusCreated, subnetID)
}

// networkTypeAndMTU is the type and MTU of a network this node makes: VXLAN, its overhead taken off the MTU,
// where the node coordinates VXLAN; flat otherwise.
func (svc *Service) networkTypeAndMTU() (string, int) {
	if svc.vxlanCoordinator != nil {
		return "vxlan", 1450
	}
	return "flat", 1500
}

// ipVersion is the subnet's IP version, its CIDR's: a given ip_version must be 4 or 6 and match the CIDR,
// as upstream's attribute validator and _validate_ip_version (SPEC-033 FR-068). Every reader of an IPv4
// subnet takes its CIDR to be IPv4.
func (r CreateSubnetRequest) ipVersion() (int, *common.OpenStackError) {
	given := r.Subnet.IPVersion
	if given != 0 && given != 4 && given != 6 {
		return 0, invalidAttribute("ip_version", fmt.Sprintf("'%d' is not in [4, 6]", given))
	}
	prefix, err := parsePrefix(r.Subnet.CIDR)
	if err != nil {
		return 0, common.NewBadRequestError("invalid CIDR")
	}
	version := 4
	if prefix.Addr().Is6() {
		version = 6
	}
	if given != 0 && given != version {
		return 0, invalidInput(fmt.Sprintf("cidr '%s' does not match the ip_version '%d'", r.Subnet.CIDR, given))
	}
	return version, nil
}

// newSubnet is a subnet create as it is stored: its defaults filled in.
type newSubnet struct {
	ipVersion  int
	enableDHCP bool
	gatewayIP  *string
	pools      []ipRange
}

// parseSubnetCreate fills in a subnet create's defaults and checks them as upstream does: the CIDR; the
// gateway in three states, per FR-023 — omitted chooses the first host address, an explicit null none,
// a value is taken as given once checked, since it is handed verbatim to dnsmasq and a value nothing
// checked could take DHCP away from the whole network; and the allocation pools, the given ones
// validated as upstream validates them, or upstream's default with the gateway taken out (SPEC-033
// FR-030).
func parseSubnetCreate(req CreateSubnetRequest, ipVersion int) (newSubnet, *common.OpenStackError) {
	n := newSubnet{ipVersion: ipVersion, enableDHCP: true}
	if req.Subnet.EnableDHCP != nil {
		n.enableDHCP = *req.Subnet.EnableDHCP
	}
	_, ipNet, err := net.ParseCIDR(req.Subnet.CIDR)
	if err != nil {
		return n, common.NewBadRequestError("invalid CIDR")
	}
	gatewayIP, present, err := optionalString(req.Subnet.GatewayIP)
	if err != nil {
		return n, common.NewBadRequestError("invalid gateway_ip")
	}
	if gatewayIP != nil {
		if err := validateSubnetGateway(*gatewayIP, req.Subnet.CIDR); err != nil {
			return n, common.NewBadRequestError("invalid gateway_ip: " + err.Error())
		}
	}
	if !present {
		auto := incrementIP(ipNet.IP, 1).String()
		gatewayIP = &auto
	}
	n.gatewayIP = gatewayIP
	prefix, err := parsePrefix(req.Subnet.CIDR)
	if err != nil {
		return n, common.NewBadRequestError("invalid CIDR")
	}
	pools, poolsGiven, refusal := parsePoolsField(req.Subnet.AllocationPools)
	if refusal != nil {
		return n, refusal
	}
	if poolsGiven {
		if refusal := validatePools(prefix, parseGateway(gatewayIP), pools); refusal != nil {
			return n, refusal
		}
	} else {
		pools = generatePools(prefix, parseGateway(gatewayIP))
	}
	sortPools(pools)
	n.pools = pools
	return n, nil
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

// subnetWriteScope is the predicate under which a caller may change or delete a subnet: their own, or
// one on their network — upstream's `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`, no `shared`
// disjunct (SPEC-022 FR-013a). An administrator's write by id is any project's (SPEC-033 FR-058).
func subnetWriteScope(caller string) string {
	return "(s.project_id = " + caller + " OR n.project_id = " + caller + ")"
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
// caller's project id being $2 and whether the caller is an administrator $3: an administrator's
// listing spans every project, so its marker resolves in every project too, and a walk cannot 404
// mid-page on a row the listing just returned. A marker that does not resolve is the resource's 404,
// written here so the handler only has to return; a nil marker with ok true means none is in effect.
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
	// $1 the marker, $2 the caller's project, $3 whether the caller is an administrator, whose listing
	// spans every project (SPEC-033 FR-057).
	err := svc.pool.QueryRow(c.Request.Context(), query, marker, c.GetString("project_id"), isAdmin(c)).Scan(&m.createdAt, &m.id)
	if errors.Is(err, database.ErrNoRows) {
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
	// subnetReadScope); an administrator's listing spans every project, and project_id/tenant_id
	// narrow either (SPEC-033 FR-057).
	projectScope(c, f, "s.project_id", subnetReadScope)

	// Client filters (B6). The project attributes are projectScope's.
	addSubnetListFilter(f, subnetFilterParams{
		NetworkID:  c.Query("network_id"),
		ID:         c.Query("id"),
		Name:       c.Query("name"),
		CIDR:       c.Query("cidr"),
		IPVersion:  c.Query("ip_version"),
		EnableDHCP: c.Query("enable_dhcp"),
	})

	// Marker-based pagination on a TOTAL order. `created_at` alone is not unique, so ties made the
	// page boundary unstable and a walk could repeat or skip rows — the same defect #549 found in the
	// compute listings. ListNetworks already pages on (created_at, id); FR-025 requires the same here.
	// The marker itself resolves as resolveListMarker describes.
	m, ok := svc.resolveListMarker(c, limit, "subnet",
		"SELECT s.created_at, s.id FROM subnets s JOIN networks n ON s.network_id = n.id WHERE s.id::text = $1 AND "+scopeOrAdmin(subnetReadScope("$2"), "$3"))
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
		SELECT `+subnetColumns+`
		FROM subnets s
		JOIN networks n ON s.network_id = n.id`+where+`
		ORDER BY s.created_at DESC, s.id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_subnets").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list subnets"))
		return
	}

	// Collected first and rendered after the rows are closed, so the stored pools are read in one
	// query without holding a second connection.
	var views []subnetView
	for rows.Next() {
		v, err := scanSubnet(rows)
		if err != nil {
			// Log and keep serving: one unreadable row must not blank every caller's subnet list,
			// including the dashboard's, and must not vanish silently either (FR-024).
			log.Warn().Err(err).Str("subnet_id", v.id).Msg("skipping unreadable subnet row")
			continue
		}
		views = append(views, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}
	subnets, err := renderSubnets(c.Request.Context(), svc.pool, views)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_subnets").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list subnets"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"subnets": subnets})
}

// GetSubnet returns a single subnet
func (svc *Service) GetSubnet(c *gin.Context) {
	svc.answerSubnet(c, http.StatusOK, c.Param("id"))
}

// answerSubnet answers with a subnet as its show reads and renders it, by id or name: GetSubnet's
// answer, and a create's and an update's, which answer the subnet as it was stored (branch review
// T224).
//
// The visibility predicate must match ListSubnets: the owning NETWORK's project counts as well as the
// subnet's own, or an id returned by the listing 404s here (SPEC-022 FR-013a). This is also upstream's
// rule — neutron's get_subnet policy is `PROJECT_READER or rule:shared or ... or
// ADMIN_OR_NET_OWNER_READER`, where NET_OWNER_READER is `project_id:%(network:project_id)s`. id::text
// comparison resolves by UUID or name without crashing on a non-UUID identifier (the CLI looks up by
// name this way). Administrators may address a subnet by id across projects; name lookups stay in the
// caller's subnet read scope.
func (svc *Service) answerSubnet(c *gin.Context, status int, idOrName string) {
	ctx := c.Request.Context()
	v, err := scanSubnet(svc.pool.QueryRow(ctx, `
		SELECT `+subnetColumns+`
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE ((s.id::text = $1 AND `+scopeOrAdmin(subnetReadScope("$2"), "$3")+`)
		    OR (s.name = $1 AND `+subnetReadScope("$2")+`))
		ORDER BY (s.id::text = $1) DESC
		LIMIT 1
	`, idOrName, c.GetString("project_id"), isAdmin(c)))
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	var bodies []gin.H
	if err == nil {
		bodies, err = renderSubnets(ctx, svc.pool, []subnetView{v})
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_subnet").Str("subnet_id", idOrName).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get subnet"))
		return
	}
	c.JSON(status, gin.H{"subnet": bodies[0]})
}

// subnetColumns are the columns a subnet's show and listing read, in scanSubnet's order; the query
// aliases the subnet as s.
const subnetColumns = `s.id, s.name, s.network_id, s.cidr, s.gateway_ip, s.ip_version, s.enable_dhcp,
		       s.dns_nameservers, s.created_at, s.updated_at, s.project_id, s.pools_stored`

// subnetView is a subnet as its show and listing read it.
type subnetView struct {
	id, name, networkID, cidr string
	// gateway_ip is nullable (migration 001) and a subnet may legitimately have none
	// (cloud-storage#546 B6, FR-021/FR-022).
	gatewayIP *string
	// Nullable: subnets.project_id carries no NOT NULL, and pgx refuses a NULL into a plain string —
	// which ends the whole result set, not just this row. A NULL owner reads as tenant_id null.
	owner                *string
	ipVersion            int
	enableDHCP           bool
	dnsNameservers       []string
	createdAt, updatedAt time.Time
	poolsStored          *bool
}

// scanSubnet reads a row of subnetColumns.
func scanSubnet(row interface{ Scan(...any) error }) (subnetView, error) {
	var v subnetView
	err := row.Scan(&v.id, &v.name, &v.networkID, &v.cidr, &v.gatewayIP, &v.ipVersion, &v.enableDHCP, &v.dnsNameservers,
		&v.createdAt, &v.updatedAt, &v.owner, &v.poolsStored)
	return v, err
}

// renderSubnets is the subnets' bodies as their create, show, update and listing answer them, the pools
// stored with them read in one query. tenant_id is the subnet's own project, not the caller's: a
// listing holds subnets of the caller's networks and of shared ones, and an administrator's every
// project's.
func renderSubnets(ctx context.Context, q subnetQuerier, views []subnetView) ([]gin.H, error) {
	var stored []string
	for _, v := range views {
		if v.poolsStored != nil && *v.poolsStored {
			stored = append(stored, v.id)
		}
	}
	storedByID, err := storedPools(ctx, q, stored)
	if err != nil {
		return nil, err
	}
	out := make([]gin.H, 0, len(views))
	for _, v := range views {
		pools := []gin.H{}
		if prefix, err := parsePrefix(v.cidr); err == nil {
			pools = renderPools(poolsFor(prefix, parseGateway(v.gatewayIP), v.poolsStored != nil && *v.poolsStored, storedByID[v.id]))
		}
		dns := v.dnsNameservers
		if dns == nil {
			dns = []string{} // OpenStack clients format these as lists and fail on JSON null
		}
		out = append(out, gin.H{
			"id":               v.id,
			"name":             v.name,
			"network_id":       v.networkID,
			"tenant_id":        v.owner,
			"cidr":             v.cidr,
			"gateway_ip":       v.gatewayIP,
			"ip_version":       v.ipVersion,
			"enable_dhcp":      v.enableDHCP,
			"dns_nameservers":  dns,
			"allocation_pools": pools,
			"host_routes":      []gin.H{},
			"service_types":    []string{},
			"created_at":       v.createdAt.Format(time.RFC3339),
			"updated_at":       v.updatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}

// DeleteSubnet deletes a subnet. A subnet a port other than the network's DHCP port has an address on is in
// use, as upstream's (SPEC-033 FR-052): checked first, before the network node is asked anything, and again
// under the subnet's lock, which every allocation on it takes, so no port is given an address on it between
// the check and the delete. A DHCP subnet leaves the network's DHCP first (FR-040): the network node
// converges it as if the subnet were gone, then the record goes, with the DHCP port's address on it, or the
// port with the network's last DHCP subnet. A failure in either converges the DHCP from the records again.
func (svc *Service) DeleteSubnet(c *gin.Context) {
	ctx := c.Request.Context()
	s, err := resolveDeletableSubnet(ctx, svc.pool, c.Param("id"), scopeOf(c))
	if err == nil {
		err = requireSubnetUnused(ctx, svc.pool, s)
	}
	if err != nil {
		sendChangeError(c, err, "delete", "subnet")
		return
	}
	if s.dhcp {
		if err := svc.convergeDHCPUnit(ctx, s.network, s.id); err != nil {
			log.Error().Err(err).Str("subnet_id", s.id).Msg("delete a subnet: its network's DHCP could not be converged without it; the subnet stays")
			svc.reapplyDHCP(ctx, s.network)
			common.SendError(c, common.NewInternalServerError("failed to delete subnet"))
			return
		}
	}
	if err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error { return deleteSubnetRecord(ctx, tx, s) }); err != nil {
		if s.dhcp {
			svc.reapplyDHCP(ctx, s.network)
		}
		sendChangeError(c, err, "delete", "subnet")
		return
	}
	c.Status(http.StatusNoContent)
}

// deletableSubnet is a subnet a delete resolved: its id, its network, and whether its network's DHCP serves
// it (an IPv4 subnet with DHCP on).
type deletableSubnet struct {
	id, network string
	dhcp        bool
}

// resolveDeletableSubnet resolves a delete's subnet by id or name. Scope: the subnet's own project, or the
// owner of the network it sits on — upstream's delete_subnet is `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`,
// so a network's owner may remove a subnet another project attached to it. Deliberately no `shared`
// disjunct, unlike GetSubnet: sharing a network lets others read its subnets, never delete them (SPEC-022
// FR-013a). The delete then goes by the resolved id: re-filtering on project would no-op for a network owner
// removing another project's subnet, a 204 with the row still present.
func resolveDeletableSubnet(ctx context.Context, q subnetQuerier, ref string, caller callerScope) (deletableSubnet, error) {
	var s deletableSubnet
	var enableDHCP bool
	var ipVersion int
	err := q.QueryRow(ctx, `
		SELECT s.id, s.network_id, s.enable_dhcp, s.ip_version
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE ((s.id::text = $1 AND `+scopeOrAdmin(subnetWriteScope("$2"), "$3")+`)
		    OR (s.name = $1 AND `+subnetWriteScope("$2")+`))
		ORDER BY (s.id::text = $1) DESC
		LIMIT 1
	`, ref, caller.project, caller.admin).Scan(&s.id, &s.network, &enableDHCP, &ipVersion)
	if errors.Is(err, database.ErrNoRows) {
		return s, common.NewNotFoundError("subnet")
	}
	s.dhcp = enableDHCP && ipVersion == 4
	return s, err
}

// requireSubnetUnused refuses a subnet in use (SubnetInUse, FR-052).
func requireSubnetUnused(ctx context.Context, q subnetQuerier, s deletableSubnet) error {
	inUse, err := subnetInUse(ctx, q, s.id, s.network)
	if err != nil {
		return err
	}
	if inUse {
		return errSubnetInUse(s.id)
	}
	return nil
}

// deleteSubnetRecord deletes a subnet in the caller's transaction, under its lock, still unused; then, for a
// DHCP subnet, the network's DHCP port drops its address there, under the network's lock (R29).
func deleteSubnetRecord(ctx context.Context, tx database.Tx, s deletableSubnet) error {
	// The caller found the subnet, so no row here means a concurrent delete got there first: 404, not
	// a 500.
	if _, err := LockSubnet(ctx, tx, s.id); errors.Is(err, database.ErrNoRows) {
		return common.NewNotFoundError("subnet")
	} else if err != nil {
		return err
	}
	if err := requireSubnetUnused(ctx, tx, s); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM subnets WHERE id = $1", s.id); err != nil {
		return err
	}
	if !s.dhcp {
		return nil
	}
	if err := lockNetwork(ctx, tx, s.network); err != nil {
		return err
	}
	return syncDHCPPort(ctx, tx, s.network, nil)
}

// undoSubnetCreate takes back a subnet create whose network's DHCP could not be converged (FR-008): the
// subnet goes, with its DHCP port's address, unless another request has changed it or given a port an
// address on it since; then the network's DHCP is converged from the records again.
func (svc *Service) undoSubnetCreate(ctx context.Context, subnetID, networkID string) {
	ctx = context.WithoutCancel(ctx)
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if _, err := LockSubnet(ctx, tx, subnetID); err != nil {
			return err
		}
		inUse, err := subnetInUse(ctx, tx, subnetID, networkID)
		if err != nil || inUse {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM subnets WHERE id = $1 AND updated_at = created_at", subnetID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := lockNetwork(ctx, tx, networkID); err != nil {
			return err
		}
		return syncDHCPPort(ctx, tx, networkID, nil)
	})
	if err != nil && !errors.Is(err, database.ErrNoRows) { // no row: the subnet, or its network, is gone already
		log.Error().Err(err).Str("subnet_id", subnetID).Msg("undo a subnet create; the subnet stays")
	}
	svc.reapplyDHCP(ctx, networkID)
}

// routerPortHolding returns the id of the router port holding ip on the subnet, or "" when none does —
// upstream's get_alloc_routerports. A router interface is a port carrying the subnet's gateway in its
// fixed_ips, recorded in router_interfaces.
func routerPortHolding(ctx context.Context, q subnetQuerier, subnetID, ip string) (string, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, p.fixed_ips
		FROM router_interfaces ri
		JOIN ports p ON p.id = ri.port_id
		WHERE ri.subnet_id = $1
	`, subnetID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var portID string
		var fixedIPs common.FixedIPs
		if err := rows.Scan(&portID, &fixedIPs); err != nil {
			return "", err
		}
		if fixedIPs.Holds(ip) {
			return portID, nil
		}
	}
	return "", rows.Err()
}

// UpdateSubnet updates a subnet's name, gateway, DHCP flag and allocation pools. Nothing supplied writes nothing,
// and the request still SUCCEEDS: upstream Neutron accepts an empty update and answers 200 with the
// unchanged resource, and Terraform and Horizon both send updates that can reduce to nothing (second
// review, AGY-2).
func (svc *Service) UpdateSubnet(c *gin.Context) {
	var req updateSubnetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	u, refusal := parseSubnetUpdate(req)
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}
	s, ok := svc.resolveWritableSubnet(c, c.Param("id"))
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if refusal := gatewayChangeRefusal(s, u); refusal != nil {
		common.SendError(c, refusal)
		return
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	var before subnetBefore
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var err error
		before, err = storeSubnetUpdate(ctx, tx, s, u, at)
		return err
	})
	if err != nil {
		sendChangeError(c, err, "update", "subnet")
		return
	}
	if gatewayChanged := u.gatewayChanged(s); gatewayChanged || before.dhcpWritten {
		if err := svc.applySubnetUpdate(context.WithoutCancel(ctx), s, before, at, gatewayChanged); err != nil {
			common.SendError(c, common.NewInternalServerError("failed to update subnet"))
			return
		}
	}
	svc.answerSubnet(c, http.StatusOK, s.id)
}

// updateSubnetRequest is a subnet update's body.
type updateSubnetRequest struct {
	Subnet struct {
		Name *string `json:"name"`
		// Raw for the same reason as CreateSubnet: an explicit null clears the gateway, an omitted key
		// leaves it alone (FR-023, contract S14).
		GatewayIP json.RawMessage `json:"gateway_ip"`
		// 'subnet set --dhcp/--no-dhcp' PUTs it; the handler had no binding for it, so the flag was a
		// 200 no-op (SPEC-031).
		EnableDHCP *bool `json:"enable_dhcp"`
		// Raw: given, they replace the subnet's pools whole (SPEC-033 FR-030).
		AllocationPools json.RawMessage `json:"allocation_pools"`
	} `json:"subnet"`
}

// subnetUpdate is a subnet update as parsed: what it gives, and whether it gives a gateway and pools.
type subnetUpdate struct {
	name                     *string
	gateway                  *string
	enableDHCP               *bool
	gatewayGiven, poolsGiven bool
	pools                    []ipRange
}

func parseSubnetUpdate(req updateSubnetRequest) (subnetUpdate, *common.OpenStackError) {
	u := subnetUpdate{name: req.Subnet.Name, enableDHCP: req.Subnet.EnableDHCP}
	var err error
	if u.gateway, u.gatewayGiven, err = optionalString(req.Subnet.GatewayIP); err != nil {
		return u, common.NewBadRequestError("invalid gateway_ip")
	}
	var refusal *common.OpenStackError
	u.pools, u.poolsGiven, refusal = parsePoolsField(req.Subnet.AllocationPools)
	return u, refusal
}

// gatewayChanged reports whether the update changes the subnet's gateway.
func (u subnetUpdate) gatewayChanged(s writableSubnet) bool {
	return u.gatewayGiven && derefOr(u.gateway, "") != derefOr(s.gateway, "")
}

// writableSubnet is the subnet an update changes, as its write scope resolved it.
type writableSubnet struct {
	id, network, cidr string
	gateway           *string
	dhcp              bool
	version           int
}

// resolveWritableSubnet resolves an update's subnet, by UUID or name, to its canonical id, answering 404
// or 500 itself. Scoped like DeleteSubnet — the subnet's own project or the owner of the network it
// sits on, no `shared` disjunct — because upstream's update_subnet and delete_subnet carry the same
// rule (`PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`). Renaming is strictly weaker than removing, so a
// scope that allows the delete and refuses the rename would be an odd place to draw a line (SPEC-022
// FR-013a).
func (svc *Service) resolveWritableSubnet(c *gin.Context, idOrName string) (writableSubnet, bool) {
	var s writableSubnet
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT s.id, s.network_id, s.cidr, s.gateway_ip, s.enable_dhcp, s.ip_version
		FROM subnets s
		JOIN networks n ON s.network_id = n.id
		WHERE ((s.id::text = $1 AND `+scopeOrAdmin(subnetWriteScope("$2"), "$3")+`)
		    OR (s.name = $1 AND `+subnetWriteScope("$2")+`))
		ORDER BY (s.id::text = $1) DESC
		LIMIT 1
	`, idOrName, c.GetString("project_id"), isAdmin(c)).Scan(&s.id, &s.network, &s.cidr, &s.gateway, &s.dhcp, &s.version)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return s, false
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_subnet").Str("subnet_id", idOrName).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update subnet"))
		return s, false
	}
	return s, true
}

// gatewayChangeRefusal checks a given gateway as on create. Whether a router interface holds the current
// one is checked under the subnet's lock (requireGatewayFree).
func gatewayChangeRefusal(s writableSubnet, u subnetUpdate) *common.OpenStackError {
	if u.gatewayGiven && u.gateway != nil {
		if err := validateSubnetGateway(*u.gateway, s.cidr); err != nil {
			return common.NewBadRequestError("invalid gateway_ip: " + err.Error())
		}
	}
	return nil
}

// requireGatewayFree refuses to change or clear a subnet's gateway, current as read under the subnet's lock,
// while a router interface holds it (upstream's GatewayIpInUse): under that lock, the lock an interface's
// add takes before it takes the gateway, so no interface is added on the old gateway meanwhile (SPEC-033
// FR-055, T269).
func requireGatewayFree(ctx context.Context, tx database.Tx, subnetID string, current *string, u subnetUpdate) error {
	if !u.gatewayGiven || current == nil || derefOr(u.gateway, "") == *current {
		return nil
	}
	holder, err := routerPortHolding(ctx, tx, subnetID, *current)
	if err != nil {
		return err
	}
	if holder != "" {
		return common.NewNeutronError(http.StatusConflict, "GatewayIpInUse",
			fmt.Sprintf("Current gateway ip %s already in use by port %s. Unable to update.", *current, holder))
	}
	return nil
}

// storeSubnetUpdate stores an update, by resolved id only — the entitlement query has already decided;
// carrying project_id here too would make a network owner's rename a silent no-op, 200 with the old
// name still stored, because the two predicates disagree (the same trap DeleteSubnet had). gateway_ip
// is only touched when the key was supplied; an explicit null clears it (S14).
//
// The pools and the gateway are checked against each other under the subnet's lock, which every
// allocation on it takes (SPEC-033 FR-030): given pools are validated as on create, against the gateway
// the subnet will have; a new gateway may not fall in the pools the subnet will have. A subnet from
// before migration 084 has its default pools stored first when its gateway changes, so they stay what
// they were rather than move with the gateway, as upstream's stored pools would. The DHCP flag is written
// only when it flips, and the network's DHCP port follows it: an IPv4 subnet's DHCP turned on gives the
// port an address on the subnet, turned off takes it away (SPEC-031, R29). It returns what the update
// changed, as it was, for its undo.
func storeSubnetUpdate(ctx context.Context, tx database.Tx, s writableSubnet, u subnetUpdate, at time.Time) (subnetBefore, error) {
	var before subnetBefore
	if _, err := LockSubnet(ctx, tx, s.id); err != nil {
		return before, err
	}
	current, err := readSubnet(ctx, tx, s.id)
	if err != nil {
		return before, err
	}
	var stored *bool
	if err := tx.QueryRow(ctx, "SELECT name, gateway_ip, enable_dhcp, pools_stored FROM subnets WHERE id = $1", s.id).
		Scan(&before.name, &before.gateway, &before.dhcp, &stored); err != nil {
		return before, err
	}
	before.poolsStored, before.pools = stored, current.pools
	if err := requireGatewayFree(ctx, tx, s.id, before.gateway, u); err != nil {
		return before, err
	}
	gateway := current.gateway
	if u.gatewayGiven {
		gateway = parseGateway(u.gateway)
	}
	pools := current.pools
	switch {
	case u.poolsGiven:
		if refusal := validatePools(current.prefix, gateway, u.pools); refusal != nil {
			return before, refusal
		}
		pools = u.pools
	case u.gatewayGiven:
		if refusal := gatewayOutOfPools(gateway, current.pools); refusal != nil {
			return before, refusal
		}
	}
	set := &common.SQLUpdate{}
	if u.name != nil {
		set.Set("name", *u.name)
	}
	if u.gatewayGiven {
		set.Set("gateway_ip", u.gateway)
	}
	if u.enableDHCP != nil && *u.enableDHCP != before.dhcp {
		set.Set("enable_dhcp", *u.enableDHCP)
		before.dhcpWritten = true
	}
	if !set.Empty() || u.poolsGiven {
		set.Set("updated_at", at)
		if _, err := tx.Exec(ctx, "UPDATE subnets SET "+set.Assignments()+" WHERE id = "+set.Arg(s.id), set.Args()...); err != nil {
			return before, err
		}
	}
	if u.poolsGiven || (u.gatewayGiven && (stored == nil || !*stored)) {
		before.poolsWritten = true
		sortPools(pools)
		if err := storePools(ctx, tx, s.id, pools); err != nil {
			return before, err
		}
	}
	if before.dhcpWritten && s.version == 4 {
		return before, syncSubnetDHCPPort(ctx, tx, s.network, s.id)
	}
	return before, nil
}

// syncSubnetDHCPPort brings a network's DHCP port in line after one of its subnets' DHCP flag flipped,
// allocating on that subnet only, whose lock the caller holds: the network's lock is taken after it, as
// lockNetwork requires (R29).
func syncSubnetDHCPPort(ctx context.Context, tx database.Tx, networkID, subnetID string) error {
	if err := lockNetwork(ctx, tx, networkID); err != nil {
		return err
	}
	return syncDHCPPort(ctx, tx, networkID, []string{subnetID})
}

// applySubnetUpdate applies a changed gateway or DHCP flag before the answer (Article II, FR-045): the
// network's DHCP, whose router option the gateway is and which serves the subnet while its flag is on,
// and, for a changed gateway, the routers whose gateway port has an address on the subnet, whose default
// route it is, as upstream notifies them (_notify_subnet_gateway_ip_update; branch T189). When one cannot
// take it, the update is undone, compare-and-set (FR-008), and they converge back.
func (svc *Service) applySubnetUpdate(ctx context.Context, s writableSubnet, before subnetBefore, at time.Time, gatewayChanged bool) error {
	var routers []string
	var err error
	if gatewayChanged {
		routers, err = subnetGatewayRouters(ctx, svc.pool, s.network, s.id)
	}
	if err == nil && (before.dhcp || before.dhcpWritten) && s.version == 4 {
		err = svc.convergeDHCPUnit(ctx, s.network)
	}
	if err == nil {
		err = svc.convergeRouterUnits(ctx, routers...)
	}
	if err != nil {
		log.Error().Err(err).Str("subnet_id", s.id).Msg("update a subnet: it could not be applied; the update was not applied")
		svc.undoSubnetUpdate(ctx, s, before, at)
		for _, router := range routers {
			svc.reapplyRouter(ctx, router)
		}
	}
	return err
}

// subnetBefore is what an update can change of a subnet, as it was before the update.
type subnetBefore struct {
	name         string
	gateway      *string
	dhcp         bool
	poolsStored  *bool
	pools        []ipRange
	poolsWritten bool // the update stored pools
	dhcpWritten  bool // the update flipped the DHCP flag
}

// undoSubnetUpdate takes back a subnet update whose network's DHCP could not be converged (FR-008): the
// name, gateway, DHCP flag and pools go back to before, the DHCP port following the flag, unless another
// update came since (its updated_at is no longer at) or something took the previous gateway's address
// meanwhile (SPEC-033 T188); then the network's DHCP is converged from the records again.
func (svc *Service) undoSubnetUpdate(ctx context.Context, s writableSubnet, before subnetBefore, at time.Time) {
	ctx = context.WithoutCancel(ctx)
	subnetID, networkID := s.id, s.network
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, floatingIPAllocationLock); err != nil {
			return err
		}
		var current *string
		if err := tx.QueryRow(ctx, "SELECT gateway_ip FROM subnets WHERE id = $1 FOR UPDATE", subnetID).Scan(&current); err != nil {
			return err
		}
		if before.gateway != nil && (current == nil || *current != *before.gateway) {
			if err := requireAddressesFree(ctx, tx, networkID, []string{*before.gateway}); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, "UPDATE subnets SET name = $1, gateway_ip = $2, enable_dhcp = $3, updated_at = $4 WHERE id = $5 AND updated_at = $6",
			before.name, before.gateway, before.dhcp, time.Now(), subnetID, at)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if before.poolsWritten {
			if err := restoreSubnetPools(ctx, tx, subnetID, before); err != nil {
				return err
			}
		}
		if before.dhcpWritten && s.version == 4 {
			return syncSubnetDHCPPort(ctx, tx, networkID, subnetID)
		}
		return nil
	})
	if err != nil && !errors.Is(err, database.ErrNoRows) { // no row: the subnet is gone already
		log.Error().Err(err).Str("subnet_id", subnetID).Msg("undo a subnet update; the update stays")
	}
	svc.reapplyDHCP(ctx, networkID)
}

// restoreSubnetPools puts back the pools an update stored: the earlier stored pools, or none stored, as
// before.
func restoreSubnetPools(ctx context.Context, tx database.Tx, subnetID string, before subnetBefore) error {
	if before.poolsStored != nil && *before.poolsStored {
		return storePools(ctx, tx, subnetID, before.pools)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM subnet_allocation_pools WHERE subnet_id = $1", subnetID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE subnets SET pools_stored = $1 WHERE id = $2", before.poolsStored, subnetID)
	return err
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

func generateMAC() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	buf[0] = (buf[0] | 2) & 0xfe // Set local bit, clear multicast bit
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}
