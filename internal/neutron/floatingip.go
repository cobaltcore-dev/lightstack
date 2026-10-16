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

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	// ADMIN's addresses as project X's (SPEC-028; mirrors
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
	// every tenant's addresses (SPEC-028). The slice keeps the WHERE layout
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

// CreateFloatingIP allocates a new floating IP
func (svc *Service) CreateFloatingIP(c *gin.Context) {
	var req CreateFloatingIPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	floatingIPID := uuid.New().String()

	// A floating IP is an address on the deployment's boundary network, so its network must actually
	// be one. Any network was previously accepted (F11, FR-035); pre-migration allocations are
	// grandfathered because only new requests are checked (FR-035a).
	if !requireExternalNetwork(c, svc.pool, req.FloatingIP.FloatingNetworkID, "a floating-IP pool") {
		return
	}

	// Get external network subnet to allocate IP from
	var subnetCIDR string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		req.FloatingIP.FloatingNetworkID,
	).Scan(&subnetCIDR)

	if errors.Is(err, pgx.ErrNoRows) {
		// No subnet exists - use default external IP pool (RFC 5737 TEST-NET-1)
		subnetCIDR = "192.0.2.0/24"
	} else if err != nil {
		log.Error().Err(err).Str("operation", "create_floatingip_subnet_lookup").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to allocate floating IP"))
		return
	}

	// Allocate an IP from the external network subnet
	floatingIP, err := svc.allocateFloatingIP(c.Request.Context(), subnetCIDR)
	if err != nil {
		log.Error().Err(err).Str("operation", "allocate_floatingip").Msg("failed to allocate floating IP")
		common.SendError(c, common.NewInternalServerError("failed to allocate floating IP"))
		return
	}

	// Determine status and associated details
	status := "DOWN"
	var fixedIP, portID, routerID *string

	if req.FloatingIP.PortID != "" {
		// Associate with port
		portID = &req.FloatingIP.PortID
		status = "ACTIVE"

		// Get fixed IP from port
		var fixedIPsJSON string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT fixed_ips FROM ports WHERE id = $1 AND project_id = $2",
			req.FloatingIP.PortID, projectID,
		).Scan(&fixedIPsJSON)

		if err != nil {
			common.SendError(c, common.NewNotFoundError("port"))
			return
		}

		// Parse fixed IPs and get the first one
		var fixedIPAddr string
		if req.FloatingIP.FixedIPAddress != "" {
			fixedIPAddr = req.FloatingIP.FixedIPAddress
		} else {
			// Extract from fixed_ips JSON
			var fixedIPs []map[string]any
			if err := json.Unmarshal([]byte(fixedIPsJSON), &fixedIPs); err != nil {
				log.Error().Err(err).Str("operation", "parse_port_fixed_ips").Msg("failed to parse port fixed_ips")
				common.SendError(c, common.NewInternalServerError("failed to parse port fixed IPs"))
				return
			}

			if len(fixedIPs) == 0 {
				common.SendError(c, common.NewBadRequestError("port has no fixed IP addresses"))
				return
			}

			// Use the first fixed IP
			if ipAddr, ok := fixedIPs[0]["ip_address"].(string); ok {
				fixedIPAddr = ipAddr
			} else {
				log.Error().Str("operation", "parse_port_fixed_ips").Msg("invalid fixed_ips format")
				common.SendError(c, common.NewInternalServerError("invalid fixed_ips format"))
				return
			}
		}
		fixedIP = &fixedIPAddr

		// Get router ID from port's network. A scan failure leaves
		// networkID empty, which causes the subsequent router lookup to
		// miss — the API then reports no available router, which is the
		// right answer when the port can't be resolved.
		var networkID string
		if err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT network_id FROM ports WHERE id = $1",
			req.FloatingIP.PortID,
		).Scan(&networkID); err != nil {
			log.Warn().Err(err).Str("port_id", req.FloatingIP.PortID).Msg("failed to resolve port network for floating IP association")
		}

		// Find router with external gateway and interface to this network
		var rID string
		err = svc.pool.QueryRow(c.Request.Context(), `
			SELECT DISTINCT r.id
			FROM routers r
			JOIN router_interfaces ri ON ri.router_id = r.id
			JOIN ports p ON p.id = ri.port_id
			WHERE p.network_id = $1 AND r.external_gateway_info IS NOT NULL
			LIMIT 1
		`, networkID).Scan(&rID)

		if err == nil {
			routerID = &rID

			// Program the floating-IP NAT in the default namespace, out the node
			// uplink (single-node WS-3; the per-router-netns path never sees
			// flat-ns VM traffic).
			if uplink, uerr := networking.DefaultUplinkInterface(); uerr == nil {
				if err := svc.routerManager.EnsureFloatingIP(floatingIP, *fixedIP, uplink); err != nil {
					log.Warn().Err(err).Str("floating_ip", floatingIP).Msg("failed to configure floating IP NAT")
				}
				if err := svc.ensureIngressFilter(c.Request.Context(), *fixedIP, uplink, req.FloatingIP.PortID); err != nil {
					log.Warn().Err(err).Str("fixed_ip", *fixedIP).Msg("WS-3: failed to program ingress filter")
				}
			} else {
				log.Warn().Err(uerr).Msg("WS-3: cannot detect uplink for floating IP NAT")
			}
		}
	}

	// Insert into database
	now := time.Now()
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, floatingIPID, projectID, req.FloatingIP.FloatingNetworkID, floatingIP, fixedIP, portID, routerID, status, req.FloatingIP.Description, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_floatingip").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create floating IP"))
		return
	}

	result := gin.H{
		"id":                  floatingIPID,
		"tenant_id":           projectID,
		"project_id":          projectID,
		"floating_network_id": req.FloatingIP.FloatingNetworkID,
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

// GetFloatingIP retrieves a single floating IP
func (svc *Service) GetFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	var fip FloatingIP
	var fixedIP, portID, routerID, description sql.NullString

	// id::text comparison resolves by UUID or floating-IP address without
	// crashing on a non-UUID identifier — the CLI looks a FIP up by its address
	// (e.g. `server add floating ip <vm> <address>`). An administrator may
	// show any floating IP (upstream's get_floatingip policy is
	// admin_or_owner), so the tenant-filtered listing's rows do not dead-end
	// their follow-up show with 404 — the same carve-out the other reads got
	// (SPEC-028). A non-admin keeps the project scope.
	fipQuery := `
		SELECT id, project_id, floating_network_id, floating_ip_address,
		       fixed_ip_address, port_id, router_id, status, description,
		       created_at, updated_at
		FROM floating_ips
		WHERE (id::text = $1 OR floating_ip_address = $1)`
	fipArgs := []any{floatingIPID}
	if !common.HasRole(c, common.RoleAdmin) {
		fipQuery += ` AND project_id = $2`
		fipArgs = append(fipArgs, projectID)
	}
	err := svc.pool.QueryRow(c.Request.Context(), fipQuery+" LIMIT 1", fipArgs...).
		Scan(&fip.ID, &fip.ProjectID, &fip.FloatingNetworkID, &fip.FloatingIPAddress,
			&fixedIP, &portID, &routerID, &fip.Status, &description, &fip.CreatedAt, &fip.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
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

// UpdateFloatingIP updates a floating IP (associate/disassociate with port)
func (svc *Service) UpdateFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	// Parse raw JSON to detect if port_id key is present
	var rawReq map[string]map[string]any
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Get current floating IP details
	var currentFloatingIP, currentFixedIP, currentRouterID sql.NullString
	var currentPortID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT floating_ip_address, fixed_ip_address, port_id, router_id FROM floating_ips WHERE id = $1 AND project_id = $2",
		floatingIPID, projectID,
	).Scan(&currentFloatingIP, &currentFixedIP, &currentPortID, &currentRouterID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("floating IP"))
		return
	}

	u := &common.SQLUpdate{}

	// Handle port association/disassociation
	if floatingIPData, ok := rawReq["floatingip"]; ok {
		if portIDValue, hasPortID := floatingIPData["port_id"]; hasPortID {
			// port_id key is present (could be null or a string)
			shouldDisassociate := portIDValue == nil

			// Remove old NAT rules if there was a previous association.
			// Best-effort: the new association/disassociation still
			// proceeds even if cleanup fails — leftover NAT rules will be
			// overwritten or pruned, not duplicated.
			if currentPortID.Valid && currentFixedIP.Valid {
				if uplink, uerr := networking.DefaultUplinkInterface(); uerr == nil {
					if err := svc.routerManager.WithdrawFloatingIP(currentFloatingIP.String, currentFixedIP.String, uplink); err != nil {
						log.Warn().Err(err).Str("floating_ip", currentFloatingIP.String).Msg("failed to remove previous floating IP NAT rules")
					}
					_ = svc.routerManager.RemoveIngressFilter(currentFixedIP.String, uplink)
				}
			}

			if shouldDisassociate {
				// Disassociate (null value)
				u.Set("port_id", nil)

				u.Set("fixed_ip_address", nil)

				u.Set("router_id", nil)

				u.Set("status", "DOWN")
			} else {
				// Associate with new port
				newPortID, ok := portIDValue.(string)
				if !ok {
					common.SendError(c, common.NewBadRequestError("port_id must be a string or null"))
					return
				}
				// Associate with new port. A scan failure leaves
				// networkID empty, which causes the subsequent router
				// lookup to miss — the API then reports no available
				// router, which is the right answer when the port can't
				// be resolved.
				var networkID string
				if err := svc.pool.QueryRow(c.Request.Context(),
					"SELECT network_id FROM ports WHERE id = $1",
					newPortID,
				).Scan(&networkID); err != nil {
					log.Warn().Err(err).Str("port_id", newPortID).Msg("failed to resolve port network during floating IP reassociation")
				}

				// Find router
				var routerID string
				err = svc.pool.QueryRow(c.Request.Context(), `
				SELECT DISTINCT r.id
				FROM routers r
				JOIN router_interfaces ri ON ri.router_id = r.id
				JOIN ports p ON p.id = ri.port_id
				WHERE p.network_id = $1 AND r.external_gateway_info IS NOT NULL
				LIMIT 1
			`, networkID).Scan(&routerID)

				if err != nil {
					common.SendError(c, common.NewBadRequestError("no router with external gateway found for this network"))
					return
				}

				// Resolve the fixed IP: explicit request value, else the port's
				// primary fixed IP from the DB (the bogus hard-coded default
				// never matched a real VM).
				var fixedIP string
				if fixedIPAddr, ok := floatingIPData["fixed_ip_address"]; ok && fixedIPAddr != nil {
					fixedIP, _ = fixedIPAddr.(string)
				}
				if fixedIP == "" {
					fixedIP = svc.portPrimaryFixedIP(c.Request.Context(), newPortID)
				}
				if fixedIP == "" {
					common.SendError(c, common.NewBadRequestError("could not determine the port's fixed IP"))
					return
				}

				// Program the floating-IP NAT in the default namespace, out the
				// node uplink (single-node WS-3).
				uplink, uerr := networking.DefaultUplinkInterface()
				if uerr != nil {
					log.Error().Err(uerr).Msg("WS-3: cannot detect uplink for floating IP NAT")
					common.SendError(c, common.NewInternalServerError("failed to configure NAT"))
					return
				}
				if err := svc.routerManager.EnsureFloatingIP(currentFloatingIP.String, fixedIP, uplink); err != nil {
					log.Error().Err(err).Str("operation", "configure_fip_nat").Msg("failed to configure NAT rules")
					common.SendError(c, common.NewInternalServerError("failed to configure NAT"))
					return
				}
				// Default-deny inbound, open per the port's SG ingress rules (T034).
				if err := svc.ensureIngressFilter(c.Request.Context(), fixedIP, uplink, newPortID); err != nil {
					log.Warn().Err(err).Str("fixed_ip", fixedIP).Msg("WS-3: failed to program ingress filter")
				}

				u.Set("port_id", newPortID)

				u.Set("fixed_ip_address", fixedIP)

				u.Set("router_id", routerID)

				u.Set("status", "ACTIVE")
			}
		}

		// Handle description update
		if descValue, hasDesc := floatingIPData["description"]; hasDesc {
			if desc, ok := descValue.(string); ok {
				u.Set("description", desc)
			}
		}
	}

	if u.Empty() {
		// No updates, just return current state
		svc.GetFloatingIP(c)
		return
	}

	u.Set("updated_at", time.Now())

	query := "UPDATE floating_ips SET " + u.Assignments() +
		" WHERE id = " + u.Arg(floatingIPID) + " AND project_id = " + u.Arg(projectID)

	_, err = svc.pool.Exec(c.Request.Context(), query, u.Args()...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_floatingip").Str("floatingip_id", floatingIPID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update floating IP"))
		return
	}

	// Return updated floating IP
	svc.GetFloatingIP(c)
}

// DeleteFloatingIP releases a floating IP
func (svc *Service) DeleteFloatingIP(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	// Get floating IP details before deletion
	var floatingIP, fixedIP, routerID sql.NullString
	var portID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT floating_ip_address, fixed_ip_address, port_id, router_id FROM floating_ips WHERE id = $1 AND project_id = $2",
		floatingIPID, projectID,
	).Scan(&floatingIP, &fixedIP, &portID, &routerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("floating IP"))
		return
	}

	// Remove NAT rules if associated
	if portID.Valid && fixedIP.Valid {
		if uplink, uerr := networking.DefaultUplinkInterface(); uerr == nil {
			if err := svc.routerManager.WithdrawFloatingIP(floatingIP.String, fixedIP.String, uplink); err != nil {
				log.Warn().Err(err).Str("floatingip_id", floatingIPID).Str("floating_ip", floatingIP.String).Msg("failed to remove floating IP NAT rules")
			}
			_ = svc.routerManager.RemoveIngressFilter(fixedIP.String, uplink)
		}
	}

	// Delete from database
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM floating_ips WHERE id = $1 AND project_id = $2",
		floatingIPID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_floatingip").Str("floatingip_id", floatingIPID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete floating IP"))
		return
	}

	c.Status(http.StatusNoContent)
}

// portPrimaryFixedIP returns a port's primary fixed IP (first entry of the
// fixed_ips JSONB array), or "" if unavailable — used to DNAT a floating IP to
// the right VM address.
func (svc *Service) portPrimaryFixedIP(ctx context.Context, portID string) string {
	var raw []byte
	if err := svc.pool.QueryRow(ctx, `SELECT fixed_ips FROM ports WHERE id = $1`, portID).Scan(&raw); err != nil {
		return ""
	}
	var fixedIPs []struct {
		IPAddress string `json:"ip_address"`
	}
	if err := json.Unmarshal(raw, &fixedIPs); err != nil {
		return ""
	}
	for _, f := range fixedIPs {
		if f.IPAddress != "" {
			return f.IPAddress
		}
	}
	return ""
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

// ensureIngressFilter programs a port's ingress filter, skipping the update when the
// rule set could not be read in full (fail-safe: keep the filter that is already there).
func (svc *Service) ensureIngressFilter(ctx context.Context, fixedIP, uplink, portID string) error {
	rules, err := svc.portIngressRules(ctx, portID)
	if err != nil {
		return err
	}
	return svc.routerManager.EnsureIngressFilter(fixedIP, uplink, rules)
}

// refreshIngressFiltersForSG rebuilds the north-south ingress filter for every
// floating-IP'd port that uses the given security group, so an SG rule change
// takes effect immediately (T034).
func (svc *Service) refreshIngressFiltersForSG(ctx context.Context, sgID string) {
	if svc.mode == "stub" {
		return
	}
	rows, err := svc.pool.Query(ctx, `
		SELECT f.fixed_ip_address, f.port_id
		FROM floating_ips f
		JOIN port_security_groups psg ON psg.port_id = f.port_id
		WHERE psg.security_group_id = $1 AND f.port_id IS NOT NULL AND f.fixed_ip_address IS NOT NULL`, sgID)
	if err != nil {
		log.Warn().Err(err).Str("sg_id", sgID).Msg("WS-3: failed to query FIPs for SG refresh")
		return
	}
	type target struct{ fixed, port string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.fixed, &t.port); err != nil {
			log.Warn().Err(err).Str("operation", "refreshIngressFiltersForSG").Msg("failed to scan row")
		} else {
			targets = append(targets, t)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Error().Err(err).Str("sg_id", sgID).Msg("WS-3: FIP rows iteration error; skipping SG ingress refresh")
		return
	}
	rows.Close()
	if len(targets) == 0 {
		return
	}
	uplink, err := networking.DefaultUplinkInterface()
	if err != nil {
		log.Warn().Err(err).Msg("WS-3: no uplink for SG ingress refresh")
		return
	}
	for _, t := range targets {
		if err := svc.ensureIngressFilter(ctx, t.fixed, uplink, t.port); err != nil {
			log.Warn().Err(err).Str("fixed_ip", t.fixed).Msg("WS-3: ingress filter refresh failed")
		}
	}
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
