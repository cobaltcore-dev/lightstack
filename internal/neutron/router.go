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
		// The project the router belongs to: an administrator's to name (SPEC-033 FR-058).
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
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

// ListRouters lists the caller's project's routers, every project's for an administrator, narrowed
// by project_id/tenant_id (SPEC-033 FR-057).
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
	projectScope(c, f, "project_id", ownedBy("project_id"))

	// Marker paging over a TOTAL order. created_at alone is not unique, so the order was undefined
	// among ties AND `created_at < $n` excluded every row sharing the marker's timestamp rather than
	// only those before it — a page walk silently dropped them. id is the primary key, so
	// (created_at, id) is total (as #549 established for compute). The marker resolves under the
	// listing's own scope: an unresolvable marker would silently drop the condition and make the
	// walk restart from the first page forever.
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at, id FROM routers WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
			marker, projectID, isAdmin(c),
		).Scan(&markerCreatedAt, &markerID)
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

	var found []Router
	for rows.Next() {
		var r Router
		var gatewayInfo sql.NullString

		if err := rows.Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
			&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}
		r.ExternalGatewayInfo = parseStoredGateway(r.ID, gatewayInfo)
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	// The gateway ports, read after the rows are closed, in one query (SPEC-033 FR-038).
	ids := make([]string, 0, len(found))
	for _, r := range found {
		ids = append(ids, r.ID)
	}
	ports, err := gatewayPortsOf(c.Request.Context(), svc.pool, ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_routers").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list routers"))
		return
	}
	routers := []gin.H{}
	for _, r := range found {
		var port *gatewayPort
		if p, ok := ports[r.ID]; ok {
			port = &p
		}
		routers = append(routers, renderRouter(r, port))
	}

	c.JSON(http.StatusOK, gin.H{"routers": routers})
}

// parseStoredGateway reads routers.external_gateway_info. A malformed value is treated as unset — the
// response then reports no gateway, the right behaviour for a corrupt row.
func parseStoredGateway(routerID string, raw sql.NullString) map[string]any {
	if !raw.Valid {
		return nil
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw.String), &stored); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Msg("failed to unmarshal external_gateway_info; treating as unset")
		return nil
	}
	return stored
}

// renderRouter is a router as upstream reports it, its gateway's addresses read from its port.
func renderRouter(r Router, port *gatewayPort) gin.H {
	gateway := renderGatewayInfo(r.ExternalGatewayInfo, port)
	return gin.H{
		"id":                    r.ID,
		"name":                  r.Name,
		"tenant_id":             r.ProjectID,
		"project_id":            r.ProjectID,
		"admin_state_up":        r.AdminStateUp,
		"status":                r.Status,
		"external_gateway_info": gateway,
		"distributed":           r.Distributed,
		"ha":                    r.HA,
		"created_at":            r.CreatedAt.Format(time.RFC3339),
		"updated_at":            r.UpdatedAt.Format(time.RFC3339),
	}
}

// readRouterView reads a router by id or name, with its gateway port, for a response: by id a router
// of projectID, or of any project for an administrator (SPEC-033 FR-057); by name one of projectID.
func (svc *Service) readRouterView(ctx context.Context, routerID, projectID string, admin bool) (gin.H, error) {
	var r Router
	var gatewayInfo sql.NullString
	// id::text comparison resolves by UUID or name without crashing on a non-UUID identifier — the CLI
	// looks a router up by name this way (consistent with GetNetwork/GetSubnet).
	if err := svc.pool.QueryRow(ctx, `
		SELECT id, name, project_id, admin_state_up, status, external_gateway_info,
		       distributed, ha, created_at, updated_at
		FROM routers
		WHERE ((id::text = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`) OR (name = $1 AND project_id = $2))
		LIMIT 1
	`, routerID, projectID, admin).Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
		&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.ExternalGatewayInfo = parseStoredGateway(r.ID, gatewayInfo)
	ports, err := gatewayPortsOf(ctx, svc.pool, []string{r.ID})
	if err != nil {
		return nil, err
	}
	var port *gatewayPort
	if p, ok := ports[r.ID]; ok {
		port = &p
	}
	return renderRouter(r, port), nil
}

// CreateRouter creates a new L3 router
func (svc *Service) CreateRouter(c *gin.Context) {
	var req CreateRouterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID, ok := svc.ownerOf(c, req.Router.ProjectID, req.Router.TenantID)
	if !ok {
		return
	}
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

	// The gateway: an external network, and the addresses an administrator may request (FR-038).
	var gatewayInfoJSON []byte
	var gateway *gatewayRequest
	if req.Router.ExternalGatewayInfo != nil {
		networkID, ok := gatewayNetworkID(c, req.Router.ExternalGatewayInfo)
		if !ok || !requireExternalNetwork(c, svc.pool, networkID, "a router gateway") {
			return
		}
		parsed, refusal := parseGatewayRequest(req.Router.ExternalGatewayInfo, common.HasRole(c, common.RoleAdmin), "create_router")
		if refusal != nil {
			common.SendError(c, refusal)
			return
		}
		gateway = &parsed
		gatewayInfoJSON, _ = json.Marshal(parsed.stored())
	}

	// Create router namespace
	if err := svc.routerManager.CreateRouterNamespace(routerID); err != nil {
		log.Error().Err(err).Str("operation", "create_router_namespace").Str("router_id", routerID).Msg("failed to create router namespace")
		common.SendError(c, common.NewInternalServerError("failed to create router namespace"))
		return
	}

	// Insert into database: the router and its gateway port in one transaction.
	now := time.Now()
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		if _, err := tx.Exec(c.Request.Context(), `
			INSERT INTO routers (id, name, project_id, admin_state_up, status, external_gateway_info, distributed, ha, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, routerID, req.Router.Name, projectID, adminStateUp, "ACTIVE", gatewayInfoJSON, distributed, ha, now, now); err != nil {
			return err
		}
		if gateway == nil {
			return nil
		}
		return setRouterGateway(c.Request.Context(), tx, routerID, gateway)
	})

	if err != nil {
		// Cleanup on error (best-effort; the DB error is the user-facing one)
		if cleanupErr := svc.routerManager.DeleteRouterNamespace(routerID); cleanupErr != nil {
			log.Warn().Err(cleanupErr).Str("router_id", routerID).Msg("rollback: failed to delete router namespace after DB insert failure")
		}
		var refusal *common.OpenStackError
		if errors.As(err, &refusal) {
			common.SendError(c, refusal)
			return
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

	view, err := svc.readRouterView(c.Request.Context(), routerID, projectID, false)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read the created router"))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"router": view})
}

// GetRouter retrieves a single router
func (svc *Service) GetRouter(c *gin.Context) {
	routerID := c.Param("id")
	view, err := svc.readRouterView(c.Request.Context(), routerID, c.GetString("project_id"), isAdmin(c))
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get router"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"router": view})
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
	id, ok := svc.resolveRouter(c, routerID, projectID)
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

	var request *gatewayRequest
	if gatewayGiven {
		if gateway == nil {
			u.Set("external_gateway_info", nil)
		} else {
			networkID, ok := gatewayNetworkID(c, gateway)
			if !ok || !requireExternalNetwork(c, svc.pool, networkID, "a router gateway") {
				return
			}
			parsed, refusal := parseGatewayRequest(gateway, common.HasRole(c, common.RoleAdmin), "update_router")
			if refusal != nil {
				common.SendError(c, refusal)
				return
			}
			request = &parsed
			gatewayInfoJSON, _ := json.Marshal(parsed.stored())
			u.Set("external_gateway_info", gatewayInfoJSON)
		}
	}

	if u.Empty() {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	u.Set("updated_at", time.Now())

	// The row and its gateway port in one transaction, under the router's lock: a gateway a floating IP
	// goes through is not removed or moved (FR-039), and a gateway's address is checked and taken there
	// (FR-038).
	err = database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
			id, projectID, isAdmin(c)).Scan(&locked)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("router") // deleted since it was authorised
		}
		if err != nil {
			return err
		}
		if gatewayGiven {
			if err := setRouterGateway(ctx, tx, id, request); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE routers SET "+u.Assignments()+" WHERE id = "+u.Arg(id), u.Args()...)
		return err
	})
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_router").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update router"))
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
	id, ok := svc.resolveRouter(c, routerID, projectID)
	if !ok {
		return
	}

	// The row, its gateway port with it (SPEC-033 FR-038), in one transaction that also counts the
	// interfaces, under the router's lock.
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
			id, projectID, isAdmin(c)).Scan(&locked)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("router")
		}
		if err != nil {
			return err
		}
		var interfaceCount int
		if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM router_interfaces WHERE router_id = $1", id).Scan(&interfaceCount); err != nil {
			return err
		}
		if interfaceCount > 0 {
			return common.NewResourceInUseError("router", "delete router",
				fmt.Sprintf("router %s still has %d interface(s) attached", id, interfaceCount))
		}
		if _, err := tx.Exec(ctx, "DELETE FROM ports WHERE device_id = $1 AND device_owner = $2", id, ownerRouterGateway); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM routers WHERE id = $1", id)
		return err
	})
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_router").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete router"))
		return
	}

	// The host follows the row. A router with no interfaces masquerades nothing, so there is no egress
	// SNAT of its own to withdraw.
	if err := svc.routerManager.DeleteRouterNamespace(id); err != nil {
		log.Warn().Err(err).Str("router_id", id).Msg("failed to delete router namespace")
	}

	c.Status(http.StatusNoContent)
}

// AddRouterInterface adds an interface to a router: on a subnet, its port made with the subnet's gateway,
// or with a port the caller names, which keeps its address — upstream's _add_interface_by_subnet and
// _add_interface_by_port.
func (svc *Service) AddRouterInterface(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	// The handler once ignored port_id and read the empty subnet_id, so 'router add port' answered 500
	// (SPEC-031).
	if refusal := interfaceRequestRefusal(req.SubnetID, req.PortID); refusal != nil {
		common.SendError(c, refusal)
		return
	}

	// One transaction checks and stores the interface's port and its router_interfaces row, so a router
	// never has half an interface (SPEC-033 FR-045, research R31): a port made or taken goes back with a
	// failed insert, rather than staying behind to block its network's deletion (SPEC-031). By subnet
	// the port is made (makeInterfacePort), by port it is taken (takeInterfacePort).
	ctx := c.Request.Context()
	admin := isAdmin(c)
	var portID, address string
	var subnet interfaceSubnet
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var routerProject *string
		err := tx.QueryRow(ctx, "SELECT id::text, project_id::text FROM routers WHERE id::text = $1 AND "+
			ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
			routerID, projectID, admin).Scan(&routerID, &routerProject)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("router")
		}
		if err != nil {
			return err
		}
		if req.PortID != "" {
			portID, subnet, address, err = takeInterfacePort(ctx, tx, routerID, req.PortID, projectID, admin)
		} else {
			portID, subnet, err = makeInterfacePort(ctx, tx, routerID, routerProject, req.SubnetID, projectID, admin)
			address = subnet.gateway
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO router_interfaces (id, router_id, port_id, subnet_id, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, uuid.New().String(), routerID, portID, subnet.id, time.Now())
		return err
	})
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to add the router interface"))
		return
	}
	cidr := subnet.cidr

	// WS-3: if this router already has an external gateway, masquerade the
	// just-attached subnet's egress out the uplink now (real mode), so egress
	// works whether the gateway was set before or after this interface.
	if svc.mode != "stub" {
		var snatOn bool
		if err := svc.pool.QueryRow(ctx,
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

	if err := svc.routerManager.AttachInterfaceToRouter(routerID, interfaceName, address, cidrSuffix); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Str("interface", interfaceName).Msg("failed to attach interface to router")
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet_id": subnet.id,
		"port_id":   portID,
	})
}

// interfaceRequestRefusal is upstream's _validate_interface_info for an add: a subnet or a port, not both.
func interfaceRequestRefusal(subnetID, portID string) *common.OpenStackError {
	switch {
	case subnetID == "" && portID == "":
		return common.NewBadRequestError("Bad router request: Either subnet_id or port_id must be specified.")
	case subnetID != "" && portID != "":
		return common.NewBadRequestError("Bad router request: Cannot specify both subnet-id and port-id.")
	}
	return nil
}

// makeInterfacePort makes an interface's port on a subnet the caller can see, any project's for an
// administrator (404 otherwise), after upstream's refusals (checkInterfaceSubnet, SPEC-033 FR-055). It holds
// the subnet's gateway and is the router's project's, as upstream's (SPEC-033 FR-058).
func makeInterfacePort(ctx context.Context, tx database.Tx, routerID string, routerProject *string, subnetRef, caller string, admin bool) (string, interfaceSubnet, error) {
	var s interfaceSubnet
	var project, gateway *string
	err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.network_id::text, s.project_id::text, s.cidr, s.gateway_ip
		  FROM subnets s JOIN networks n ON s.network_id = n.id
		 WHERE s.id::text = $1 AND `+scopeOrAdmin(subnetReadScope("$2"), "$3"),
		subnetRef, caller, admin).Scan(&s.id, &s.networkID, &project, &s.cidr, &gateway)
	if errors.Is(err, database.ErrNoRows) {
		return "", s, common.NewNotFoundError("subnet")
	}
	if err != nil {
		return "", s, err
	}
	s.project, s.gateway = derefOr(project, ""), derefOr(gateway, "")
	if err := checkInterfaceSubnet(ctx, tx, routerID, s, caller, admin); err != nil {
		return "", s, err
	}
	portID := uuid.New().String()
	fixedIPsJSON, _ := json.Marshal(common.FixedIPs{{SubnetID: s.id, IPAddress: s.gateway}})
	now := time.Now()
	_, err = tx.Exec(ctx, `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, portID, "router-interface-"+routerID[:8], s.networkID, routerProject, routerID, ownerRouterInterface,
		generateMAC(), true, "ACTIVE", fixedIPsJSON, now, now)
	return portID, s, err
}

// takeInterfacePort makes a port the router's interface, as upstream's _add_interface_by_port: one the
// caller can see, any project's for an administrator (404), attached to nothing (409 PortInUse), with a
// fixed IP and at most one IPv4 subnet, on a subnet the router has no port on and none of its interfaces'
// overlaps (400). It keeps its project and its address, the first fixed IP's being the interface's.
func takeInterfacePort(ctx context.Context, tx database.Tx, routerID, portRef, caller string, admin bool) (string, interfaceSubnet, string, error) {
	var portID, networkID string
	var device *string
	var fixed common.FixedIPs
	err := tx.QueryRow(ctx, "SELECT id::text, network_id::text, device_id, fixed_ips FROM ports WHERE id::text = $1 AND "+
		ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE", portRef, caller, admin,
	).Scan(&portID, &networkID, &device, &fixed)
	if errors.Is(err, database.ErrNoRows) {
		return "", interfaceSubnet{}, "", common.NewNotFoundError("port")
	}
	if err != nil {
		return "", interfaceSubnet{}, "", err
	}
	if derefOr(device, "") != "" {
		return "", interfaceSubnet{}, "", common.NewNeutronError(http.StatusConflict, "PortInUse", fmt.Sprintf(
			"Unable to complete operation on port %s for network %s. Port already has an attached device %s.",
			portID, networkID, *device))
	}
	if len(fixed) == 0 {
		return "", interfaceSubnet{}, "", common.NewBadRequestError("Bad router request: Router port must have at least one fixed IP.")
	}
	subnets, err := interfacePortSubnets(ctx, tx, fixed)
	if err != nil {
		return "", interfaceSubnet{}, "", err
	}
	for _, s := range subnets {
		if err := checkRouterSubnets(ctx, tx, routerID, s); err != nil {
			return "", interfaceSubnet{}, "", err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE ports SET device_id = $1, device_owner = $2, updated_at = $3 WHERE id::text = $4",
		routerID, ownerRouterInterface, time.Now(), portID)
	return portID, subnets[0], fixed[0].IPAddress, err
}

// interfacePortSubnets reads the subnets of a port's fixed IPs, refusing a second IPv4 one as upstream's
// _validate_router_port_info does.
func interfacePortSubnets(ctx context.Context, tx database.Tx, fixed common.FixedIPs) ([]interfaceSubnet, error) {
	var out []interfaceSubnet
	v4 := 0
	for _, f := range fixed {
		var s interfaceSubnet
		if err := tx.QueryRow(ctx, "SELECT id::text, network_id::text, cidr FROM subnets WHERE id::text = $1", f.SubnetID).
			Scan(&s.id, &s.networkID, &s.cidr); err != nil {
			return nil, fmt.Errorf("read subnet %s of the port: %w", f.SubnetID, err)
		}
		if prefix, err := parsePrefix(s.cidr); err == nil && prefix.Addr().Is4() {
			if v4++; v4 > 1 {
				return nil, common.NewBadRequestError("Bad router request: Cannot have multiple IPv4 subnets on router port.")
			}
		}
		out = append(out, s)
	}
	return out, nil
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
	id, ok := svc.resolveRouter(c, routerID, projectID)
	if !ok {
		return
	}

	// One transaction under the router's lock finds the interface, refuses while a floating IP of the
	// router reaches a fixed IP on its subnet (FR-039), and deletes the interface's port, which takes its
	// router_interfaces row with it (the foreign key cascades). The host follows.
	var portID, subnetID, cidr string
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id = $1 FOR UPDATE", id).Scan(&locked); err != nil {
			if errors.Is(err, database.ErrNoRows) {
				return common.NewNotFoundError("router")
			}
			return err
		}
		err := tx.QueryRow(ctx, `
			SELECT ri.port_id::text, ri.subnet_id::text, s.cidr
			FROM router_interfaces ri
			JOIN subnets s ON s.id = ri.subnet_id
			WHERE ri.router_id = $1
			  AND ($2 = '' OR ri.subnet_id::text = $2)
			  AND ($3 = '' OR ri.port_id::text = $3)
			LIMIT 1
		`, id, req.SubnetID, req.PortID).Scan(&portID, &subnetID, &cidr)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("router interface")
		}
		if err != nil {
			return err
		}
		if err := requireInterfaceUnused(ctx, tx, id, subnetID, cidr); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID)
		return err
	})
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "remove_router_interface").Str("router_id", id).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to remove the router interface"))
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

// resolveRouter returns the stored id of the router routerID names, if the caller may write it — their
// own project's, any project's for an administrator (SPEC-033 FR-058) — writing the refusal itself:
// 404, as for any router the caller cannot see, so the handler only has to return. Router writes
// authorise through it before they touch anything, the host included. By id only, as upstream; a
// client resolves a name to an id before it writes.
func (svc *Service) resolveRouter(c *gin.Context, routerID, projectID string) (string, bool) {
	var id string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id::text FROM routers WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		routerID, projectID, isAdmin(c),
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
	err := externalNetworkRefusal(c.Request.Context(), pool, networkID, c.GetString("project_id"), use, isAdmin(c), false)
	var refusal *common.OpenStackError
	switch {
	case err == nil:
		return true
	case errors.As(err, &refusal):
		common.SendError(c, refusal)
	default:
		log.Error().Err(err).Str("network_id", networkID).Msg("failed to check external network")
		common.SendError(c, common.NewInternalServerError("failed to verify external network"))
	}
	return false
}

// externalNetworkRefusal is requireExternalNetwork's check on a querier: nil when networkID names an
// external network the caller can see, the *common.OpenStackError to answer with when it does not, or
// a database error. An administrator sees every network (SPEC-033 FR-057), so another project's
// internal network is "not external" to them rather than unknown, as upstream's. forUpdate locks the
// network's row, for a transaction that relies on it staying external until it commits (SPEC-033
// research R20).
func externalNetworkRefusal(ctx context.Context, q subnetQuerier, networkID, projectID, use string, admin, forUpdate bool) error {
	if refusal := networkIDRefusal(networkID, use); refusal != nil {
		return refusal
	}

	// Scoped to what the caller may see — owner OR shared OR external, the same visibility predicate
	// GetNetwork and ListDHCPAgentsOnNetwork use. Unscoped, this answered 400 ("not an external
	// network") for a network the caller cannot see and 404 for one that does not exist, so the pair
	// distinguished existence and made the endpoint an oracle (branch review 2026-08-13, SRC-4).
	// Nothing usable is hidden by this: a network that qualifies as a gateway is external, and external
	// is itself one of the three ways to be visible (FR-033a).
	query := `SELECT external FROM networks
		WHERE id::text = $1 AND ` + scopeOrAdmin(networkReadScope("$2"), "$3")
	if forUpdate {
		query += " FOR UPDATE"
	}
	var external bool
	err := q.QueryRow(ctx, query, networkID, projectID, admin).Scan(&external)
	if errors.Is(err, database.ErrNoRows) {
		return common.NewNotFoundError("network")
	}
	if err != nil {
		return fmt.Errorf("check external network %s: %w", networkID, err)
	}
	if !external {
		return common.NewBadRequestError("Network " + networkID + " is not an external network and cannot be used as " + use)
	}
	return nil
}

// networkIDRefusal checks a network id from a request body before anything is looked up: present,
// and a UUID. By id only, unlike GetNetwork, which also takes a name: upstream validates network_id and
// floating_network_id as UUIDs (type:uuid) and answers 400, and a client resolves a name to an id
// before it sends one. A name used to reach the lookup and come back as a 404.
func networkIDRefusal(networkID, use string) *common.OpenStackError {
	if networkID == "" {
		return common.NewBadRequestError("a network_id is required to name " + use)
	}
	if uuid.Validate(networkID) != nil {
		return common.NewBadRequestError(fmt.Sprintf("invalid network_id for %s: %q is not a valid UUID", use, networkID))
	}
	return nil
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
