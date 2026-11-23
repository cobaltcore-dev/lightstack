package neutron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
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

// CreateRouter creates a router, and its gateway port when it is given one, in one transaction. The
// router is then converged on the network node before the answer; when that fails, the router is
// removed again, compare-and-set, and the request answers 500 (SPEC-033 FR-033, FR-045).
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

	// The router and its gateway port in one transaction.
	ctx := c.Request.Context()
	now := time.Now()
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO routers (id, name, project_id, admin_state_up, status, external_gateway_info, distributed, ha, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, routerID, req.Router.Name, projectID, adminStateUp, "ACTIVE", gatewayInfoJSON, distributed, ha, now, now); err != nil {
			return err
		}
		if gateway == nil {
			return nil
		}
		return setRouterGateway(ctx, tx, routerID, gateway)
	})
	if err != nil {
		sendChangeError(c, err, "create", "router")
		return
	}

	// Stored. From here the router is applied or undone, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.convergeRouterUnit(cctx, routerID); err != nil {
		if uerr := svc.undoRouterCreate(cctx, routerID); uerr != nil {
			log.Error().Err(uerr).Str("router_id", routerID).Msg("remove a router the network node could not apply; the reconciliation pass applies its record")
		}
		svc.reapplyRouter(cctx, routerID)
		log.Error().Err(err).Str("router_id", routerID).Msg("the network node could not apply the router; the create was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the router"))
		return
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

// UpdateRouter updates a router's name, admin state and gateway. A gateway set or changed is stored
// with the router, then converged on the network node before the answer, and put back,
// compare-and-set, when that fails (FR-008, FR-045). A gateway cleared is taken off the network node
// first and out of the records after, as a deletion is (FR-040). The rest changes no host state.
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

	// Authorise first, before anything reaches a node.
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
	// The update's transaction: under the router's lock, the gateway as request asks — nil clears it —
	// when given, then the row's assignments. A gateway a floating IP of the router goes through is not
	// removed or moved (FR-039); its address is checked and taken here (FR-038).
	admin := isAdmin(c)
	store := func(tx database.Tx) error {
		if err := lockRouter(ctx, tx, id, projectID, admin); err != nil {
			return err
		}
		if gatewayGiven {
			if err := setRouterGateway(ctx, tx, id, request); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "UPDATE routers SET "+u.Assignments()+" WHERE id = "+u.Arg(id), u.Args()...)
		return err
	}

	switch {
	case gatewayGiven && gateway == nil:
		if !svc.clearRouterGateway(c, id, projectID, store) {
			return
		}
	case gatewayGiven:
		if !svc.changeRouterGateway(c, id, projectID, store) {
			return
		}
	default:
		if err := database.WithTx(ctx, svc.pool, store); err != nil {
			sendChangeError(c, err, "update", "router")
			return
		}
	}
	svc.GetRouter(c)
}

// lockRouter locks a router the caller may change: their project's, any project's for an
// administrator. A router deleted since it was authorised is not found.
func lockRouter(ctx context.Context, tx database.Tx, id, projectID string, admin bool) error {
	var locked string
	err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
		id, projectID, admin).Scan(&locked)
	if errors.Is(err, database.ErrNoRows) {
		return common.NewNotFoundError("router")
	}
	return err
}

// changeRouterGateway sets or changes a router's gateway: stored, converged on the network node, and
// put back, compare-and-set, when the network node cannot apply it. It answers a failure itself and
// reports whether the change stands.
func (svc *Service) changeRouterGateway(c *gin.Context, id, projectID string, store func(database.Tx) error) bool {
	ctx := c.Request.Context()
	var before, after routerGatewayRecord
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := lockRouter(ctx, tx, id, projectID, isAdmin(c)); err != nil {
			return err
		}
		var err error
		if before, err = readRouterGatewayRecord(ctx, tx, id); err != nil {
			return err
		}
		if err := store(tx); err != nil {
			return err
		}
		after, err = readRouterGatewayRecord(ctx, tx, id)
		return err
	})
	if err != nil {
		sendChangeError(c, err, "update", "router")
		return false
	}
	// Stored. From here the change is applied or undone, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.convergeRouterUnit(cctx, id); err != nil {
		if uerr := svc.undoRouterGateway(cctx, id, before, after); uerr != nil {
			log.Error().Err(uerr).Str("router_id", id).Msg("put a router's gateway back; the reconciliation pass applies its record")
		}
		svc.reapplyRouter(cctx, id)
		log.Error().Err(err).Str("router_id", id).Msg("the network node could not apply the router's gateway; the change was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the router's gateway"))
		return false
	}
	return true
}

// clearRouterGateway clears a router's gateway in FR-040's order: refused while a floating IP goes
// through it (FR-039); taken off the network node; then out of the records, refused again if a floating
// IP arrived meanwhile. A failure after the first step converges the router from its records again. It
// answers a failure itself and reports whether the clear stands.
func (svc *Service) clearRouterGateway(c *gin.Context, id, projectID string, store func(database.Tx) error) bool {
	ctx := c.Request.Context()
	var port string
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := lockRouter(ctx, tx, id, projectID, isAdmin(c)); err != nil {
			return err
		}
		ports, err := gatewayPortsOf(ctx, tx, []string{id})
		if err != nil {
			return err
		}
		if gw, ok := ports[id]; ok {
			port = gw.id
			return requireGatewayUnused(ctx, tx, id, gw.networkID)
		}
		return nil
	})
	if err != nil {
		sendChangeError(c, err, "update", "router")
		return false
	}
	// From here the removal is applied or put back, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if port != "" {
		if err := svc.convergeRouterUnit(cctx, id, port); err != nil {
			svc.reapplyRouter(cctx, id)
			log.Error().Err(err).Str("router_id", id).Msg("the network node could not remove the router's gateway; it was kept")
			common.SendError(c, common.NewInternalServerError("failed to remove the router's gateway"))
			return false
		}
	}
	if err := database.WithTx(cctx, svc.pool, store); err != nil {
		svc.reapplyRouter(cctx, id)
		sendChangeError(c, err, "update", "router")
		return false
	}
	return true
}

// routerGatewayRecord is what a gateway change replaces: the router's external_gateway_info and its
// gateway port, whole, so an undo can put the port back as it was.
type routerGatewayRecord struct {
	info sql.NullString
	port *routerGatewayPort
}

type routerGatewayPort struct {
	id, networkID, mac string
	fixedIPs           common.FixedIPs
	created, updated   time.Time
}

func readRouterGatewayRecord(ctx context.Context, tx database.Tx, routerID string) (routerGatewayRecord, error) {
	var r routerGatewayRecord
	if err := tx.QueryRow(ctx, "SELECT external_gateway_info FROM routers WHERE id::text = $1", routerID).Scan(&r.info); err != nil {
		return r, fmt.Errorf("router %s: read its gateway: %w", routerID, err)
	}
	var p routerGatewayPort
	err := tx.QueryRow(ctx, `
		SELECT id::text, network_id::text, mac_address, fixed_ips, created_at, updated_at FROM ports
		 WHERE device_id::text = $1 AND `+gatewayPortWhere(""), routerID,
	).Scan(&p.id, &p.networkID, &p.mac, &p.fixedIPs, &p.created, &p.updated)
	if errors.Is(err, database.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("router %s: read its gateway port: %w", routerID, err)
	}
	r.port = &p
	return r, nil
}

// sameGateway reports whether two records hold the same gateway: the same network and SNAT, and the
// same port with the same addresses.
func sameGateway(routerID string, a, b routerGatewayRecord) bool {
	ga, gb := parseStoredGateway(routerID, a.info), parseStoredGateway(routerID, b.info)
	if (ga == nil) != (gb == nil) || ga != nil && (ga["network_id"] != gb["network_id"] || gatewaySNATOn(ga) != gatewaySNATOn(gb)) {
		return false
	}
	if (a.port == nil) != (b.port == nil) {
		return false
	}
	return a.port == nil || a.port.id == b.port.id && slices.Equal(a.port.fixedIPs, b.port.fixedIPs)
}

// undoRouterGateway puts a router's gateway back as it was before this request changed it, only while
// it still holds what the request left and its previous addresses are free: another change since
// stays (SPEC-033 T188).
func (svc *Service) undoRouterGateway(ctx context.Context, routerID string, before, after routerGatewayRecord) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, floatingIPAllocationLock); err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id::text = $1 FOR UPDATE", routerID).Scan(&locked); err != nil {
			if errors.Is(err, database.ErrNoRows) {
				return nil
			}
			return err
		}
		now, err := readRouterGatewayRecord(ctx, tx, routerID)
		if err != nil || !sameGateway(routerID, now, after) {
			return err
		}
		if now.port != nil {
			// A floating IP that has come to go through a gateway on another network keeps it (FR-039):
			// the later change stays (SPEC-033 T270).
			if before.port == nil || before.port.networkID != now.port.networkID {
				if err := requireGatewayUnused(ctx, tx, routerID, now.port.networkID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, "DELETE FROM ports WHERE id::text = $1", now.port.id); err != nil {
				return err
			}
		}
		if p := before.port; p != nil {
			addresses := make([]string, 0, len(p.fixedIPs))
			for _, f := range p.fixedIPs {
				addresses = append(addresses, f.IPAddress)
			}
			if err := requireAddressesFree(ctx, tx, p.networkID, addresses); err != nil {
				return err
			}
			raw, _ := json.Marshal(p.fixedIPs)
			if _, err := tx.Exec(ctx, `
				INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
				VALUES ($1, '', $2, NULL, $3, $4, $5, true, 'ACTIVE', $6, $7, $8)`,
				p.id, p.networkID, routerID, ownerRouterGateway, p.mac, raw, p.created, p.updated); err != nil {
				return err
			}
		}
		var info any
		if before.info.Valid {
			info = before.info.String
		}
		_, err = tx.Exec(ctx, "UPDATE routers SET external_gateway_info = $1 WHERE id::text = $2", info, routerID)
		return err
	})
}

// undoRouterCreate removes a router this request created, with its gateway port, only while it is as
// created: never updated since, and with no interface.
func (svc *Service) undoRouterCreate(ctx context.Context, routerID string) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM routers WHERE id::text = $1 AND updated_at = created_at
			   AND NOT EXISTS (SELECT 1 FROM router_interfaces WHERE router_id::text = $1)`, routerID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM ports WHERE device_id::text = $1 AND "+gatewayPortWhere(""), routerID)
		return err
	})
}

// reapplyRouter converges a router from its records again after a change that did not stand. What it
// cannot apply, the reconciliation pass converges.
func (svc *Service) reapplyRouter(ctx context.Context, routerID string) {
	if err := svc.convergeRouterUnit(ctx, routerID); err != nil {
		log.Error().Err(err).Str("router_id", routerID).Msg("converge a router from its records again; the reconciliation pass converges it")
	}
}

// errRouterInUse is upstream's RouterInUse as _ensure_router_not_in_use raises it: the reason names
// every port attached to the router — its interfaces' and its gateway's, the router's records, in the
// order they were made.
func errRouterInUse(ctx context.Context, q subnetQuerier, id string) error {
	ports, err := queryStrings(ctx, q, `
		SELECT id::text FROM ports
		 WHERE id IN (SELECT port_id FROM router_interfaces WHERE router_id::text = $1)
		    OR (`+gatewayPortWhere("")+` AND device_id::text = $1)
		 ORDER BY created_at, id`, id)
	if err != nil {
		return fmt.Errorf("router %s: read its ports: %w", id, err)
	}
	return common.NewNeutronError(http.StatusConflict, "RouterInUse",
		fmt.Sprintf("Router %s has ports still attached: %s", id, strings.Join(ports, ", ")))
}

// DeleteRouter deletes a router in FR-040's order: refused while it has interfaces; taken off the
// network node; then out of the records with its gateway port, refused again if an interface arrived
// meanwhile. A failure after the first step converges the router from its records again (SPEC-033
// FR-040, research R34).
func (svc *Service) DeleteRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Authorise first, before anything reaches a node.
	id, ok := svc.resolveRouter(c, routerID, projectID)
	if !ok {
		return
	}
	refuseInUse := func(tx database.Tx) error {
		if err := lockRouter(ctx, tx, id, projectID, isAdmin(c)); err != nil {
			return err
		}
		var interfaces int
		if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM router_interfaces WHERE router_id::text = $1", id).Scan(&interfaces); err != nil {
			return err
		}
		if interfaces > 0 {
			return errRouterInUse(ctx, tx, id)
		}
		return nil
	}
	if err := database.WithTx(ctx, svc.pool, refuseInUse); err != nil {
		sendChangeError(c, err, "delete", "router")
		return
	}

	// From here the removal is applied or put back, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.removeRouterUnit(cctx, id); err != nil {
		svc.reapplyRouter(cctx, id)
		log.Error().Err(err).Str("router_id", id).Msg("the network node could not remove the router; it was kept")
		common.SendError(c, common.NewInternalServerError("failed to remove the router"))
		return
	}
	err := database.WithTx(cctx, svc.pool, func(tx database.Tx) error {
		if err := refuseInUse(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(cctx, "DELETE FROM ports WHERE device_id::text = $1 AND "+gatewayPortWhere(""), id); err != nil {
			return err
		}
		_, err := tx.Exec(cctx, "DELETE FROM routers WHERE id::text = $1", id)
		return err
	})
	if err != nil {
		svc.reapplyRouter(cctx, id)
		sendChangeError(c, err, "delete", "router")
		return
	}
	c.Status(http.StatusNoContent)
}

// AddRouterInterface adds an interface to a router, on a subnet or as a port, as upstream's
// add_router_interface: its port and router_interfaces row are stored in one transaction, then the router
// is converged on the network node before the answer; when that fails, the add is undone, compare-and-set,
// and the request answers 500 (SPEC-033 FR-045, FR-055, FR-069).
func (svc *Service) AddRouterInterface(c *gin.Context) {
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
	a := interfaceAdd{router: c.Param("id"), subnet: req.SubnetID, port: req.PortID,
		caller: c.GetString("project_id"), admin: isAdmin(c)}
	ctx := c.Request.Context()
	var added routerInterface
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var err error
		added, err = addInterfaceRecords(ctx, tx, a)
		return err
	})
	if err != nil {
		sendChangeError(c, err, "add an interface to", "router")
		return
	}
	// Stored. From here the interface is applied or undone, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.convergeRouterUnit(cctx, added.router); err != nil {
		svc.undoInterfaceAdd(cctx, added)
		log.Error().Err(err).Str("router_id", added.router).Msg("the network node could not apply the interface; the add was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the router interface"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"subnet_id": added.subnets[0].id,
		"port_id":   added.port,
	})
}

// interfaceRequestRefusal is upstream's _validate_interface_info for an add: a subnet or a port, not both.
func interfaceRequestRefusal(subnetID, portID string) *common.OpenStackError {
	switch {
	case subnetID == "" && portID == "":
		return badRequest("router", "Either subnet_id or port_id must be specified")
	case subnetID != "" && portID != "":
		return badRequest("router", "Cannot specify both subnet-id and port-id")
	}
	return nil
}

// interfaceAdd is an add_router_interface request: the router, the subnet or the port it names, and its
// caller's project and role.
type interfaceAdd struct {
	router, subnet, port, caller string
	admin                        bool
}

// routerInterface is an interface as an add stored it, for its undo: the subnets it joined to the router —
// every subnet of a port added by port, the one subnet otherwise; by port, the port's earlier owner; a
// subnet that joined the network's IPv6 interface port, that port's fixed IPs before and after (SPEC-033
// T315).
type routerInterface struct {
	router, port  string
	subnets       []interfaceSubnet
	byPort        bool
	previousOwner *string
	joined        *portJoin
}

// portJoin is an IPv6 subnet's gateway added to the router's IPv6 interface port on its network: the
// port's fixed IPs before and after.
type portJoin struct{ before, after common.FixedIPs }

// addInterfaceRecords stores an interface in the caller's transaction: the router locked, then its port —
// made on the subnet with its gateway, the router's project's (FR-058), the network's IPv6 interface port
// joined by an IPv6 subnet, or the given port taken — and a router_interfaces row per subnet it joins to the
// router (SPEC-033 R31, T315).
func addInterfaceRecords(ctx context.Context, tx database.Tx, a interfaceAdd) (routerInterface, error) {
	var routerProject *string
	err := tx.QueryRow(ctx, "SELECT id::text, project_id::text FROM routers WHERE id::text = $1 AND "+
		ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE", a.router, a.caller, a.admin).Scan(&a.router, &routerProject)
	if errors.Is(err, database.ErrNoRows) {
		return routerInterface{}, common.NewNotFoundError("router")
	}
	if err != nil {
		return routerInterface{}, err
	}
	var added routerInterface
	if a.port != "" {
		added, err = takeInterfacePort(ctx, tx, a)
	} else {
		added, err = makeInterfacePort(ctx, tx, a, routerProject)
	}
	if err != nil {
		return added, err
	}
	for _, s := range added.subnets {
		if _, err := tx.Exec(ctx, "INSERT INTO router_interfaces (id, router_id, port_id, subnet_id, created_at) VALUES ($1, $2, $3, $4, $5)",
			uuid.New().String(), a.router, added.port, s.id, time.Now()); err != nil {
			return added, err
		}
	}
	return added, nil
}

// makeInterfacePort makes an interface's port on a subnet the caller can see, any project's for an
// administrator (404 otherwise), after upstream's checks (checkInterfaceSubnet), with the subnet's gateway
// as read under its lock. An IPv6 subnet joins the router's interface port on its network that holds an
// IPv6 address already, as upstream's _add_interface_by_subnet adds a prefix to it (SPEC-033 T315).
func makeInterfacePort(ctx context.Context, tx database.Tx, a interfaceAdd, routerProject *string) (routerInterface, error) {
	added := routerInterface{router: a.router, port: uuid.New().String()}
	var subnet interfaceSubnet
	var project, gateway *string
	err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.network_id::text, s.project_id::text, s.cidr, s.gateway_ip
		  FROM subnets s JOIN networks n ON s.network_id = n.id
		 WHERE s.id::text = $1 AND `+scopeOrAdmin(subnetReadScope("$2"), "$3"),
		a.subnet, a.caller, a.admin).Scan(&subnet.id, &subnet.networkID, &project, &subnet.cidr, &gateway)
	if errors.Is(err, database.ErrNoRows) {
		return added, common.NewNotFoundError("subnet")
	}
	if err != nil {
		return added, err
	}
	subnet.project, subnet.gateway = derefOr(project, ""), derefOr(gateway, "")
	added.subnets = []interfaceSubnet{subnet}
	address, err := checkInterfaceSubnet(ctx, tx, a, subnet)
	if err != nil {
		return added, err
	}
	entry := common.FixedIP{SubnetID: subnet.id, IPAddress: address.String()}
	if address.Is6() {
		if joined, err := joinIPv6InterfacePort(ctx, tx, &added, subnet.networkID, entry); joined || err != nil {
			return added, err
		}
	}
	fixed, _ := json.Marshal(common.FixedIPs{entry})
	now := time.Now()
	_, err = tx.Exec(ctx, `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true, 'ACTIVE', $8, $9, $10)`,
		added.port, "router-interface-"+a.router[:8], subnet.networkID, routerProject, a.router, ownerRouterInterface,
		generateMAC(), fixed, now, now)
	return added, err
}

// joinIPv6InterfacePort adds entry, an IPv6 subnet's gateway, to the router's interface port on the network
// that holds an IPv6 address, if there is one, and reports whether there was (_find_ipv6_router_port_by_network).
func joinIPv6InterfacePort(ctx context.Context, tx database.Tx, added *routerInterface, networkID string, entry common.FixedIP) (bool, error) {
	ports, err := routerInterfacePorts(ctx, tx, added.router)
	if err != nil {
		return false, err
	}
	for _, p := range ports {
		if p.networkID != networkID || !holdsIPv6(p.fixedIPs) {
			continue
		}
		join := &portJoin{before: p.fixedIPs, after: append(slices.Clone(p.fixedIPs), entry)}
		if err := setPortFixedIPs(ctx, tx, p.id, join.after); err != nil {
			return false, err
		}
		added.port, added.joined = p.id, join
		return true, nil
	}
	return false, nil
}

// setPortFixedIPs writes a router port's fixed IPs.
func setPortFixedIPs(ctx context.Context, tx database.Tx, portID string, fixed common.FixedIPs) error {
	raw, _ := json.Marshal(fixed)
	_, err := tx.Exec(ctx, "UPDATE ports SET fixed_ips = $1, updated_at = $2 WHERE id::text = $3", raw, time.Now(), portID)
	return err
}

// holdsIPv6 reports whether any of the fixed IPs is an IPv6 address (upstream's _port_has_ipv6_address).
func holdsIPv6(fixed common.FixedIPs) bool {
	return slices.ContainsFunc(fixed, func(f common.FixedIP) bool {
		a, err := netip.ParseAddr(f.IPAddress)
		return err == nil && a.Unmap().Is6()
	})
}

// takeInterfacePort makes a port the router's interface, as upstream's _add_interface_by_port: one the
// caller can see (404), attached to nothing (409 PortInUse), with a fixed IP, no IPv6 address when another
// port of the router on its network has one, and at most one IPv4 subnet, on subnets the router has no port
// on and none of its interfaces' overlaps (400). It keeps its project and its addresses, and joins the router
// to every subnet they are on (SPEC-033 FR-069, T315).
func takeInterfacePort(ctx context.Context, tx database.Tx, a interfaceAdd) (routerInterface, error) {
	added := routerInterface{router: a.router, byPort: true}
	var network string
	var device *string
	var fixed common.FixedIPs
	err := tx.QueryRow(ctx, "SELECT id::text, network_id::text, device_id, device_owner, fixed_ips FROM ports WHERE id::text = $1 AND "+
		ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE", a.port, a.caller, a.admin,
	).Scan(&added.port, &network, &device, &added.previousOwner, &fixed)
	if errors.Is(err, database.ErrNoRows) {
		return added, common.NewNotFoundError("port")
	}
	if err != nil {
		return added, err
	}
	if derefOr(device, "") != "" {
		return added, common.NewNeutronError(http.StatusConflict, "PortInUse", fmt.Sprintf(
			"Unable to complete operation on port %s for network %s. Port already has an attached device %s.",
			added.port, network, *device))
	}
	if len(fixed) == 0 {
		return added, badRequest("router", "Router port must have at least one fixed IP")
	}
	if err := requireOneIPv6Port(ctx, tx, a.router, added.port, network, fixed); err != nil {
		return added, err
	}
	subnets, err := interfacePortSubnets(ctx, tx, fixed)
	if err != nil {
		return added, err
	}
	for _, s := range subnets {
		if err := checkRouterSubnets(ctx, tx, a.router, s); err != nil {
			return added, err
		}
	}
	added.subnets = subnets
	_, err = tx.Exec(ctx, "UPDATE ports SET device_id = $1, device_owner = $2, updated_at = $3 WHERE id::text = $4",
		a.router, ownerRouterInterface, time.Now(), added.port)
	return added, err
}

// requireOneIPv6Port is upstream's _validate_one_router_ipv6_port_per_network: a port with an IPv6 address
// joins no router that has another port on its network with one, its gateway port included (SPEC-033
// T315). Upstream's message ends in a period of its own, so the answer ends in two, as upstream's.
func requireOneIPv6Port(ctx context.Context, tx database.Tx, routerID, portID, networkID string, fixed common.FixedIPs) error {
	if !holdsIPv6(fixed) {
		return nil
	}
	ports, err := routerInterfacePorts(ctx, tx, routerID)
	if err != nil {
		return err
	}
	gateways, err := gatewayPortsOf(ctx, tx, []string{routerID})
	if err != nil {
		return err
	}
	if g, ok := gateways[routerID]; ok {
		ports = append(ports, g)
	}
	for _, p := range ports {
		if p.id != portID && p.networkID == networkID && holdsIPv6(p.fixedIPs) {
			return badRequest("router", fmt.Sprintf("Router already contains IPv6 port %s belonging to network id %s. "+
				"Only one IPv6 port from the same network subnet can be connected to a router.", p.id, p.networkID))
		}
	}
	return nil
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
				return nil, badRequest("router", "Cannot have multiple IPv4 subnets on router port")
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// undoInterfaceAdd takes back an interface the network node could not apply, compare-and-set: under the
// router's lock, unless a floating IP has come to go through it meanwhile on any subnet it joined (FR-039),
// whose change stays. A port made for it goes; a port taken gets its earlier owner back; a port an IPv6
// subnet joined gets its fixed IPs back while they are as the join left them. The router is converged from
// the records again either way (SPEC-033 FR-008, T270, T315).
func (svc *Service) undoInterfaceAdd(ctx context.Context, added routerInterface) {
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id::text = $1 FOR UPDATE", added.router).Scan(&locked); err != nil {
			return err
		}
		for _, s := range added.subnets {
			if err := requireInterfaceUnused(ctx, tx, added.router, s); err != nil {
				return err
			}
		}
		if added.joined != nil {
			return undoPortJoin(ctx, tx, added)
		}
		if !added.byPort {
			_, err := tx.Exec(ctx, "DELETE FROM ports WHERE id::text = $1 AND device_id::text = $2 AND device_owner = $3",
				added.port, added.router, ownerRouterInterface)
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM router_interfaces WHERE port_id::text = $1 AND router_id::text = $2", added.port, added.router); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE ports SET device_id = NULL, device_owner = $1, updated_at = $2 WHERE id::text = $3 AND device_id::text = $4 AND device_owner = $5",
			added.previousOwner, time.Now(), added.port, added.router, ownerRouterInterface)
		return err
	})
	if err != nil {
		log.Error().Err(err).Str("router_id", added.router).Msg("take back an interface the network node could not apply; the reconciliation pass applies its record")
	}
	svc.reapplyRouter(ctx, added.router)
}

// undoPortJoin takes an IPv6 subnet back off the interface port it joined, if the port's fixed IPs are as the
// join left them.
func undoPortJoin(ctx context.Context, tx database.Tx, added routerInterface) error {
	var now common.FixedIPs
	if err := tx.QueryRow(ctx, "SELECT fixed_ips FROM ports WHERE id::text = $1 FOR UPDATE", added.port).Scan(&now); err != nil {
		return err
	}
	if !slices.Equal(now, added.joined.after) {
		log.Info().Str("port_id", added.port).Msg("undo: the router port changed since the join; it keeps that change")
		return nil
	}
	if err := setPortFixedIPs(ctx, tx, added.port, added.joined.before); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM router_interfaces WHERE router_id::text = $1 AND port_id::text = $2 AND subnet_id::text = $3",
		added.router, added.port, added.subnets[0].id)
	return err
}

// RemoveRouterInterface removes a router's interface in FR-040's order: refused while a floating IP of
// the router reaches a fixed IP on a subnet it removes (FR-039); taken off the network node; then out of the
// records, refused again if such a floating IP arrived meanwhile. A removal by port removes the port with
// every subnet it holds, which takes its router_interfaces rows with it; a removal by subnet removes the
// port that holds it, or, when that port holds other subnets too, takes only that subnet's fixed IP and row
// off it, as upstream's _remove_interface_by_subnet. A failure after the first step converges the router
// from its records again (SPEC-033 FR-040, research R34, T315).
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

	// Authorise first, and find the interface by its rows on THIS router.
	id, ok := svc.resolveRouter(c, routerID, projectID)
	if !ok {
		return
	}
	var removal interfaceRemoval
	find := func(tx database.Tx) error {
		if err := lockRouter(ctx, tx, id, projectID, isAdmin(c)); err != nil {
			return err
		}
		var err error
		if removal, err = findInterfaceRemoval(ctx, tx, id, req.SubnetID, req.PortID); err != nil {
			return err
		}
		for _, s := range removal.subnets {
			if err := requireInterfaceUnused(ctx, tx, id, s); err != nil {
				return err
			}
		}
		return nil
	}
	if err := database.WithTx(ctx, svc.pool, find); err != nil {
		sendChangeError(c, err, "remove an interface from", "router")
		return
	}

	// From here the removal is applied or put back, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.convergeRouterUnit(cctx, id, removal.absent()); err != nil {
		svc.reapplyRouter(cctx, id)
		log.Error().Err(err).Str("router_id", id).Msg("the network node could not remove the interface; it was kept")
		common.SendError(c, common.NewInternalServerError("failed to remove the router interface"))
		return
	}
	removed := removal
	err := database.WithTx(cctx, svc.pool, func(tx database.Tx) error {
		if err := find(tx); err != nil {
			return err
		}
		if removal.absent() != removed.absent() || removal.port != removed.port {
			return common.NewConflictError("the router's interface changed meanwhile")
		}
		if removal.partial {
			return takeSubnetOffPort(cctx, tx, id, removal.port, removal.subnets[0].id)
		}
		_, err := tx.Exec(cctx, "DELETE FROM ports WHERE id::text = $1", removal.port)
		return err
	})
	if err != nil {
		svc.reapplyRouter(cctx, id)
		sendChangeError(c, err, "remove an interface from", "router")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"subnet_id": removal.subnets[0].id,
		"port_id":   removal.port,
	})
}

// interfaceRemoval is what a remove_router_interface takes off the router: the interface port, and the
// subnets it removes, each checked for floating IPs — every subnet of the port, or, by subnet, that one,
// partial when the port keeps others.
type interfaceRemoval struct {
	port    string
	subnets []interfaceSubnet
	partial bool
}

// absent is what the network node reads as gone while the removal is applied: the subnet of a partial
// removal, off the leg that stays, or the port.
func (r interfaceRemoval) absent() string {
	if r.partial {
		return r.subnets[0].id
	}
	return r.port
}

// findInterfaceRemoval finds the interface a removal names among the router's router_interfaces rows: by
// port, all of the port's, the subnet when also named among them, as upstream's _remove_interface_by_port;
// by subnet, the port holding it (_remove_interface_by_subnet). None is 404.
func findInterfaceRemoval(ctx context.Context, tx database.Tx, routerID, subnetID, portID string) (interfaceRemoval, error) {
	type row struct {
		port   string
		subnet interfaceSubnet
	}
	var rows []row
	q, err := tx.Query(ctx, `
		SELECT ri.port_id::text, ri.subnet_id::text, s.network_id::text, s.cidr
		  FROM router_interfaces ri JOIN subnets s ON s.id = ri.subnet_id
		 WHERE ri.router_id::text = $1
		 ORDER BY ri.created_at, ri.id`, routerID)
	if err != nil {
		return interfaceRemoval{}, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
	}
	for q.Next() {
		var r row
		if err := q.Scan(&r.port, &r.subnet.id, &r.subnet.networkID, &r.subnet.cidr); err != nil {
			q.Close()
			return interfaceRemoval{}, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
		}
		rows = append(rows, r)
	}
	q.Close()
	if err := q.Err(); err != nil {
		return interfaceRemoval{}, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
	}
	notFound := common.NewNotFoundError("router interface")
	if portID == "" {
		i := slices.IndexFunc(rows, func(r row) bool { return r.subnet.id == subnetID })
		if i < 0 {
			return interfaceRemoval{}, notFound
		}
		held := 0
		for _, r := range rows {
			if r.port == rows[i].port {
				held++
			}
		}
		return interfaceRemoval{port: rows[i].port, subnets: []interfaceSubnet{rows[i].subnet}, partial: held > 1}, nil
	}
	removal := interfaceRemoval{port: portID}
	named := subnetID == ""
	for _, r := range rows {
		if r.port == portID {
			removal.subnets = append(removal.subnets, r.subnet)
			named = named || r.subnet.id == subnetID
		}
	}
	if len(removal.subnets) == 0 || !named {
		return interfaceRemoval{}, notFound
	}
	return removal, nil
}

// takeSubnetOffPort takes a subnet's fixed IP and router_interfaces row off a router port that keeps others.
func takeSubnetOffPort(ctx context.Context, tx database.Tx, routerID, portID, subnetID string) error {
	var fixed common.FixedIPs
	if err := tx.QueryRow(ctx, "SELECT fixed_ips FROM ports WHERE id::text = $1 FOR UPDATE", portID).Scan(&fixed); err != nil {
		return fmt.Errorf("read router port %s: %w", portID, err)
	}
	kept := slices.DeleteFunc(slices.Clone(fixed), func(f common.FixedIP) bool { return f.SubnetID == subnetID })
	if err := setPortFixedIPs(ctx, tx, portID, kept); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM router_interfaces WHERE router_id::text = $1 AND port_id::text = $2 AND subnet_id::text = $3",
		routerID, portID, subnetID)
	return err
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
	err := externalNetworkRefusal(c.Request.Context(), pool, networkID, use, scopeOf(c))
	if err == nil {
		return true
	}
	sendChangeError(c, err, "verify", "external network")
	return false
}

// errNotExternal refuses a network that is not external where one must be.
func errNotExternal(networkID, use string) *common.OpenStackError {
	return common.NewBadRequestError("Network " + networkID + " is not an external network and cannot be used as " + use)
}

// externalNetworkRefusal is requireExternalNetwork's check on a querier: nil when networkID names an
// external network the caller can see, the *common.OpenStackError to answer with when it does not, or
// a database error. An administrator sees every network (SPEC-033 FR-057), so another project's
// internal network is "not external" to them rather than unknown, as upstream's. forUpdate locks the
// network's row, for a transaction that relies on it staying external until it commits (SPEC-033
// research R20).
func externalNetworkRefusal(ctx context.Context, q subnetQuerier, networkID, use string, caller callerScope) error {
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
	var external bool
	err := q.QueryRow(ctx, query, networkID, caller.project, caller.admin).Scan(&external)
	if errors.Is(err, database.ErrNoRows) {
		return common.NewNotFoundError("network")
	}
	if err != nil {
		return fmt.Errorf("check external network %s: %w", networkID, err)
	}
	if !external {
		return errNotExternal(networkID, use)
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
