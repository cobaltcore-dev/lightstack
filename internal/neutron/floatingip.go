package neutron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
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
		Description       string `json:"description"`
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

// ListFloatingIPs lists all floating IPs for the project
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
	// The caller's project — unless an administrator filters by tenant:
	// upstream honours the filter for admin tokens, and it was silently
	// ignored before, so `floating ip list --project X` presented the
	// ADMIN's addresses as project X's (SPEC-031; mirrors
	// buildNetworkListFilter's tenant bypass). ::text so a non-UUID value
	// narrows to nothing instead of failing the cast.
	fipTenant := c.Query("tenant_id")
	if fipTenant == "" {
		fipTenant = c.Query("project_id")
	}
	if fipTenant != "" && common.HasRole(c, common.RoleAdmin) {
		f.Eq("project_id::text", fipTenant)
	} else {
		f.Eq("project_id", projectID)
	}

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
	// listing's own scope (see the tenant bypass above): an unresolvable marker would silently
	// drop the condition and make the walk restart from the first page forever.
	fipAdminBypass := fipTenant != "" && common.HasRole(c, common.RoleAdmin)
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		markerQuery := "SELECT created_at, id FROM floating_ips WHERE id = $1"
		markerArgs := []any{marker}
		if !fipAdminBypass {
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
// converged on this node before the answer; when the node cannot apply it, the record is removed
// again, compare-and-set, and the request answers 500 (SPEC-033 FR-008a, research R20).
func (svc *Service) CreateFloatingIP(c *gin.Context) {
	var req CreateFloatingIPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	ctx := c.Request.Context()
	projectID := c.GetString("project_id")
	floatingIPID := uuid.New().String()
	networkID := req.FloatingIP.FloatingNetworkID
	// The body's own checks, before anything is read.
	if refusal := networkIDRefusal(networkID, "a floating-IP pool"); refusal != nil {
		common.SendError(c, refusal)
		return
	}

	// Get external network subnet to allocate IP from
	var subnetCIDR string
	err := svc.pool.QueryRow(ctx,
		"SELECT cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		networkID,
	).Scan(&subnetCIDR)

	if errors.Is(err, database.ErrNoRows) {
		// No subnet exists - use default external IP pool (RFC 5737 TEST-NET-1)
		subnetCIDR = "192.0.2.0/24"
	} else if err != nil {
		log.Error().Err(err).Str("operation", "create_floatingip_subnet_lookup").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to allocate floating IP"))
		return
	}

	// The address is chosen before the transaction, as before; the insert's unique constraint
	// re-checks it.
	floatingIP, err := svc.allocateFloatingIP(ctx, subnetCIDR)
	if err != nil {
		log.Error().Err(err).Str("operation", "allocate_floatingip").Msg("failed to allocate floating IP")
		common.SendError(c, common.NewInternalServerError("failed to allocate floating IP"))
		return
	}

	// Determine status and associated details
	status := "DOWN"
	var fixedIP, portID, routerID *string
	now := time.Now()

	// One transaction makes the checks and stores the row; nothing is programmed before it commits
	// (constitution v1.5.0, limit 2; research R20).
	err = database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		// 1. The floating network, locked: it must stay external until the row is in.
		if err := externalNetworkRefusal(ctx, tx, networkID, projectID, "a floating-IP pool", true); err != nil {
			return err
		}
		if req.FloatingIP.PortID != "" {
			// 2. The port, the caller's, with its fixed IP. No lock: a port's project never changes,
			// and the foreign key re-checks that it still exists at the insert.
			fixed, portNetwork, err := portFixedIP(ctx, tx, req.FloatingIP.PortID, projectID, req.FloatingIP.FixedIPAddress, false)
			if err != nil {
				return err
			}
			portID, fixedIP, status = &req.FloatingIP.PortID, &fixed, "ACTIVE"
			// 3. The router, found as before and then locked by id. Without one the floating IP is
			// associated all the same; the node's NAT does not route through it.
			if rID, ok, err := lockGatewayRouter(ctx, tx, portNetwork); err != nil {
				return err
			} else if ok {
				routerID = &rID
			}
		}
		// 4. The row.
		_, err := tx.Exec(ctx, `
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
		if err := svc.convergeFloatingIP(cctx, floatingIP); err != nil {
			if uerr := svc.undoFloatingIPCreate(cctx, floatingIPID, *portID, *fixedIP, req.FloatingIP.Description); uerr != nil {
				log.Error().Err(uerr).Str("floating_ip", floatingIP).Msg("remove a floating IP the node could not apply; the reconciliation pass applies its record")
			}
			if cerr := svc.convergeFloatingIP(cctx, floatingIP); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", floatingIP).Msg("converge a floating IP after an undo; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", floatingIP).Msg("this node could not apply the floating IP; the change was undone")
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
	var refusal *common.OpenStackError
	switch {
	case errors.As(err, &refusal):
		common.SendError(c, refusal)
	case database.IsForeignKeyViolation(err):
		common.SendError(c, common.NewNotFoundError("network, port or router"))
	default:
		log.Error().Err(err).Str("operation", operation+"_floatingip").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to "+operation+" floating IP"))
	}
}

// portFixedIP reads a port of the caller's project and the fixed IP a floating IP reaches on it: the
// one requested, or the port's first. It returns the port's network too. forUpdate locks the port.
func portFixedIP(ctx context.Context, q subnetQuerier, portID, projectID, requested string, forUpdate bool) (fixedIP, networkID string, err error) {
	query := "SELECT network_id::text, fixed_ips FROM ports WHERE id::text = $1 AND project_id = $2"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var raw []byte
	err = q.QueryRow(ctx, query, portID, projectID).Scan(&networkID, &raw)
	if errors.Is(err, database.ErrNoRows) {
		return "", "", common.NewNotFoundError("port")
	}
	if err != nil {
		return "", "", fmt.Errorf("read port %s: %w", portID, err)
	}
	if requested != "" {
		return requested, networkID, nil
	}
	var fixedIPs []struct {
		IPAddress string `json:"ip_address"`
	}
	if err := json.Unmarshal(raw, &fixedIPs); err != nil {
		return "", "", fmt.Errorf("parse the fixed IPs of port %s: %w", portID, err)
	}
	for _, f := range fixedIPs {
		if f.IPAddress != "" {
			return f.IPAddress, networkID, nil
		}
	}
	return "", "", common.NewBadRequestError("port has no fixed IP addresses")
}

// lockGatewayRouter finds the router with an external gateway that has an interface on networkID, as
// before, and locks it by id in a query of its own: FOR UPDATE OF is outside the shared SQL subset
// (internal/database/doc.go). ok is false when there is none, or its gateway went meanwhile.
func lockGatewayRouter(ctx context.Context, tx database.Tx, networkID string) (string, bool, error) {
	var routerID string
	err := tx.QueryRow(ctx, `
		SELECT DISTINCT r.id::text
		FROM routers r
		JOIN router_interfaces ri ON ri.router_id = r.id
		JOIN ports p ON p.id = ri.port_id
		WHERE p.network_id::text = $1 AND r.external_gateway_info IS NOT NULL
		LIMIT 1
	`, networkID).Scan(&routerID)
	if errors.Is(err, database.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find the router of network %s: %w", networkID, err)
	}
	err = tx.QueryRow(ctx,
		"SELECT id::text FROM routers WHERE id::text = $1 AND external_gateway_info IS NOT NULL FOR UPDATE", routerID,
	).Scan(&routerID)
	if errors.Is(err, database.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock router %s: %w", routerID, err)
	}
	return routerID, true, nil
}

// GetFloatingIP retrieves a single floating IP
func (svc *Service) GetFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	var fip FloatingIP
	var fixedIP, portID, routerID, description sql.NullString

	// Administrators may address any floating IP by id. Address lookups stay
	// in the caller's project so they cannot select another tenant's row.
	fipQuery := `
		SELECT id, project_id, floating_network_id, floating_ip_address,
		       fixed_ip_address, port_id, router_id, status, description,
		       created_at, updated_at
		FROM floating_ips
		WHERE (id::text = $1 AND `
	idScope := `project_id = $2`
	if common.HasRole(c, common.RoleAdmin) {
		idScope = `TRUE`
	}
	fipQuery += idScope + `) OR (floating_ip_address = $1 AND project_id = $2)`
	err := svc.pool.QueryRow(c.Request.Context(), fipQuery+" LIMIT 1", floatingIPID, projectID).
		Scan(&fip.ID, &fip.ProjectID, &fip.FloatingNetworkID, &fip.FloatingIPAddress,
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

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// UpdateFloatingIP associates, moves or disassociates a floating IP, and sets its description. A
// change to port_id is stored, then converged on this node before the answer; when the node cannot
// apply it, the old association is put back, compare-and-set, and the request answers 500
// (SPEC-033 FR-008a, research R20).
func (svc *Service) UpdateFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Parse raw JSON to detect if port_id key is present
	var rawReq map[string]map[string]any
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	data := rawReq["floatingip"]
	portValue, portGiven := data["port_id"]
	newPort := ""
	if portGiven && portValue != nil {
		s, ok := portValue.(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("port_id must be a string or null"))
			return
		}
		newPort = s
	}
	requestedFixed := ""
	if v, ok := data["fixed_ip_address"]; ok && v != nil {
		requestedFixed, _ = v.(string)
	}
	var description *string
	if v, ok := data["description"]; ok {
		if s, ok := v.(string); ok {
			description = &s
		}
	}

	var id, address string
	var old, written fipAssociation
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		next := fipAssociation{status: "DOWN"}
		if newPort != "" {
			// The new port first, then its router, and only then the row (research R20): a port's
			// deletion holds the port before it clears the floating IPs that name it, so the
			// opposite order could deadlock with it.
			fixed, portNetwork, err := portFixedIP(ctx, tx, newPort, projectID, requestedFixed, true)
			if err != nil {
				return err
			}
			routerID, ok, err := lockGatewayRouter(ctx, tx, portNetwork)
			if err != nil {
				return err
			}
			if !ok {
				return common.NewBadRequestError("no router with external gateway found for this network")
			}
			next = fipAssociation{port: nullable(newPort), fixed: nullable(fixed), router: nullable(routerID), status: "ACTIVE"}
		}
		err := tx.QueryRow(ctx, `
			SELECT id::text, floating_ip_address, port_id::text, fixed_ip_address, router_id::text, COALESCE(status, '')
			FROM floating_ips WHERE id::text = $1 AND project_id = $2 FOR UPDATE`, floatingIPID, projectID,
		).Scan(&id, &address, &old.port, &old.fixed, &old.router, &old.status)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("floating IP")
		}
		if err != nil {
			return fmt.Errorf("lock floating IP: %w", err)
		}
		u := &common.SQLUpdate{}
		if portGiven {
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
		written = next
		return nil
	})
	if err != nil {
		sendFloatingIPStoreError(c, err, "update")
		return
	}

	if portGiven {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		cctx := context.WithoutCancel(ctx)
		if err := svc.convergeFloatingIP(cctx, address); err != nil {
			if uerr := svc.undoFloatingIPAssociation(cctx, id, written, old); uerr != nil {
				log.Error().Err(uerr).Str("floating_ip", address).Msg("put a floating IP's association back; the reconciliation pass applies its record")
			}
			if cerr := svc.convergeFloatingIP(cctx, address); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", address).Msg("converge a floating IP after an undo; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", address).Msg("this node could not apply the floating IP's association; the change was undone")
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
// this request wrote: an association another request has written since stays (research R20).
func (svc *Service) undoFloatingIPAssociation(ctx context.Context, id string, written, old fipAssociation) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var now fipAssociation
		err := tx.QueryRow(ctx, `
			SELECT port_id::text, fixed_ip_address, router_id::text, COALESCE(status, '')
			FROM floating_ips WHERE id = $1 FOR UPDATE`, id).Scan(&now.port, &now.fixed, &now.router, &now.status)
		if errors.Is(err, database.ErrNoRows) {
			return nil // deleted since
		}
		if err != nil {
			return err
		}
		if now != written {
			log.Info().Str("floatingip_id", id).Msg("undo: another change reached the floating IP since; it keeps that change")
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE floating_ips SET port_id = $1, fixed_ip_address = $2, router_id = $3, status = $4, updated_at = $5
			WHERE id = $6`, nullOrValue(old.port), nullOrValue(old.fixed), nullOrValue(old.router), old.status, time.Now(), id)
		return err
	})
}

// floatingIPRecord is a floating IP's whole row, kept to restore a deletion the node could not apply.
type floatingIPRecord struct {
	id, address                                          string
	project, network, fixed, port, router, status, descr sql.NullString
	created, updated                                     time.Time
}

func (r floatingIPRecord) associated() bool { return r.port.Valid && r.fixed.Valid }

// portForwardingRecord is a port forwarding's whole row, deleted with its floating IP.
type portForwardingRecord struct {
	id, project, internalPort, internalIP, protocol string
	externalPortNum, internalPortNum                int
	status, descr                                   sql.NullString
	created, updated                                time.Time
}

// DeleteFloatingIP releases a floating IP. One transaction deletes the row and, through the foreign
// key, its port forwardings. An associated floating IP is then converged on this node before the
// answer; when the node cannot apply it, the row and its port forwardings are restored and the request
// answers 500 (SPEC-033 FR-008a, research R20).
func (svc *Service) DeleteFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var row floatingIPRecord
	var forwardings []portForwardingRecord
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id::text, floating_ip_address, project_id::text, floating_network_id::text, fixed_ip_address,
			       port_id::text, router_id::text, status, description, created_at, updated_at
			FROM floating_ips WHERE id::text = $1 AND project_id = $2 FOR UPDATE`, floatingIPID, projectID,
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

	if row.associated() {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		cctx := context.WithoutCancel(ctx)
		if err := svc.convergeFloatingIP(cctx, row.address); err != nil {
			if rerr := svc.restoreFloatingIP(cctx, row, forwardings); rerr != nil {
				log.Error().Err(rerr).Str("floating_ip", row.address).Msg("restore a floating IP the node could not withdraw; the reconciliation pass removes it")
			}
			if cerr := svc.convergeFloatingIP(cctx, row.address); cerr != nil {
				log.Error().Err(cerr).Str("floating_ip", row.address).Msg("converge a floating IP after its restore; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("floating_ip", row.address).Msg("this node could not withdraw the floating IP; the deletion was undone")
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
// forwarding whose internal port is gone stays deleted; and an address allocated again keeps it
// deleted, since that later change stays.
func (svc *Service) restoreFloatingIP(ctx context.Context, row floatingIPRecord, forwardings []portForwardingRecord) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
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
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM floating_ips WHERE floating_ip_address = $1)", row.address).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return nil
		}
		if ok, err := exists("ports", row.port); err != nil {
			return err
		} else if row.port.Valid && !ok {
			row.port, row.fixed, row.router = sql.NullString{}, sql.NullString{}, sql.NullString{}
			row.status = sql.NullString{String: "DOWN", Valid: true}
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
			nullOrValue(row.port), nullOrValue(row.router), nullOrValue(row.status), nullOrValue(row.descr),
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

// ConvergeFloatingIPs converges each floating IP on this node (convergeFloatingIP) and joins the
// failures. A server's deletion calls it after the transaction that disassociated them (SPEC-033
// research R25).
func (svc *Service) ConvergeFloatingIPs(ctx context.Context, floatingIPs []string) error {
	var errs []error
	for _, f := range floatingIPs {
		if err := svc.convergeFloatingIP(ctx, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// convergeFloatingIP makes this node hold what floating IP F's record says (SPEC-033 research R23),
// under the node lock, with the uplink from svc.uplink(), whose error is a failed change. F
// associated with X: X's filter, then F's NAT to X and F's address. Otherwise, or without a record:
// F withdrawn, the address first. Then each fixed IP the NAT pointed at before loses its filter,
// unless a record still associates a floating IP with it or a rule of the shape on the node still
// reaches it. Errors name the floating IP and the address.
func (svc *Service) convergeFloatingIP(ctx context.Context, floatingIP string) error {
	release, err := svc.lockNode()
	if err != nil {
		return fmt.Errorf("floating IP %s: %w", floatingIP, err)
	}
	defer release()
	uplink, err := svc.uplink()
	if err != nil {
		return fmt.Errorf("floating IP %s: no uplink: %w", floatingIP, err)
	}
	_, err = svc.convergeFloatingIPLocked(ctx, floatingIP, uplink)
	return err
}

// convergeFloatingIPLocked is convergeFloatingIP for a caller that holds the node lock. It reads the
// record itself, under that lock, so it acts on the record as it is then. It reports the fixed IPs
// whose filters it removed.
func (svc *Service) convergeFloatingIPLocked(ctx context.Context, floatingIP, uplink string) ([]string, error) {
	var fixed, port sql.NullString
	err := svc.pool.QueryRow(ctx,
		"SELECT fixed_ip_address, port_id::text FROM floating_ips WHERE floating_ip_address = $1", floatingIP,
	).Scan(&fixed, &port)
	if err != nil && !errors.Is(err, database.ErrNoRows) {
		return nil, fmt.Errorf("floating IP %s: read its record: %w", floatingIP, err)
	}
	var replaced []string
	if fixed.Valid && fixed.String != "" && port.Valid {
		rules, err := svc.portIngressRules(ctx, port.String)
		if err != nil {
			return nil, fmt.Errorf("floating IP %s: read the rules of port %s: %w", floatingIP, port.String, err)
		}
		if err := svc.dataplane.EnsureIngressFilter(fixed.String, uplink, rules); err != nil {
			return nil, fmt.Errorf("floating IP %s: filter of %s: %w", floatingIP, fixed.String, err)
		}
		if replaced, err = svc.dataplane.EnsureFloatingIP(floatingIP, fixed.String, uplink); err != nil {
			return nil, fmt.Errorf("floating IP %s: NAT to %s: %w", floatingIP, fixed.String, err)
		}
	} else if replaced, err = svc.dataplane.WithdrawFloatingIP(floatingIP, uplink); err != nil {
		return nil, fmt.Errorf("floating IP %s: withdraw: %w", floatingIP, err)
	}
	removed, err := svc.removeUnreachedFilters(ctx, uplink, replaced)
	if err != nil {
		return removed, fmt.Errorf("floating IP %s: %w", floatingIP, err)
	}
	return removed, nil
}

// removeUnreachedFilters removes the filter of each fixed IP that no record associates a floating IP
// with and that no rule of the shape on the node still reaches, marked or not (research R23): a filter
// removed while something still forwards to its fixed IP would hand that traffic to the host's FORWARD
// policy. The caller holds the node lock. It reports the fixed IPs whose filters it removed.
func (svc *Service) removeUnreachedFilters(ctx context.Context, uplink string, fixedIPs []string) ([]string, error) {
	if len(fixedIPs) == 0 {
		return nil, nil
	}
	state, err := svc.dataplane.NorthSouthState(uplink)
	if err != nil {
		return nil, fmt.Errorf("list the node: %w", err)
	}
	var removed []string
	for _, x := range fixedIPs {
		if natReaches(state.NAT, x) {
			continue
		}
		held, err := svc.fixedIPAssociated(ctx, x)
		if err != nil {
			return removed, err
		}
		if held {
			continue
		}
		if err := svc.dataplane.RemoveIngressFilter(x); err != nil {
			return removed, fmt.Errorf("fixed IP %s: remove its filter: %w", x, err)
		}
		removed = append(removed, x)
	}
	return removed, nil
}

// fixedIPAssociated reports whether a record associates any floating IP with fixedIP.
func (svc *Service) fixedIPAssociated(ctx context.Context, fixedIP string) (bool, error) {
	var held bool
	if err := svc.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM floating_ips WHERE fixed_ip_address = $1 AND port_id IS NOT NULL)", fixedIP,
	).Scan(&held); err != nil {
		return false, fmt.Errorf("fixed IP %s: read its associations: %w", fixedIP, err)
	}
	return held, nil
}

// natReaches reports whether any listed rule of the shape points at fixedIP.
func natReaches(nat []networking.FloatingNAT, fixedIP string) bool {
	for _, r := range nat {
		if r.FixedIP == fixedIP {
			return true
		}
	}
	return false
}

// portIngressRules returns the IP-based ingress rules of a port's security
// groups (skipping same-group / remote_group_id rules, which are east-west) —
// what the north-south ingress filter enforces for a floating IP (T034).
// portIngressRules returns the ingress rules to program for a port. It reports an error
// rather than a short list when the read fails: callers replace the port's whole filter
// with what they get back, so a truncated list would silently drop firewall allowances.
// On error the caller must leave the existing filter in place.
func (svc *Service) portIngressRules(ctx context.Context, portID string) ([]networking.IngressRule, error) {
	rows, err := svc.pool.Query(ctx, `
		SELECT r.protocol, r.port_range_min, r.port_range_max, r.remote_ip_prefix
		FROM security_group_rules r
		JOIN port_security_groups psg ON psg.security_group_id = r.security_group_id
		WHERE psg.port_id = $1 AND r.direction = 'ingress'
		  AND (r.ethertype = 'IPv4' OR r.ethertype IS NULL)
		  AND r.remote_group_id IS NULL`, portID)
	if err != nil {
		log.Warn().Err(err).Str("port_id", portID).Msg("WS-3: failed to query ingress rules")
		return nil, err
	}
	defer rows.Close()
	var out []networking.IngressRule
	for rows.Next() {
		var proto, remote sql.NullString
		var pmin, pmax sql.NullInt32
		if err := rows.Scan(&proto, &pmin, &pmax, &remote); err != nil {
			log.Warn().Err(err).Str("operation", "portIngressRules").Msg("failed to scan row")
			continue
		}
		out = append(out, networking.IngressRule{
			Protocol: proto.String,
			PortMin:  int(pmin.Int32),
			PortMax:  int(pmax.Int32),
			Remote:   remote.String,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Str("port_id", portID).Msg("WS-3: ingress rules rows iteration error; refusing to program a partial filter")
		return nil, err
	}
	return out, nil
}

// ensureIngressFilter rebuilds the filter of one fixed IP from the port's groups, under the node
// lock: the rules are read inside it, so a reconciliation pass never applies rules it read before a
// change the node has since applied (SPEC-033 research R8). A rule set that cannot be read in full
// leaves the existing filter in place. A lock not acquired within a second is an error naming the
// address, wrapping errNodeBusy.
func (svc *Service) ensureIngressFilter(ctx context.Context, fixedIP, uplink, portID string) error {
	release, err := svc.lockNode()
	if err != nil {
		return fmt.Errorf("ingress filter for %s: %w", fixedIP, err)
	}
	defer release()
	rules, err := svc.portIngressRules(ctx, portID)
	if err != nil {
		return err
	}
	return svc.dataplane.EnsureIngressFilter(fixedIP, uplink, rules)
}

// RefreshPortFilters rebuilds, on this node, the filter of every fixed IP the ports' floating IPs
// reach, from what the database now holds (SPEC-033 FR-007, research R9). A change to a port's
// groups calls it after committing, so the change reaches the dataplane before the request answers.
// A port without a floating IP has nothing to rebuild.
//
// It tries every address and returns the failures joined, each naming its port and address. An
// error means the change has not reached the dataplane, and the old filter still stands: the caller
// undoes its change (FR-008). It never skips the work, so a node without an uplink is a failure. In
// stub mode nothing is programmed, and only a database error can fail it.
func (svc *Service) RefreshPortFilters(ctx context.Context, portIDs []string) error {
	var errs []error
	var uplink string
	var uplinkErr error
	uplinkRead := false
	for _, portID := range portIDs {
		addrs, err := svc.floatingFixedIPs(ctx, portID)
		if err != nil {
			errs = append(errs, fmt.Errorf("port %s: read its floating IPs: %w", portID, err))
			continue
		}
		if len(addrs) == 0 {
			continue
		}
		if !uplinkRead {
			uplink, uplinkErr = svc.uplink()
			uplinkRead = true
		}
		if uplinkErr != nil {
			errs = append(errs, fmt.Errorf("port %s: no uplink to filter its floating IPs on: %w", portID, uplinkErr))
			continue
		}
		for _, addr := range addrs {
			if err := svc.ensureIngressFilter(ctx, addr, uplink, portID); err != nil {
				errs = append(errs, fmt.Errorf("port %s, address %s: %w", portID, addr, err))
			}
		}
	}
	return errors.Join(errs...)
}

// floatingFixedIPs returns the fixed IPs of a port that a floating IP reaches.
func (svc *Service) floatingFixedIPs(ctx context.Context, portID string) ([]string, error) {
	rows, err := svc.pool.Query(ctx, `
		SELECT DISTINCT fixed_ip_address FROM floating_ips
		WHERE port_id = $1 AND fixed_ip_address IS NOT NULL
		ORDER BY fixed_ip_address`, portID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, rows.Err()
}

// rebuildGroupFilters rebuilds, on this node, the filter of every fixed IP whose floating IP's port
// carries the group, one address at a time under the node lock, with the uplink from svc.uplink(),
// whose error is a failed change (SPEC-033 research R21). It returns the failures joined, each naming
// its address. A read that fails is a failed change too: it never skips the work.
func (svc *Service) rebuildGroupFilters(ctx context.Context, sgID string) error {
	rows, err := svc.pool.Query(ctx, `
		SELECT DISTINCT f.fixed_ip_address, f.port_id::text
		FROM floating_ips f
		JOIN port_security_groups psg ON psg.port_id = f.port_id
		WHERE psg.security_group_id::text = $1 AND f.port_id IS NOT NULL AND f.fixed_ip_address IS NOT NULL
		ORDER BY f.fixed_ip_address`, sgID)
	if err != nil {
		return fmt.Errorf("read the floating IPs of group %s: %w", sgID, err)
	}
	type target struct{ fixed, port string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.fixed, &t.port); err != nil {
			rows.Close()
			return fmt.Errorf("read a floating IP of group %s: %w", sgID, err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the floating IPs of group %s: %w", sgID, err)
	}
	if len(targets) == 0 {
		return nil
	}
	uplink, err := svc.uplink()
	if err != nil {
		return fmt.Errorf("no uplink to rebuild the filters of group %s on: %w", sgID, err)
	}
	var errs []error
	for _, t := range targets {
		if err := svc.ensureIngressFilter(ctx, t.fixed, uplink, t.port); err != nil {
			errs = append(errs, fmt.Errorf("port %s, address %s: %w", t.port, t.fixed, err))
		}
	}
	return errors.Join(errs...)
}

// Helper function to allocate a floating IP from subnet
func (svc *Service) allocateFloatingIP(ctx context.Context, subnetCIDR string) (string, error) {
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return "", err
	}

	// Get all allocated floating IPs in this subnet
	rows, err := svc.pool.Query(ctx,
		"SELECT floating_ip_address FROM floating_ips",
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	allocatedIPs := make(map[string]bool)
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			log.Warn().Err(err).Str("operation", "allocateFloatingIP").Msg("failed to scan row")
			continue
		}
		allocatedIPs[ip] = true
	}
	// A partial read leaves allocatedIPs incomplete, which would hand out an IP that
	// is already allocated — fail instead of risking a duplicate floating IP.
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("floating IP allocation: rows iteration error")
		return "", fmt.Errorf("failed to read allocated floating IPs: %w", err)
	}

	// Find first available IP in range
	// Start from .100 to avoid gateway and DHCP range
	ip := incrementIP(ipNet.IP, 100)
	for ipNet.Contains(ip) {
		ipStr := ip.String()
		if !allocatedIPs[ipStr] {
			return ipStr, nil
		}
		ip = incrementIP(ip, 1)
	}

	return "", fmt.Errorf("no available IPs in subnet %s", subnetCIDR)
}
