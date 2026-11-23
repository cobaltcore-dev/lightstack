package neutron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreateFloatingIPRequest represents a floating IP creation request
type CreateFloatingIPRequest struct {
	FloatingIP struct {
		FloatingNetworkID string `json:"floating_network_id" binding:"required"`
		PortID            string `json:"port_id"`
		FixedIPAddress    string `json:"fixed_ip_address"`
		SubnetID          string `json:"subnet_id"`
		FloatingIPAddress string `json:"floating_ip_address"`
		Description       string `json:"description"`
		// The project the floating IP belongs to: an administrator's to name (SPEC-033 FR-058).
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
	} `json:"floatingip"`
}

// UpdateFloatingIPRequest represents a floating IP update request
type UpdateFloatingIPRequest struct {
	FloatingIP struct {
		PortID         *string `json:"port_id"`
		FixedIPAddress *string `json:"fixed_ip_address"`
		Description    *string `json:"description"`
	} `json:"floatingip"`
}

// ListFloatingIPs lists the caller's project's floating IPs, every project's for an administrator,
// narrowed by project_id/tenant_id (SPEC-033 FR-057).
func (svc *Service) ListFloatingIPs(c *gin.Context) {
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

	// Standard Neutron list filters (neutron-lib FloatingIPDbMixin):
	// floating_network_id, port_id, router_id, status, fixed_ip_address,
	// floating_ip_address, description. All exact matches. They were silently
	// ignored before, so `openstack floating ip list --network <net>` returned
	// every tenant's addresses (SPEC-031). The slice keeps the WHERE layout
	// deterministic.
	for _, flt := range []struct{ param, column string }{
		{"floating_network_id", "floating_network_id"},
		{"port_id", "port_id"},
		{"router_id", "router_id"},
		{"status", "status"},
		{"fixed_ip_address", "fixed_ip_address"},
		{"floating_ip_address", "floating_ip_address"},
		{"description", "description"},
	} {
		v := c.Query(flt.param)
		if v == "" {
			continue
		}
		switch flt.column {
		case "floating_network_id", "port_id", "router_id":
			// UUID columns: a non-UUID value can only match nothing, but it
			// would fail the cast and answer 500 — answer the empty list a
			// well-formed-but-unknown value gets. The comparison itself is
			// uuid-typed (not ::text), so an upper-case filter value matches
			// like any client would expect.
			if _, err := uuid.Parse(v); err != nil {
				c.JSON(http.StatusOK, gin.H{"floatingips": []gin.H{}})
				return
			}
			f.Eq(flt.column, v)
		default:
			f.Eq(flt.column, v)
		}
	}

	// Marker paging over a TOTAL order. created_at alone is not unique, so the order was undefined
	// among ties AND `created_at < $n` excluded every row sharing the marker's timestamp rather than
	// only those before it — a page walk silently dropped them. id is the primary key, so
	// (created_at, id) is total (as #549 established for compute). The marker resolves under the
	// listing's own scope — an administrator's spans every project: an unresolvable marker would
	// silently drop the condition and make the walk restart from the first page forever.
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at, id FROM floating_ips WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
			marker, projectID, isAdmin(c),
		).Scan(&markerCreatedAt, &markerID)
		if err == nil {
			f.Cond(fmt.Sprintf("(created_at, id) < (%s, %s)", f.Arg(markerCreatedAt), f.Arg(markerID)))
		}
	}

	where := f.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, project_id, floating_network_id, floating_ip_address,
		       fixed_ip_address, port_id, router_id, status, description,
		       created_at, updated_at
		FROM floating_ips`+where+`
		ORDER BY created_at DESC, id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_floatingips").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list floating IPs"))
		return
	}
	defer rows.Close()

	var floatingIPs []gin.H
	for rows.Next() {
		var fip FloatingIP
		var fixedIP, portID, routerID, description sql.NullString

		if err := rows.Scan(&fip.ID, &fip.ProjectID, &fip.FloatingNetworkID, &fip.FloatingIPAddress,
			&fixedIP, &portID, &routerID, &fip.Status, &description, &fip.CreatedAt, &fip.UpdatedAt); err != nil {
			continue
		}

		result := gin.H{
			"id":                  fip.ID,
			"tenant_id":           fip.ProjectID,
			"project_id":          fip.ProjectID,
			"floating_network_id": fip.FloatingNetworkID,
			"floating_ip_address": fip.FloatingIPAddress,
			"status":              fip.Status,
			"created_at":          fip.CreatedAt.Format(time.RFC3339),
			"updated_at":          fip.UpdatedAt.Format(time.RFC3339),
		}

		if fixedIP.Valid {
			result["fixed_ip_address"] = fixedIP.String
		} else {
			result["fixed_ip_address"] = nil
		}

		if portID.Valid {
			result["port_id"] = portID.String
		} else {
			result["port_id"] = nil
		}

		if routerID.Valid {
			result["router_id"] = routerID.String
		} else {
			result["router_id"] = nil
		}

		if description.Valid {
			result["description"] = description.String
		} else {
			result["description"] = ""
		}

		floatingIPs = append(floatingIPs, result)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if floatingIPs == nil {
		floatingIPs = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"floatingips": floatingIPs})
}

// CreateFloatingIP allocates a new floating IP. With a port, the record is stored first and then
// converged before the answer: its router on the network node, its fixed IP's filter here. When that
// fails, the record is removed again, compare-and-set, and the request answers 500 (SPEC-033 FR-008a,
// research R20).
func (svc *Service) CreateFloatingIP(c *gin.Context) {
	var req CreateFloatingIPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	ctx := c.Request.Context()
	caller, admin := c.GetString("project_id"), isAdmin(c)
	floatingIPID := uuid.New().String()
	networkID := req.FloatingIP.FloatingNetworkID
	// The body's own checks, before anything is read.
	if refusal := networkIDRefusal(networkID, "a floating-IP pool"); refusal != nil {
		common.SendError(c, refusal)
		return
	}
	// The floating IP's project: the one an administrator names, or the caller's. An administrator may
	// also associate it with another project's port, as upstream's _internal_fip_assoc_data lets them
	// (SPEC-033 FR-058).
	projectID, ok := svc.ownerOf(c, req.FloatingIP.ProjectID, req.FloatingIP.TenantID)
	if !ok {
		return
	}
	// Choosing the address is an administrator's or the project manager's, as upstream's
	// create_floatingip:floating_ip_address (FR-054). A non-administrator's floating IP is their own
	// project's, which their token's roles are held on. null, as absent, asks for none.
	if req.FloatingIP.FloatingIPAddress != "" && !admin && !common.HasRole(c, common.RoleManager) {
		common.SendError(c, common.NewNeutronError(http.StatusForbidden, "PolicyNotAuthorized",
			"(rule:create_floatingip:floating_ip_address) is disallowed by policy"))
		return
	}

	if req.FloatingIP.FixedIPAddress != "" && req.FloatingIP.PortID == "" {
		common.SendError(c, errFixedIPWithoutPort())
		return
	}

	// Determine status and associated details
	status := "DOWN"
	var fixedIP, portID, routerID *string
	var floatingIP string
	now := time.Now()

	// One transaction makes the checks, chooses the address and stores the row; nothing is programmed
	// before it commits (constitution v1.5.0, limit 2; research R20, R36). Its locks, in order: the
	// allocation lock, which every floating-IP create takes first (FR-027); the port, which serialises
	// two associations of one fixed IP (FR-029); the router, as a gateway change takes it before its
	// subnets; the subnets the address is chosen from (FR-032); and last the floating network, which must
	// stay external until the row is in. The network comes last because every port or gateway insert on
	// it holds a subnet's lock and then a key-share lock on the network through its foreign key, which a
	// FOR UPDATE taken before the subnets would deadlock with. It is checked first, unlocked, so its
	// refusal still comes before any other.
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, floatingIPAllocationLock); err != nil {
			return err
		}
		if err := externalNetworkRefusal(ctx, tx, networkID, "a floating-IP pool", callerScope{project: caller, admin: admin}); err != nil {
			return err
		}
		if req.FloatingIP.PortID != "" {
			port, err := portFixedIP(ctx, tx, callerScope{project: caller, admin: admin}, req.FloatingIP.PortID, req.FloatingIP.FixedIPAddress)
			if err != nil {
				return err
			}
			router, err := routerForFloatingIP(ctx, tx, port, networkID)
			if err != nil {
				return err
			}
			if err := requireOneFloatingIPPerFixedIP(ctx, tx, networkID, port, floatingIPRef{}); err != nil {
				return err
			}
			portID, fixedIP, routerID, status = &port.id, &port.fixedIP, &router, "ACTIVE"
		}
		var err error
		if floatingIP, err = allocateFloatingAddress(ctx, tx, networkID, req.FloatingIP.SubnetID, req.FloatingIP.FloatingIPAddress); err != nil {
			return err
		}
		if err := lockExternalNetwork(ctx, tx, networkID, "a floating-IP pool"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, floatingIPID, projectID, networkID, floatingIP, fixedIP, portID, routerID, status, req.FloatingIP.Description, now, now)
		return err
	})
	if err != nil {
		sendFloatingIPStoreError(c, err, "create")
		return
	}

	if portID != nil {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		cctx := context.WithoutCancel(ctx)
		routers := []string{*routerID}
		if err := svc.convergeRouterUnits(cctx, routers...); err != nil {
			if uerr := svc.undoFloatingIPCreate(cctx, floatingIPID, *portID, *fixedIP, req.FloatingIP.Description); uerr != nil {
				log.Error().Err(uerr).Str("floating_ip", floatingIP).Msg("remove a floating IP the node could not apply; the reconciliation pass applies its record")
			}
			if cerr := svc.convergeRouterUnits(cctx, routers...); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", floatingIP).Msg("converge a floating IP after an undo; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", floatingIP).Msg("this node could not apply the floating IP; the change was not applied")
			common.SendError(c, common.NewInternalServerError("failed to apply the floating IP"))
			return
		}
	}

	result := gin.H{
		"id":                  floatingIPID,
		"tenant_id":           projectID,
		"project_id":          projectID,
		"floating_network_id": networkID,
		"floating_ip_address": floatingIP,
		"status":              status,
		"description":         req.FloatingIP.Description,
		"created_at":          now.Format(time.RFC3339),
		"updated_at":          now.Format(time.RFC3339),
	}

	if fixedIP != nil {
		result["fixed_ip_address"] = *fixedIP
	} else {
		result["fixed_ip_address"] = nil
	}

	if portID != nil {
		result["port_id"] = *portID
	} else {
		result["port_id"] = nil
	}

	if routerID != nil {
		result["router_id"] = *routerID
	} else {
		result["router_id"] = nil
	}

	c.JSON(http.StatusCreated, gin.H{"floatingip": result})
}

// undoFloatingIPCreate removes a created floating IP the node could not apply, if its port, fixed
// IP and description still hold what the create wrote: a row another request has changed since stays
// (research R20). One statement, so the comparison and the delete are one step.
func (svc *Service) undoFloatingIPCreate(ctx context.Context, id, portID, fixedIP, description string) error {
	_, err := svc.pool.Exec(ctx, `
		DELETE FROM floating_ips
		WHERE id = $1 AND port_id::text = $2 AND fixed_ip_address = $3 AND COALESCE(description, '') = $4`,
		id, portID, fixedIP, description)
	return err
}

// sendFloatingIPStoreError answers a floating-IP change whose transaction failed: a refusal as it is,
// a row the request named that another request deleted meanwhile as 404, anything else as 500.
func sendFloatingIPStoreError(c *gin.Context, err error, operation string) {
	if database.IsForeignKeyViolation(err) {
		common.SendError(c, common.NewNotFoundError("network, port or router"))
		return
	}
	sendChangeError(c, err, operation, "floating IP")
}

// GetFloatingIP retrieves a single floating IP
func (svc *Service) GetFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	var fip FloatingIP
	var fixedIP, portID, routerID, description sql.NullString

	// id::text comparison resolves by UUID or floating-IP address without
	// crashing on a non-UUID identifier — the CLI looks a FIP up by its address
	// (e.g. `server add floating ip <vm> <address>`). By id an administrator reaches any project's
	// (SPEC-033 FR-057); an address, like a name, resolves in the caller's project.
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, project_id, floating_network_id, floating_ip_address,
		       fixed_ip_address, port_id, router_id, status, description,
		       created_at, updated_at
		FROM floating_ips
		WHERE ((id::text = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+`) OR (floating_ip_address = $1 AND project_id = $2))
		LIMIT 1
	`, floatingIPID, projectID, isAdmin(c)).Scan(&fip.ID, &fip.ProjectID, &fip.FloatingNetworkID, &fip.FloatingIPAddress,
		&fixedIP, &portID, &routerID, &fip.Status, &description, &fip.CreatedAt, &fip.UpdatedAt)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("floating IP"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_floatingip").Str("floatingip_id", floatingIPID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get floating IP"))
		return
	}

	result := gin.H{
		"id":                  fip.ID,
		"tenant_id":           fip.ProjectID,
		"project_id":          fip.ProjectID,
		"floating_network_id": fip.FloatingNetworkID,
		"floating_ip_address": fip.FloatingIPAddress,
		"status":              fip.Status,
		"created_at":          fip.CreatedAt.Format(time.RFC3339),
		"updated_at":          fip.UpdatedAt.Format(time.RFC3339),
	}

	if fixedIP.Valid {
		result["fixed_ip_address"] = fixedIP.String
	} else {
		result["fixed_ip_address"] = nil
	}

	if portID.Valid {
		result["port_id"] = portID.String
	} else {
		result["port_id"] = nil
	}

	if routerID.Valid {
		result["router_id"] = routerID.String
	} else {
		result["router_id"] = nil
	}

	if description.Valid {
		result["description"] = description.String
	} else {
		result["description"] = ""
	}

	c.JSON(http.StatusOK, gin.H{"floatingip": result})
}

// fipAssociation is a floating IP's association, as its row holds it.
type fipAssociation struct {
	port, fixed, router sql.NullString
	status              string
}

// disassociate clears the association, as a port's deletion leaves a floating IP: no port, fixed IP or
// router, DOWN.
func (a *fipAssociation) disassociate() { *a = fipAssociation{status: "DOWN"} }

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// floatingIPUpdate is a floating IP update's body: the port to associate, "" with portGiven to
// disassociate; the fixed IP requested on it; a description to set.
type floatingIPUpdate struct {
	port, fixedIP string
	portGiven     bool
	description   *string
}

// parseFloatingIPUpdate reads a floating IP update's body as upstream's attribute validators and
// _check_and_get_fip_assoc read it: a fixed_ip_address or a description that is not a string, and a
// fixed_ip_address without a port_id, are 400 (SPEC-033 FR-071, T271).
func parseFloatingIPUpdate(data map[string]any) (floatingIPUpdate, *common.OpenStackError) {
	var u floatingIPUpdate
	var value any
	if value, u.portGiven = data["port_id"]; u.portGiven && value != nil {
		s, ok := value.(string)
		if !ok {
			return u, common.NewBadRequestError("port_id must be a string or null")
		}
		u.port = s
	}
	if value, given := data["fixed_ip_address"]; given && value != nil {
		s, ok := value.(string)
		if !ok {
			return u, invalidAttribute("fixed_ip_address", fmt.Sprintf("'%v' is not a valid IP address", value))
		}
		u.fixedIP = s
	}
	if value, given := data["description"]; given {
		s, ok := value.(string)
		if !ok {
			return u, invalidAttribute("description", fmt.Sprintf("'%v' is not a valid string", value))
		}
		u.description = &s
	}
	if u.fixedIP != "" && u.port == "" {
		return u, errFixedIPWithoutPort()
	}
	return u, nil
}

// errFixedIPWithoutPort is upstream's refusal of a fixed_ip_address without a port_id.
func errFixedIPWithoutPort() *common.OpenStackError {
	return badRequest("floatingip", "fixed_ip_address cannot be specified without a port_id")
}

// UpdateFloatingIP associates, moves or disassociates a floating IP, and sets its description. A
// change to port_id is stored, then converged before the answer (applyFloatingIPChange); when that
// fails, the old association is put back, compare-and-set, and the request answers 500 (SPEC-033
// FR-008a, research R20).
func (svc *Service) UpdateFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID, admin := c.GetString("project_id"), isAdmin(c)
	ctx := c.Request.Context()

	// Parse raw JSON to detect if port_id key is present
	var rawReq map[string]map[string]any
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	update, refusal := parseFloatingIPUpdate(rawReq["floatingip"])
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}
	newPort, portGiven, requestedFixed, description := update.port, update.portGiven, update.fixedIP, update.description

	var id, address, network string
	var old, written fipAssociation
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		next := fipAssociation{status: "DOWN"}
		if newPort != "" {
			// The floating IP's network and address, read unlocked: neither ever changes.
			var current string
			err := tx.QueryRow(ctx, "SELECT floating_network_id::text, floating_ip_address FROM floating_ips WHERE id::text = $1 AND "+
				ownerOrAdmin("project_id", "$2", "$3"), floatingIPID, projectID, admin).Scan(&network, &current)
			if errors.Is(err, database.ErrNoRows) {
				return common.NewNotFoundError("floating IP")
			}
			if err != nil {
				return fmt.Errorf("read floating IP: %w", err)
			}
			// The new port first, then its router, and only then the row (research R20): a port's
			// deletion holds the port before it clears the floating IPs that name it, so the
			// opposite order could deadlock with it. The router is upstream's choice (FR-028), and
			// the fixed IP may have no other floating IP of the network (FR-029). An administrator may
			// name any project's port (FR-058).
			port, err := portFixedIP(ctx, tx, callerScope{project: projectID, admin: admin}, newPort, requestedFixed)
			if err != nil {
				return err
			}
			routerID, err := routerForFloatingIP(ctx, tx, port, network)
			if err != nil {
				return err
			}
			if err := requireOneFloatingIPPerFixedIP(ctx, tx, network, port, floatingIPRef{id: floatingIPID, address: current}); err != nil {
				return err
			}
			next = fipAssociation{port: nullable(newPort), fixed: nullable(port.fixedIP), router: nullable(routerID), status: "ACTIVE"}
		}
		err := tx.QueryRow(ctx, `
			SELECT id::text, floating_ip_address, floating_network_id::text, port_id::text, fixed_ip_address, router_id::text, COALESCE(status, '')
			FROM floating_ips WHERE id::text = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+` FOR UPDATE`, floatingIPID, projectID, admin,
		).Scan(&id, &address, &network, &old.port, &old.fixed, &old.router, &old.status)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("floating IP")
		}
		if err != nil {
			return fmt.Errorf("lock floating IP: %w", err)
		}
		// A floating IP its port forwardings use is theirs, as upstream's: a port for it is refused
		// (FipInUseByPortForwarding), and a null port_id leaves it and its router as they are
		// (SPEC-033 T192).
		associates := portGiven && (newPort != "" || old.port.Valid)
		if newPort != "" {
			var forwarded bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM port_forwardings WHERE floatingip_id::text = $1)", id).Scan(&forwarded); err != nil {
				return fmt.Errorf("read the port forwardings of floating IP %s: %w", id, err)
			}
			if forwarded {
				return common.NewNeutronError(http.StatusConflict, "FipInUseByPortForwarding",
					fmt.Sprintf("Floating IP %s in use by Port Forwarding resources.", id))
			}
		}
		written = old
		u := &common.SQLUpdate{}
		if associates {
			u.Set("port_id", nullOrValue(next.port))
			u.Set("fixed_ip_address", nullOrValue(next.fixed))
			u.Set("router_id", nullOrValue(next.router))
			u.Set("status", next.status)
		}
		if description != nil {
			u.Set("description", *description)
		}
		if u.Empty() {
			return nil
		}
		u.Set("updated_at", time.Now())
		if _, err := tx.Exec(ctx, "UPDATE floating_ips SET "+u.Assignments()+" WHERE id = "+u.Arg(id), u.Args()...); err != nil {
			return err
		}
		if associates {
			written = next
		}
		return nil
	})
	if err != nil {
		sendFloatingIPStoreError(c, err, "update")
		return
	}

	if portGiven {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		// The routers it went and goes through both change.
		cctx := context.WithoutCancel(ctx)
		routers := []string{old.router.String, written.router.String}
		if err := svc.convergeRouterUnits(cctx, routers...); err != nil {
			if uerr := svc.undoFloatingIPAssociation(cctx, network, floatingIPRef{id: id, address: address}, written, old); uerr != nil {
				log.Error().Err(uerr).Str("floating_ip", address).Msg("put a floating IP's association back; the reconciliation pass applies its record")
			}
			if cerr := svc.convergeRouterUnits(cctx, routers...); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", address).Msg("converge a floating IP after an undo; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", address).Msg("this node could not apply the floating IP's association; the change was not applied")
			common.SendError(c, common.NewInternalServerError("failed to apply the floating IP"))
			return
		}
	}

	// Return updated floating IP
	svc.GetFloatingIP(c)
}

func nullOrValue(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

// undoFloatingIPAssociation puts a floating IP's old association back if its row still holds the one
// this request wrote: an association another request has written since stays (research R20). The old
// association comes back only where the rules still allow it (keepRestorableAssociation), its port and
// router locked before the floating IP's row, as an association locks them; otherwise the floating IP is
// left unassociated, and the change made meanwhile stays (SPEC-033 T313).
func (svc *Service) undoFloatingIPAssociation(ctx context.Context, network string, fip floatingIPRef, written, old fipAssociation) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := keepRestorableAssociation(ctx, tx, network, fip, &old); err != nil {
			return err
		}
		var now fipAssociation
		err := tx.QueryRow(ctx, `
			SELECT port_id::text, fixed_ip_address, router_id::text, COALESCE(status, '')
			FROM floating_ips WHERE id = $1 FOR UPDATE`, fip.id).Scan(&now.port, &now.fixed, &now.router, &now.status)
		if errors.Is(err, database.ErrNoRows) {
			return nil // deleted since
		}
		if err != nil {
			return err
		}
		if now != written {
			log.Info().Str("floatingip_id", fip.id).Msg("undo: another change reached the floating IP since; it keeps that change")
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE floating_ips SET port_id = $1, fixed_ip_address = $2, router_id = $3, status = $4, updated_at = $5
			WHERE id = $6`, nullOrValue(old.port), nullOrValue(old.fixed), nullOrValue(old.router), old.status, time.Now(), fip.id)
		return err
	})
}

// floatingIPRecord is a floating IP's whole row, kept to restore a deletion the node could not apply.
type floatingIPRecord struct {
	id, address             string
	project, network, descr sql.NullString
	fipAssociation
	created, updated time.Time
}

// keepRestorableAssociation keeps an association floating IP self of network is to get back — a deletion's
// restore, an update's undo — only where the rules still allow it, checked as an association checks them and
// under its locks, in its order: the port, holding the fixed IP (portFixedIP); a router joining the fixed
// IP's subnet to the network (FR-028); and no other floating IP of the network on the fixed IP (FR-029).
// Otherwise — the port gone, the fixed IP off it, no router, another floating IP — the floating IP gets it
// back unassociated, and the change made meanwhile stays (SPEC-033 T270, T313). An association without a
// port, a port forwarding's router, is kept as it is.
func keepRestorableAssociation(ctx context.Context, tx database.Tx, network string, self floatingIPRef, a *fipAssociation) error {
	if !a.port.Valid || !a.fixed.Valid {
		return nil
	}
	// The floating IP's own association, which passed an association's checks once: its port, whichever
	// project holds it, locked as an association locks it.
	port := floatingPort{id: a.port.String}
	var fixed common.FixedIPs
	err := tx.QueryRow(ctx, "SELECT network_id::text, fixed_ips FROM ports WHERE id::text = $1 FOR UPDATE", port.id).Scan(&port.networkID, &fixed)
	if errors.Is(err, database.ErrNoRows) {
		a.disassociate() // the port is gone
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock port %s: %w", port.id, err)
	}
	port, err = port.withFixedIP(fixed, a.fixed.String)
	var router string
	if err == nil {
		router, err = routerForFloatingIP(ctx, tx, port, network)
	}
	if err == nil {
		err = requireOneFloatingIPPerFixedIP(ctx, tx, network, port, self)
	}
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		a.disassociate()
		return nil
	}
	if err != nil {
		return err
	}
	a.router = nullable(router)
	return nil
}

// portForwardingRecord is a port forwarding's whole row, deleted with its floating IP.
type portForwardingRecord struct {
	id, project, internalPort, internalIP, protocol string
	externalPortNum, internalPortNum                int
	status, descr                                   sql.NullString
	created, updated                                time.Time
}

// DeleteFloatingIP releases a floating IP. One transaction deletes the row and, through the foreign
// key, its port forwardings. A floating IP through a router — on a port, or with port forwardings — is
// then converged away before the answer; when that fails, the row and its port forwardings are
// restored and the request answers 500 (SPEC-033 FR-008a, research R20).
func (svc *Service) DeleteFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var row floatingIPRecord
	var forwardings []portForwardingRecord
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id::text, floating_ip_address, project_id::text, floating_network_id::text, fixed_ip_address,
			       port_id::text, router_id::text, COALESCE(status, ''), description, created_at, updated_at
			FROM floating_ips WHERE id::text = $1 AND `+ownerOrAdmin("project_id", "$2", "$3")+` FOR UPDATE`, floatingIPID, projectID, isAdmin(c),
		).Scan(&row.id, &row.address, &row.project, &row.network, &row.fixed, &row.port, &row.router,
			&row.status, &row.descr, &row.created, &row.updated)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("floating IP")
		}
		if err != nil {
			return fmt.Errorf("lock floating IP: %w", err)
		}
		if forwardings, err = readPortForwardings(ctx, tx, row.id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM floating_ips WHERE id = $1", row.id)
		return err
	})
	if err != nil {
		sendFloatingIPStoreError(c, err, "delete")
		return
	}

	if row.router.Valid {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		// Its router changes, its port forwardings' rules going with it.
		cctx := context.WithoutCancel(ctx)
		routers := []string{row.router.String}
		if err := svc.convergeRouterUnits(cctx, routers...); err != nil {
			if rerr := svc.restoreFloatingIP(cctx, row, forwardings); rerr != nil {
				log.Error().Err(rerr).Str("floating_ip", row.address).Msg("restore a floating IP the node could not withdraw; the reconciliation pass removes it")
			}
			if cerr := svc.convergeRouterUnits(cctx, routers...); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", row.address).Msg("converge a floating IP after its restore; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", row.address).Msg("this node could not withdraw the floating IP; the deletion was not applied")
			common.SendError(c, common.NewInternalServerError("failed to withdraw the floating IP"))
			return
		}
	}

	c.Status(http.StatusNoContent)
}

func readPortForwardings(ctx context.Context, tx database.Tx, floatingIPID string) ([]portForwardingRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, project_id::text, internal_port_id::text, internal_ip_address, protocol,
		       external_port, internal_port, status, description, created_at, updated_at
		FROM port_forwardings WHERE floatingip_id = $1`, floatingIPID)
	if err != nil {
		return nil, fmt.Errorf("read port forwardings: %w", err)
	}
	defer rows.Close()
	var out []portForwardingRecord
	for rows.Next() {
		var p portForwardingRecord
		if err := rows.Scan(&p.id, &p.project, &p.internalPort, &p.internalIP, &p.protocol,
			&p.externalPortNum, &p.internalPortNum, &p.status, &p.descr, &p.created, &p.updated); err != nil {
			return nil, fmt.Errorf("read port forwarding: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// restoreFloatingIP puts a deleted floating IP and its port forwardings back, every column as it was.
// What other requests deleted meanwhile is applied as the foreign keys would have applied it
// (research R20): without its project or network nothing comes back; without its port it comes back
// unassociated, as a port's deletion leaves a floating IP; without its router, without one; a port
// forwarding whose internal port is gone stays deleted; and an address something holds again — a
// floating IP, a port — keeps it deleted, since that later change stays: the
// allocator's locks are taken and addressHeldError answered (SPEC-033 T188).
func (svc *Service) restoreFloatingIP(ctx context.Context, row floatingIPRecord, forwardings []portForwardingRecord) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, floatingIPAllocationLock); err != nil {
			return err
		}
		exists := func(table string, id sql.NullString) (bool, error) {
			if !id.Valid {
				return false, nil
			}
			var ok bool
			err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id::text = $1)", id.String).Scan(&ok)
			return ok, err
		}
		for _, owner := range []struct {
			table string
			id    sql.NullString
		}{{"projects", row.project}, {"networks", row.network}} {
			ok, err := exists(owner.table, owner.id)
			if err != nil {
				return err
			}
			if owner.id.Valid && !ok {
				return nil // its project or network is gone: the floating IP would have gone with it
			}
		}
		// The association first, its port and router locked before the address's subnet, as a create
		// locks them (SPEC-033 T313).
		if err := keepRestorableAssociation(ctx, tx, row.network.String, floatingIPRef{id: row.id, address: row.address}, &row.fipAssociation); err != nil {
			return err
		}
		if err := requireAddressesFree(ctx, tx, row.network.String, []string{row.address}); err != nil {
			return err
		}
		if ok, err := exists("routers", row.router); err != nil {
			return err
		} else if row.router.Valid && !ok {
			row.router = sql.NullString{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			row.id, nullOrValue(row.project), nullOrValue(row.network), row.address, nullOrValue(row.fixed),
			nullOrValue(row.port), nullOrValue(row.router), nullOrValue(nullable(row.status)), nullOrValue(row.descr),
			row.created, row.updated); err != nil {
			return fmt.Errorf("restore floating IP %s: %w", row.address, err)
		}
		for _, p := range forwardings {
			ok, err := exists("ports", nullable(p.internalPort))
			if err != nil {
				return err
			}
			if !ok {
				continue // its internal port is gone: the port forwarding would have gone with it
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id, internal_ip_address, external_port, internal_port, protocol, status, description, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				p.id, p.project, row.id, p.internalPort, p.internalIP, p.externalPortNum, p.internalPortNum,
				p.protocol, nullOrValue(p.status), nullOrValue(p.descr), p.created, p.updated); err != nil {
				return fmt.Errorf("restore port forwarding %s: %w", p.id, err)
			}
		}
		return nil
	})
}

// FloatingIPAssociation is a floating IP's association as it stood before a change took it away: the
// fixed IP it reached, through which router.
type FloatingIPAssociation struct{ Address, FixedIP, RouterID string }

// ConvergeDisassociated makes the disassociation of a deleted port's floating IPs in force: their
// routers, on the network node. A port's deletion, a failed boot's release and a server's deletion call
// it after the transaction that disassociated them (SPEC-033 research R25). The guests' own firewall does
// not change with a floating IP: its traffic reaches them through the router, and their ports' chains
// filter it (R32). A failure is logged, naming the floating IPs, and the deletion stands: the ports'
// teardown has run without an undo, and the reconciliation pass removes what is left (R25, a recorded
// departure).
func (svc *Service) ConvergeDisassociated(ctx context.Context, was []FloatingIPAssociation) {
	if len(was) == 0 {
		return
	}
	routers := make([]string, 0, len(was))
	addresses := make([]string, 0, len(was))
	for _, a := range was {
		routers = append(routers, a.RouterID)
		addresses = append(addresses, a.Address)
	}
	if err := svc.convergeRouterUnits(ctx, routers...); err != nil {
		log.Error().Err(err).Strs("floating_ips", addresses).
			Msg("withdraw the floating IPs of a deleted port; the reconciliation pass removes what is left")
	}
}

// RefreshPortFilters converges the firewall of ports whose groups changed, on the nodes they are bound
// to, and of every port whose rules expand a group in groups, whose members changed with them
// (SPEC-033 FR-007, research R32, R33). A change to a port's groups calls it after committing, so the
// change is in force before the request answers; an error means it is not, and the caller undoes the
// change (FR-008).
func (svc *Service) RefreshPortFilters(ctx context.Context, portIDs, groups []string) error {
	return svc.convergePorts(ctx, portChange{ports: portIDs, members: groups})
}

// convergeGroupRules converges the firewall of every bound port carrying a group whose rules changed,
// on its node (R32).
func (svc *Service) convergeGroupRules(ctx context.Context, sgID string) error {
	return svc.convergePorts(ctx, portChange{carrying: []string{sgID}})
}
