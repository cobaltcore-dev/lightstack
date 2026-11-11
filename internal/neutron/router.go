package neutron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Router represents a Neutron L3 router
type Router struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	ProjectID           string         `json:"tenant_id"` // Keep tenant_id for compatibility
	AdminStateUp        bool           `json:"admin_state_up"`
	Status              string         `json:"status"`
	ExternalGatewayInfo map[string]any `json:"external_gateway_info"`
	Distributed         bool           `json:"distributed"`
	HA                  bool           `json:"ha"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

// FloatingIP represents a floating IP address
type FloatingIP struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"tenant_id"`
	FloatingNetworkID string    `json:"floating_network_id"`
	FloatingIPAddress string    `json:"floating_ip_address"`
	FixedIPAddress    string    `json:"fixed_ip_address"`
	PortID            string    `json:"port_id"`
	RouterID          string    `json:"router_id"`
	Status            string    `json:"status"`
	Description       string    `json:"description"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// CreateRouterRequest represents a router creation request
type CreateRouterRequest struct {
	Router struct {
		Name                string         `json:"name" binding:"required"`
		AdminStateUp        *bool          `json:"admin_state_up"`
		ExternalGatewayInfo map[string]any `json:"external_gateway_info"`
		Distributed         *bool          `json:"distributed"`
		HA                  *bool          `json:"ha"`
	} `json:"router"`
}

// UpdateRouterRequest represents a router update request
type UpdateRouterRequest struct {
	Router struct {
		Name         *string `json:"name"`
		AdminStateUp *bool   `json:"admin_state_up"`
		// Raw, so an omitted key stays apart from an explicit null and an empty object — both of
		// which clear the gateway (see routerGatewayUpdate).
		ExternalGatewayInfo json.RawMessage `json:"external_gateway_info"`
	} `json:"router"`
}

// ListRouters lists all routers for the project
func (svc *Service) ListRouters(c *gin.Context) {
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
	// The caller's project — unless an administrator filters by tenant:
	// upstream honours the filter for admin tokens, and it was silently
	// ignored before, so `router list --project X` presented the admin's
	// routers as project X's (SPEC-028; mirrors buildNetworkListFilter's
	// tenant bypass). ::text so a non-UUID value narrows to nothing instead
	// of failing the cast.
	routerTenant := c.Query("tenant_id")
	if routerTenant == "" {
		routerTenant = c.Query("project_id")
	}
	routerAdminBypass := routerTenant != "" && common.HasRole(c, common.RoleAdmin)
	if routerAdminBypass {
		f.Eq("project_id::text", routerTenant)
	} else {
		f.Eq("project_id", projectID)
	}

	// Marker paging over a TOTAL order. created_at alone is not unique, so the order was undefined
	// among ties AND `created_at < $n` excluded every row sharing the marker's timestamp rather than
	// only those before it — a page walk silently dropped them. id is the primary key, so
	// (created_at, id) is total (as #549 established for compute). The marker resolves under the
	// listing's own scope: an unresolvable marker would silently drop the condition and make the
	// walk restart from the first page forever.
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		markerQuery := "SELECT created_at, id FROM routers WHERE id = $1"
		markerArgs := []any{marker}
		if !routerAdminBypass {
			markerQuery += " AND project_id = $2"
			markerArgs = append(markerArgs, projectID)
		}
		err := svc.pool.QueryRow(c.Request.Context(), markerQuery, markerArgs...).
			Scan(&markerCreatedAt, &markerID)
		if err == nil {
			f.Cond(fmt.Sprintf("(created_at, id) < (%s, %s)", f.Arg(markerCreatedAt), f.Arg(markerID)))
		}
	}

	where := f.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, project_id, admin_state_up, status, external_gateway_info,
		       distributed, ha, created_at, updated_at
		FROM routers`+where+`
		ORDER BY created_at DESC, id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_routers").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list routers"))
		return
	}
	defer rows.Close()

	var routers []gin.H
	for rows.Next() {
		var r Router
		var gatewayInfo sql.NullString

		if err := rows.Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
			&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}

		// Parse external gateway info. A malformed value leaves
		// ExternalGatewayInfo nil — the API response then omits it,
		// which is the right behavior for a corrupt row.
		if gatewayInfo.Valid {
			if err := json.Unmarshal([]byte(gatewayInfo.String), &r.ExternalGatewayInfo); err != nil {
				log.Warn().Err(err).Str("router_id", r.ID).Msg("failed to unmarshal external_gateway_info; treating as unset")
			}
		}

		routers = append(routers, gin.H{
			"id":                    r.ID,
			"name":                  r.Name,
			"tenant_id":             r.ProjectID,
			"project_id":            r.ProjectID,
			"admin_state_up":        r.AdminStateUp,
			"status":                r.Status,
			"external_gateway_info": r.ExternalGatewayInfo,
			"distributed":           r.Distributed,
			"ha":                    r.HA,
			"created_at":            r.CreatedAt.Format(time.RFC3339),
			"updated_at":            r.UpdatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if routers == nil {
		routers = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"routers": routers})
}

// CreateRouter creates a new L3 router
func (svc *Service) CreateRouter(c *gin.Context) {
	var req CreateRouterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	routerID := uuid.New().String()

	adminStateUp := true
	if req.Router.AdminStateUp != nil {
		adminStateUp = *req.Router.AdminStateUp
	}

	distributed := false
	if req.Router.Distributed != nil {
		distributed = *req.Router.Distributed
	}

	ha := false
	if req.Router.HA != nil {
		ha = *req.Router.HA
	}

	// Serialize external gateway info to JSON
	var gatewayInfoJSON []byte
	if req.Router.ExternalGatewayInfo != nil {
		networkID, ok := gatewayNetworkID(c, req.Router.ExternalGatewayInfo)
		if !ok || !requireExternalNetwork(c, svc.pool, networkID, "a router gateway") {
			return
		}
		gatewayInfoJSON, _ = json.Marshal(req.Router.ExternalGatewayInfo)
	}

	// Create router namespace
	if err := svc.routerManager.CreateRouterNamespace(routerID); err != nil {
		log.Error().Err(err).Str("operation", "create_router_namespace").Str("router_id", routerID).Msg("failed to create router namespace")
		common.SendError(c, common.NewInternalServerError("failed to create router namespace"))
		return
	}

	// Insert into database
	now := time.Now()
	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO routers (id, name, project_id, admin_state_up, status, external_gateway_info, distributed, ha, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, routerID, req.Router.Name, projectID, adminStateUp, "ACTIVE", gatewayInfoJSON, distributed, ha, now, now)

	if err != nil {
		// Cleanup on error (best-effort; the DB error is the user-facing one)
		if cleanupErr := svc.routerManager.DeleteRouterNamespace(routerID); cleanupErr != nil {
			log.Warn().Err(cleanupErr).Str("router_id", routerID).Msg("rollback: failed to delete router namespace after DB insert failure")
		}
		log.Error().Err(err).Str("operation", "create_router").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create router"))
		return
	}

	// If external gateway is configured, set it up
	if req.Router.ExternalGatewayInfo != nil {
		if err := svc.configureExternalGateway(c.Request.Context(), routerID, req.Router.ExternalGatewayInfo); err != nil {
			// Log error but don't fail the creation
			log.Warn().Err(err).Str("router_id", routerID).Msg("failed to configure external gateway")
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"router": gin.H{
			"id":                    routerID,
			"name":                  req.Router.Name,
			"tenant_id":             projectID,
			"project_id":            projectID,
			"admin_state_up":        adminStateUp,
			"status":                "ACTIVE",
			"external_gateway_info": req.Router.ExternalGatewayInfo,
			"distributed":           distributed,
			"ha":                    ha,
			"created_at":            now.Format(time.RFC3339),
			"updated_at":            now.Format(time.RFC3339),
		},
	})
}

// GetRouter retrieves a single router
func (svc *Service) GetRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")

	var r Router
	var gatewayInfo sql.NullString

	// id::text comparison resolves by UUID or name without crashing on a
	// non-UUID identifier — the CLI looks a router up by name this way
	// (consistent with GetNetwork/GetSubnet). An administrator may show any
	// router (upstream's get_router policy is admin_or_owner), so the
	// tenant-filtered listing's rows do not dead-end their follow-up show
	// with 404 — the same carve-out the other reads got (SPEC-028). A
	// non-admin keeps the project scope.
	routerQuery := `
		SELECT id, name, project_id, admin_state_up, status, external_gateway_info,
		       distributed, ha, created_at, updated_at
		FROM routers
		WHERE (id::text = $1 OR name = $1)`
	routerArgs := []any{routerID}
	if !common.HasRole(c, common.RoleAdmin) {
		routerQuery += ` AND project_id = $2`
		routerArgs = append(routerArgs, projectID)
	}
	err := svc.pool.QueryRow(c.Request.Context(), routerQuery+" LIMIT 1", routerArgs...).
		Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
			&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get router"))
		return
	}

	// Parse external gateway info. A malformed value leaves
	// ExternalGatewayInfo nil — the API response then omits it, which
	// is the right behavior for a corrupt row.
	if gatewayInfo.Valid {
		if err := json.Unmarshal([]byte(gatewayInfo.String), &r.ExternalGatewayInfo); err != nil {
			log.Warn().Err(err).Str("router_id", r.ID).Msg("failed to unmarshal external_gateway_info; treating as unset")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"router": gin.H{
			"id":                    r.ID,
			"name":                  r.Name,
			"tenant_id":             r.ProjectID,
			"project_id":            r.ProjectID,
			"admin_state_up":        r.AdminStateUp,
			"status":                r.Status,
			"external_gateway_info": r.ExternalGatewayInfo,
			"distributed":           r.Distributed,
			"ha":                    r.HA,
			"created_at":            r.CreatedAt.Format(time.RFC3339),
			"updated_at":            r.UpdatedAt.Format(time.RFC3339),
		},
	})
}

// UpdateRouter updates an existing router
func (svc *Service) UpdateRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var req UpdateRouterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	gateway, gatewayGiven, err := routerGatewayUpdate(req.Router.ExternalGatewayInfo)
	if err != nil {
		common.SendError(c, common.NewBadRequestError("invalid external_gateway_info"))
		return
	}

	// Authorise first. The gateway's host side used to run before anything confirmed the caller owns
	// the router: a project that knew another's router id could install egress SNAT for that router's
	// subnets — egress its owner never configured — and was then told 404.
	id, ok := svc.resolveOwnRouter(c, routerID, projectID)
	if !ok {
		return
	}

	u := &common.SQLUpdate{}

	if req.Router.Name != nil {
		u.Set("name", *req.Router.Name)
	}

	if req.Router.AdminStateUp != nil {
		u.Set("admin_state_up", *req.Router.AdminStateUp)
	}

	if gatewayGiven {
		if gateway == nil {
			u.Set("external_gateway_info", nil)
		} else {
			networkID, ok := gatewayNetworkID(c, gateway)
			if !ok || !requireExternalNetwork(c, svc.pool, networkID, "a router gateway") {
				return
			}
			gatewayInfoJSON, _ := json.Marshal(gateway)
			u.Set("external_gateway_info", gatewayInfoJSON)
		}
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	u.Set("updated_at", time.Now())

	query := "UPDATE routers SET " + u.Assignments() +
		" WHERE id = " + u.Arg(id) + " AND project_id = " + u.Arg(projectID)

	tag, err := svc.pool.Exec(ctx, query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_router").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update router"))
		return
	}
	// Ownership was settled above, so matching no row here means a concurrent delete got there first.
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}

	// The host follows the row. A gateway that went away, or stopped masquerading, takes back the
	// egress SNAT it gave the router's subnets — which used to stay in place for good.
	if gatewayGiven {
		if gateway != nil {
			if err := svc.configureExternalGateway(ctx, id, gateway); err != nil {
				log.Warn().Err(err).Str("router_id", id).Msg("failed to configure external gateway")
			}
		}
		if gateway == nil || !gatewaySNATOn(gateway) {
			svc.withdrawRouterEgressSNAT(ctx, id)
		}
	}

	// Return updated router
	svc.GetRouter(c)
}

// DeleteRouter deletes a router
func (svc *Service) DeleteRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Authorise first. The namespace used to go before the owner-scoped delete, which then answered
	// 204 whatever it matched, and the unscoped in-use count answered 409 for a router the caller
	// could not even see.
	id, ok := svc.resolveOwnRouter(c, routerID, projectID)
	if !ok {
		return
	}

	var interfaceCount int
	if err := svc.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM router_interfaces WHERE router_id = $1", id,
	).Scan(&interfaceCount); err != nil {
		log.Error().Err(err).Str("operation", "delete_router_check").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to check router interfaces"))
		return
	}
	if interfaceCount > 0 {
		common.SendError(c, common.NewResourceInUseError("router", "delete router",
			fmt.Sprintf("router %s still has %d interface(s) attached", id, interfaceCount)))
		return
	}

	tag, err := svc.pool.Exec(ctx, "DELETE FROM routers WHERE id = $1 AND project_id = $2", id, projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_router").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete router"))
		return
	}
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}

	// The host follows the row. A router with no interfaces masquerades nothing, so there is no egress
	// SNAT of its own to withdraw.
	if err := svc.routerManager.DeleteRouterNamespace(id); err != nil {
		log.Warn().Err(err).Str("router_id", id).Msg("failed to delete router namespace")
	}

	c.Status(http.StatusNoContent)
}

// AddRouterInterface adds an interface (subnet) to a router
func (svc *Service) AddRouterInterface(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var req struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	if req.SubnetID == "" && req.PortID == "" {
		common.SendError(c, common.NewBadRequestError("either subnet_id or port_id must be specified"))
		return
	}

	// Authorise through the same resolver RemoveRouterInterface uses: the
	// raw EXISTS lookup here did the same job for a UUID, but went unwrapped
	// for the ownership question this answer now settles the same way.
	id, ok := svc.resolveOwnRouter(c, routerID, projectID)
	if !ok {
		return
	}

	var subnetID, networkID, cidr, portID string
	var gatewayIP *string
	if req.PortID != "" {
		// port_id path — attach a port the caller already owns. The handler
		// used to ignore the field entirely and read the (empty) subnet_id
		// instead, so 'router add port' answered 500 "failed to get subnet"
		// (SPEC-028).
		if _, err := uuid.Parse(req.PortID); err != nil {
			common.SendError(c, common.NewNotFoundError("port"))
			return
		}
		var fixedIPsJSON []byte
		var deviceID sql.NullString
		err := svc.pool.QueryRow(ctx, `
			SELECT id::text, network_id::text, fixed_ips, device_id
			FROM ports
			WHERE id = $1 AND project_id = $2
		`, req.PortID, projectID).Scan(&portID, &networkID, &fixedIPsJSON, &deviceID)
		if errors.Is(err, database.ErrNoRows) {
			common.SendError(c, common.NewNotFoundError("port"))
			return
		}
		if err != nil {
			log.Error().Err(err).Str("operation", "add_router_interface_port_lookup").Str("port_id", req.PortID).Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to get port"))
			return
		}
		if deviceID.Valid && deviceID.String != "" {
			common.SendError(c, common.NewResourceInUseError("port", "attach port to router",
				"Port "+portID+" already has a device assigned and cannot be attached to a router."))
			return
		}
		var fixedIPs []struct {
			SubnetID string `json:"subnet_id"`
		}
		if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil || len(fixedIPs) == 0 || fixedIPs[0].SubnetID == "" {
			common.SendError(c, common.NewBadRequestError("port has no subnet; use a port with a fixed IP from a subnet"))
			return
		}
		subnetID = fixedIPs[0].SubnetID
	}

	// Subnet row — reached from either path (the caller's subnet_id, or the
	// subnet the port's first fixed IP comes from).
	if req.PortID == "" {
		if _, err := uuid.Parse(req.SubnetID); err != nil {
			common.SendError(c, common.NewNotFoundError("subnet"))
			return
		}
		subnetID = req.SubnetID
	}
	// The subnet must be the caller's, or sit on a network the caller can
	// legitimately use — their own (an admin may have created the subnet
	// on it, which subnetReadScope allows) or a shared/external one,
	// mirroring the port path's project scoping.
	err := svc.pool.QueryRow(ctx, `
		SELECT s.id::text, s.network_id::text, s.cidr, s.gateway_ip
		FROM subnets s
		JOIN networks n ON n.id = s.network_id
		WHERE s.id = $1
		  AND (s.project_id = $2 OR n.project_id = $2 OR n.shared = true OR n.external = true)
	`, subnetID, projectID).Scan(&subnetID, &networkID, &cidr, &gatewayIP)
	if errors.Is(err, database.ErrNoRows) {
		// Not-found for both a bogus id and another project's private subnet:
		// the port path above is project-scoped the same way, and answering
		// 404 rather than 403 keeps the resource's existence private.
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface_subnet_lookup").Str("subnet_id", subnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get subnet"))
		return
	}

	// A subnet without a gateway (an explicit null clears it, S14) used to
	// 500 here: the scan above targeted a plain string and NULL cannot land
	// in one. Upstream never refuses a usable pool — /30, /31, /32 and IPv6
	// subnets all allocate — so a NULL gateway falls back to the same
	// allocator CreatePort uses, and only an exhausted/invalid CIDR is a
	// clean 400 (SPEC-028).
	if gatewayIP == nil {
		allocated := allocateIP(cidr)
		if allocated == "" {
			common.SendError(c, common.NewBadRequestError("subnet's cidr has no usable address for a router interface"))
			return
		}
		gatewayIP = &allocated
	}

	// A second interface for the same (router, subnet) is upstream's 409 —
	// and it must be refused BEFORE anything is written: the unique
	// constraint used to turn it into a 500 that left the freshly created
	// interface port behind, which then blocked the network's deletion with
	// a 409 nobody could 'router remove port' away (the interface row had
	// never been inserted, so the remove lookup found nothing; SPEC-028).
	var alreadyAttached bool
	err = svc.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM router_interfaces WHERE router_id = $1 AND subnet_id = $2)",
		id, subnetID).Scan(&alreadyAttached)
	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface_duplicate_check").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to check router interface"))
		return
	}
	if alreadyAttached {
		common.SendError(c, common.NewResourceInUseError("router", "add interface to router",
			"Router already has an interface on subnet "+subnetID+"."))
		return
	}

	now := time.Now()
	if req.PortID == "" {
		// Create a port for the router interface
		portID = uuid.New().String()
		macAddress := generateMAC()

		fixedIPs := []map[string]string{
			{
				"subnet_id":  subnetID,
				"ip_address": *gatewayIP,
			},
		}
		fixedIPsJSON, _ := json.Marshal(fixedIPs)

		_, err = svc.pool.Exec(ctx, `
			INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, portID, "router-interface-"+id[:8], networkID, projectID, id, "network:router_interface",
			macAddress, true, "ACTIVE", fixedIPsJSON, now, now)

		if err != nil {
			log.Error().Err(err).Str("operation", "add_router_interface_port").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to create router interface port"))
			return
		}
	} else {
		// Claim the caller's port for the router — CONDITIONALLY. The
		// device_id read earlier is only a check, not a reservation: two
		// concurrent attaches of the same port to different routers both
		// pass it, and an unconditional UPDATE lets both claims win (last
		// write leaves the port owned by a router whose interface row also
		// points at it). The WHERE keeps the claim atomic in the row lock;
		// zero rows affected means someone claimed it in between. (The
		// column is UUID: an unclaimed port is NULL, never ''.)
		ct, err := svc.pool.Exec(ctx, `
			UPDATE ports SET device_id = $1, device_owner = 'network:router_interface', updated_at = $2
			WHERE id = $3 AND device_id IS NULL
		`, id, now, portID)
		if err != nil {
			log.Error().Err(err).Str("operation", "add_router_interface_port_claim").Str("port_id", portID).Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to attach port to router"))
			return
		}
		if ct.RowsAffected() == 0 {
			common.SendError(c, common.NewResourceInUseError("port", "attach port to router",
				"Port "+portID+" already has a device assigned and cannot be attached to a router."))
			return
		}
	}

	// Create router interface record
	_, err = svc.pool.Exec(ctx, `
		INSERT INTO router_interfaces (id, router_id, port_id, subnet_id, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, uuid.New().String(), id, portID, subnetID, now)

	if err != nil {
		// The record is what ties the pieces together; without it the port
		// above would be an orphan holding the network hostage. The port
		// this handler created is taken back outright; the caller's own
		// port is UNclaimed — its device_id was empty on entry (checked
		// above), so clearing the fields restores exactly the pre-call
		// state rather than leaving a router-owned port no 'router remove
		// port' can match (no interface row exists to remove). The same
		// rollback serves the unique-violation race below: it, too, has
		// already claimed the port when it loses.
		if req.PortID == "" {
			if _, derr := svc.pool.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID); derr != nil {
				log.Warn().Err(derr).Str("port_id", portID).Msg("rollback: failed to delete router interface port")
			}
		} else {
			if _, derr := svc.pool.Exec(ctx, `
				UPDATE ports SET device_id = NULL, device_owner = NULL, updated_at = $2 WHERE id = $1
			`, portID, now); derr != nil {
				log.Warn().Err(derr).Str("port_id", portID).Msg("rollback: failed to unclaim router interface port")
			}
		}
		if common.IsUniqueViolation(err) {
			// The constraint is the duplicate-interface backstop: the
			// EXISTS check above can lose a race with a concurrent attach
			// of the same (router, subnet). Answer the same 409 the check
			// produces instead of surfacing the race as a 500.
			common.SendError(c, common.NewResourceInUseError("router", "add interface to router",
				"Router already has an interface on subnet "+subnetID+"."))
			return
		}
		log.Error().Err(err).Str("operation", "add_router_interface_record").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create router interface record"))
		return
	}

	// WS-3: if this router already has an external gateway, masquerade the
	// just-attached subnet's egress out the uplink now (real mode), so egress
	// works whether the gateway was set before or after this interface.
	if svc.mode != "stub" {
		var snatOn bool
		if err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT EXISTS (SELECT 1 FROM routers rt WHERE rt.id = $1 AND "+snatOnPredicate+")", routerID).Scan(&snatOn); err == nil && snatOn {
			if uplink, uerr := networking.DefaultUplinkInterface(); uerr == nil {
				if err := svc.routerManager.EnsureEgressSNAT(cidr, uplink); err != nil {
					log.Warn().Err(err).Str("router_id", routerID).Str("cidr", cidr).Msg("WS-3: egress SNAT on interface attach failed")
				} else {
					log.Info().Str("router_id", routerID).Str("cidr", cidr).Str("uplink", uplink).Msg("WS-3: egress SNAT enabled")
				}
			} else {
				log.Warn().Err(uerr).Msg("WS-3: cannot detect uplink for egress SNAT on attach")
			}
		}
	}

	// Attach interface to router namespace
	interfaceName := "qg-" + portID[:10]
	_, ipNet, _ := net.ParseCIDR(cidr)
	maskBits, _ := ipNet.Mask.Size()
	cidrSuffix := fmt.Sprintf("%d", maskBits)

	if err := svc.routerManager.AttachInterfaceToRouter(routerID, interfaceName, *gatewayIP, cidrSuffix); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Str("interface", interfaceName).Msg("failed to attach interface to router")
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet_id": subnetID,
		"port_id":   portID,
	})
}

// RemoveRouterInterface removes an interface from a router
func (svc *Service) RemoveRouterInterface(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var req struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	if req.SubnetID == "" && req.PortID == "" {
		common.SendError(c, common.NewBadRequestError("either subnet_id or port_id must be specified"))
		return
	}

	// Authorise first, and find the interface by its row on THIS router. Nothing here used to check who
	// owns the router: the lookup, the host detach and the row delete were all unscoped, so any project
	// that knew a router id could take an interface off it — and a port id shorter than ten characters,
	// or none at all, panicked the slice that names the device.
	id, ok := svc.resolveOwnRouter(c, routerID, projectID)
	if !ok {
		return
	}

	var portID, subnetID, cidr string
	err := svc.pool.QueryRow(ctx, `
		SELECT ri.port_id::text, ri.subnet_id::text, s.cidr
		FROM router_interfaces ri
		JOIN subnets s ON s.id = ri.subnet_id
		WHERE ri.router_id = $1
		  AND ($2 = '' OR ri.subnet_id::text = $2)
		  AND ($3 = '' OR ri.port_id::text = $3)
		LIMIT 1
	`, id, req.SubnetID, req.PortID).Scan(&portID, &subnetID, &cidr)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("router interface"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "remove_router_interface_lookup").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to find router interface"))
		return
	}

	// The row first: deleting the interface's port takes its router_interfaces row with it (the
	// foreign key cascades), and the host follows.
	if _, err := svc.pool.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID); err != nil {
		log.Error().Err(err).Str("operation", "remove_router_interface_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete router interface port"))
		return
	}

	interfaceName := "qg-" + portID[:10]
	if err := svc.routerManager.DetachInterfaceFromRouter(id, interfaceName); err != nil {
		log.Warn().Err(err).Str("router_id", id).Str("interface", interfaceName).Msg("failed to detach interface from router")
	}
	// The subnet no longer leaves through this router; its range keeps its SNAT only if another
	// router still needs it.
	svc.withdrawEgressSNAT(ctx, []string{cidr})

	c.JSON(http.StatusOK, gin.H{
		"subnet_id": subnetID,
		"port_id":   portID,
	})
}

// Helper function to configure external gateway
func (svc *Service) configureExternalGateway(ctx context.Context, routerID string, gatewayInfo map[string]any) error {
	if gatewayInfo == nil {
		return nil
	}

	networkID, ok := gatewayInfo["network_id"].(string)
	if !ok {
		return fmt.Errorf("missing network_id in external_gateway_info")
	}

	enableSNAT := gatewaySNATOn(gatewayInfo)

	// Get external network details
	var externalCIDR string
	err := svc.pool.QueryRow(ctx,
		"SELECT cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		networkID,
	).Scan(&externalCIDR)

	if err != nil {
		return fmt.Errorf("failed to get external network subnet: %w", err)
	}

	// Get all internal subnets connected to this router
	rows, err := svc.pool.Query(ctx, `
		SELECT s.cidr
		FROM subnets s
		JOIN router_interfaces ri ON ri.subnet_id = s.id
		WHERE ri.router_id = $1
	`, routerID)

	if err != nil {
		return err
	}
	defer rows.Close()

	// Single-node WS-3: masquerade each internal subnet out the node's real
	// uplink in the default namespace, where the VMs and their gateway bridge
	// live (the multi-node per-router-netns SNAT never sees this traffic).
	// enable_snat=false (a transparent router) skips it.
	if enableSNAT && svc.mode != "stub" {
		uplink, uerr := networking.DefaultUplinkInterface()
		if uerr != nil {
			log.Warn().Err(uerr).Str("router_id", routerID).Msg("WS-3: cannot detect node uplink; egress SNAT not programmed")
			return nil
		}
		for rows.Next() {
			var internalCIDR string
			if err := rows.Scan(&internalCIDR); err != nil {
				log.Warn().Err(err).Str("operation", "configureExternalGateway").Msg("failed to scan row")
				continue
			}
			if err := svc.routerManager.EnsureEgressSNAT(internalCIDR, uplink); err != nil {
				log.Warn().Err(err).Str("router_id", routerID).Str("cidr", internalCIDR).Msg("WS-3: failed to enable egress SNAT")
			}
		}
		// Report a partial read: some internal CIDRs may have no egress SNAT rule.
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Str("router_id", routerID).Msg("WS-3: rows iteration error while enabling egress SNAT")
			return fmt.Errorf("failed to read router internal subnets: %w", err)
		}
	}

	return nil
}

// resolveOwnRouter returns the stored id of the caller's router named by routerID, writing the
// refusal itself — 404, as for any router the caller cannot see — so the handler only has to return.
// Router writes authorise through it before they touch anything, the host included. By id only, as
// upstream; a client resolves a name to an id before it writes.
func (svc *Service) resolveOwnRouter(c *gin.Context, routerID, projectID string) (string, bool) {
	var id string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id::text FROM routers WHERE id::text = $1 AND project_id = $2", routerID, projectID,
	).Scan(&id)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("router"))
		return "", false
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "resolve_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to look up router"))
		return "", false
	}
	return id, true
}

// snatOnPredicate holds for a router (aliased rt) whose gateway masquerades its subnets: the gateway
// names a network, and enable_snat is not turned off — absent means on, as upstream. Compared as JSON
// rather than cast, so no stored value can turn it into a query error.
const snatOnPredicate = `rt.external_gateway_info->>'network_id' IS NOT NULL
	AND COALESCE(rt.external_gateway_info->'enable_snat', 'true'::jsonb) <> 'false'::jsonb`

// egressSNATCIDRs returns the internal subnet ranges that leave through a router with SNAT on.
// reconcileNAT ensures a MASQUERADE for each; withdrawEgressSNAT keeps any range still listed. On a
// read error it returns what it read so far together with the error: a short list is safe to ensure
// from and unsafe to withdraw against.
func (svc *Service) egressSNATCIDRs(ctx context.Context) ([]string, error) {
	rows, err := svc.pool.Query(ctx, `
		SELECT DISTINCT s.cidr
		FROM routers rt
		JOIN router_interfaces ri ON ri.router_id = rt.id
		JOIN subnets s ON s.id = ri.subnet_id
		WHERE `+snatOnPredicate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cidrs []string
	for rows.Next() {
		var cidr string
		if err := rows.Scan(&cidr); err != nil {
			return cidrs, err
		}
		cidrs = append(cidrs, cidr)
	}
	return cidrs, rows.Err()
}

// egressSNATNoLongerNeeded returns those of cidrs that no router with SNAT on still needs. The
// MASQUERADE rule is per range, not per router, and two projects may well both use 10.0.0.0/24, so a
// range stays while any router still has an interface on a subnet of it.
func (svc *Service) egressSNATNoLongerNeeded(ctx context.Context, cidrs []string) ([]string, error) {
	needed, err := svc.egressSNATCIDRs(ctx)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, cidr := range cidrs {
		if !slices.Contains(needed, cidr) && !slices.Contains(stale, cidr) {
			stale = append(stale, cidr)
		}
	}
	return stale, nil
}

// withdrawEgressSNAT removes the egress rules of each of cidrs that no router still needs: after a
// gateway was cleared or stopped masquerading, or an interface went. That used never to happen — once
// installed, a subnet's egress stayed until teardown. Best-effort, and skipped when the needed set
// cannot be read in full; a range withdrawn while another path adds a router for it is restored on
// the reconciler's next pass.
func (svc *Service) withdrawEgressSNAT(ctx context.Context, cidrs []string) {
	if svc.mode == "stub" || len(cidrs) == 0 {
		return
	}
	stale, err := svc.egressSNATNoLongerNeeded(ctx, cidrs)
	if err != nil {
		log.Warn().Err(err).Msg("egress SNAT: needed ranges unreadable; nothing withdrawn")
		return
	}
	if len(stale) == 0 {
		return
	}
	uplink, err := networking.DefaultUplinkInterface()
	if err != nil {
		log.Warn().Err(err).Msg("egress SNAT: cannot detect the uplink; nothing withdrawn")
		return
	}
	for _, cidr := range stale {
		if err := svc.routerManager.RemoveEgressSNAT(cidr, uplink); err != nil {
			log.Warn().Err(err).Str("cidr", cidr).Msg("egress SNAT: failed to withdraw")
		} else {
			log.Info().Str("cidr", cidr).Msg("egress SNAT: withdrawn")
		}
	}
}

// withdrawRouterEgressSNAT withdraws the egress rules of every range the router has an interface on.
func (svc *Service) withdrawRouterEgressSNAT(ctx context.Context, routerID string) {
	if svc.mode == "stub" {
		return
	}
	// cidr is NOT NULL, so queryStrings dropping empty values removes nothing real.
	cidrs, err := queryStrings(ctx, svc.pool, `
		SELECT s.cidr FROM router_interfaces ri JOIN subnets s ON s.id = ri.subnet_id WHERE ri.router_id = $1`,
		routerID)
	if err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Msg("egress SNAT: router ranges unreadable; nothing withdrawn")
		return
	}
	svc.withdrawEgressSNAT(ctx, cidrs)
}

// requireExternalNetwork refuses a network that is not marked external, writing the refusal itself
// (SPEC-024 / cloud-storage#546 F11, FR-034).
//
// Any network was previously accepted as a gateway — configureExternalGateway only ever looked up a
// subnet on it — so a tenant could point a router at an arbitrary network and Lightstack would try to
// masquerade out of it.
//
// Applies to NEW create/update requests only. Gateways stored before migration 066 keep working and
// the reconciler keeps asserting their NAT, because the column defaults to false and an upgrade must
// not silently withdraw working guest egress (FR-035a).
//
// It takes the id rather than the gateway blob so the floating-IP caller, which has a plain string,
// does not have to wrap it in a throwaway map (branch review 2026-08-13, SRC-7).
func requireExternalNetwork(c *gin.Context, pool database.DB, networkID, use string) bool {
	if networkID == "" {
		common.SendError(c, common.NewBadRequestError("a network_id is required to name "+use))
		return false
	}
	// By id only, unlike GetNetwork, which also takes a name: upstream validates network_id and
	// floating_network_id as UUIDs (type:uuid) and answers 400, and a client resolves a name to an id
	// before it sends one. A name used to reach the lookup below and come back as a 404.
	if uuid.Validate(networkID) != nil {
		common.SendError(c, common.NewBadRequestError(fmt.Sprintf("invalid network_id for %s: %q is not a valid UUID", use, networkID)))
		return false
	}

	// Scoped to what the caller may see — owner OR shared OR external, the same visibility predicate
	// GetNetwork and ListDHCPAgentsOnNetwork use. Unscoped, this answered 400 ("not an external
	// network") for a network the caller cannot see and 404 for one that does not exist, so the pair
	// distinguished existence and made the endpoint an oracle (branch review 2026-08-13, SRC-4).
	// Nothing usable is hidden by this: a network that qualifies as a gateway is external, and external
	// is itself one of the three ways to be visible (FR-033a).
	var external bool
	err := pool.QueryRow(c.Request.Context(), `
		SELECT external FROM networks
		WHERE id::text = $1 AND (project_id = $2 OR shared = true OR external = true)`,
		networkID, c.GetString("project_id")).Scan(&external)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return false
	}
	if err != nil {
		log.Error().Err(err).Str("network_id", networkID).Msg("failed to check external network")
		common.SendError(c, common.NewInternalServerError("failed to verify external network"))
		return false
	}
	if !external {
		common.SendError(c, common.NewBadRequestError(
			"Network "+networkID+" is not an external network and cannot be used as "+use))
		return false
	}
	return true
}

// gatewayNetworkID extracts the network a router's external_gateway_info names.
//
// A missing, empty or non-string network_id is refused rather than waved through. It used to return
// "true, someone else will report it" — but nobody did: CreateRouter persists the gateway before
// calling configureExternalGateway and then swallows that function's error, so the router was
// created with 201 carrying a gateway that had never been validated, which reconcileNAT then treated
// as real forever (branch review 2026-08-13, SRC-3). Reporting 201 for a request the server did not
// honour is the defect class this feature exists to remove.
//
// enable_snat, when present, must be a boolean: it is stored as given and read back as the decision
// whether the router's subnets are masqueraded (gatewaySNATOn, egressSNATCIDRs).
func gatewayNetworkID(c *gin.Context, gatewayInfo map[string]any) (string, bool) {
	networkID, ok := gatewayInfo["network_id"].(string)
	if !ok || networkID == "" {
		common.SendError(c, common.NewBadRequestError(
			"external_gateway_info requires a non-empty string network_id"))
		return "", false
	}
	if snat, present := gatewayInfo["enable_snat"]; present {
		if _, isBool := snat.(bool); !isBool {
			common.SendError(c, common.NewBadRequestError("external_gateway_info.enable_snat must be a boolean"))
			return "", false
		}
	}
	return networkID, true
}

// routerGatewayUpdate decodes external_gateway_info from an update. given is false when the key was
// omitted; a nil gateway with given true clears it — an explicit null or an empty object, both of
// which upstream's dict_or_nodata accepts, and the latter what `openstack router unset
// --external-gateway` sends.
func routerGatewayUpdate(raw json.RawMessage) (gateway map[string]any, given bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	if err := json.Unmarshal(raw, &gateway); err != nil {
		return nil, true, err
	}
	if len(gateway) == 0 {
		return nil, true, nil
	}
	return gateway, true, nil
}

// gatewaySNATOn reports whether a gateway masquerades the router's subnets: enable_snat absent means
// on, as upstream.
func gatewaySNATOn(gateway map[string]any) bool {
	on, set := gateway["enable_snat"].(bool)
	return !set || on
}
