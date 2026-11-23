package neutron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreatePortForwardingRequest represents a port forwarding creation request
type CreatePortForwardingRequest struct {
	PortForwarding struct {
		InternalPortID    string `json:"internal_port_id" binding:"required"`
		InternalIPAddress string `json:"internal_ip_address" binding:"required"`
		ExternalPort      int    `json:"external_port" binding:"required"`
		InternalPort      int    `json:"internal_port" binding:"required"`
		Protocol          string `json:"protocol" binding:"required"`
		Description       string `json:"description"`
	} `json:"port_forwarding"`
}

// UpdatePortForwardingRequest represents a port forwarding update request
type UpdatePortForwardingRequest struct {
	PortForwarding struct {
		InternalPortID    *string `json:"internal_port_id"`
		InternalIPAddress *string `json:"internal_ip_address"`
		InternalPort      *int    `json:"internal_port"`
		Description       *string `json:"description"`
	} `json:"port_forwarding"`
}

// PortForwarding represents a port forwarding rule
type PortForwarding struct {
	ID                string
	ProjectID         string
	FloatingIPID      string
	InternalPortID    string
	InternalIPAddress string
	ExternalPort      int
	InternalPort      int
	Protocol          string
	Status            string
	Description       sql.NullString
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ListPortForwardings handles GET /v2.0/floatingips/:id/port_forwardings: the forwardings of a
// floating IP of the caller's project, of any project's for an administrator, narrowed by
// project_id/tenant_id (SPEC-033 FR-057).
func (svc *Service) ListPortForwardings(c *gin.Context) {
	floatingIPID := c.Param("id")
	projectID := c.GetString("project_id")

	// Verify floating IP exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM floating_ips WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+")",
		floatingIPID, projectID, isAdmin(c),
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("floating IP"))
		return
	}

	// List port forwardings
	f := &common.SQLFilter{}
	f.Eq("floatingip_id", floatingIPID)
	projectScope(c, f, "project_id", ownedBy("project_id"))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, project_id, floatingip_id, internal_port_id, internal_ip_address,
		       external_port, internal_port, protocol, status, description,
		       created_at, updated_at
		FROM port_forwardings`+f.Where()+`
		ORDER BY external_port ASC
	`, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_port_forwardings").Msg("failed to query port forwardings")
		common.SendError(c, common.NewInternalServerError("failed to list port forwardings"))
		return
	}
	defer rows.Close()

	var forwardings []gin.H
	for rows.Next() {
		var pf PortForwarding
		if err := rows.Scan(&pf.ID, &pf.ProjectID, &pf.FloatingIPID, &pf.InternalPortID,
			&pf.InternalIPAddress, &pf.ExternalPort, &pf.InternalPort, &pf.Protocol,
			&pf.Status, &pf.Description, &pf.CreatedAt, &pf.UpdatedAt); err != nil {
			continue
		}

		result := gin.H{
			"id":                  pf.ID,
			"internal_port_id":    pf.InternalPortID,
			"internal_ip_address": pf.InternalIPAddress,
			"external_port":       pf.ExternalPort,
			"internal_port":       pf.InternalPort,
			"protocol":            pf.Protocol,
			"created_at":          pf.CreatedAt.Format(time.RFC3339),
			"updated_at":          pf.UpdatedAt.Format(time.RFC3339),
		}

		if pf.Description.Valid {
			result["description"] = pf.Description.String
		}

		forwardings = append(forwardings, result)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if forwardings == nil {
		forwardings = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"port_forwardings": forwardings})
}

// CreatePortForwarding handles POST /v2.0/floatingips/:id/port_forwardings. One transaction checks and
// stores it: the floating IP, of the caller's project or any for an administrator, and its router; the
// internal port, of the floating IP's project, holding the internal address; no duplicate. The router
// is then converged on the network node before the answer; when that fails, the forwarding is removed
// again, compare-and-set, and the request answers 500 (SPEC-033 FR-045, FR-049; research R37).
func (svc *Service) CreatePortForwarding(c *gin.Context) {
	floatingIPID := c.Param("id")

	var req CreatePortForwardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	pf := req.PortForwarding
	protocol := strings.ToLower(pf.Protocol)
	if protocol != "tcp" && protocol != "udp" {
		common.SendError(c, common.NewBadRequestError("protocol must be tcp or udp"))
		return
	}
	if pf.ExternalPort < 1 || pf.ExternalPort > 65535 {
		common.SendError(c, common.NewBadRequestError("external_port must be between 1 and 65535"))
		return
	}
	if pf.InternalPort < 1 || pf.InternalPort > 65535 {
		common.SendError(c, common.NewBadRequestError("internal_port must be between 1 and 65535"))
		return
	}

	ctx := c.Request.Context()
	pfID := uuid.New().String()
	now := time.Now()
	var routerID string
	var bound bool // this create recorded the router on the floating IP
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		// Upstream's order (pf_plugin): the floating IP, read unlocked — its network and project never
		// change — then the internal port and its checks, then the router, locked, and only then the
		// floating IP, locked: a floating IP's association takes the router before the floating IP too
		// (research R20; SPEC-033 T192).
		fip, err := readForwardingFloatingIP(ctx, tx, floatingIPID, scopeOf(c), false)
		if err != nil {
			return err
		}
		port, err := checkInternalPort(ctx, tx, fip, pf.InternalPortID, pf.InternalIPAddress)
		if err != nil {
			return err
		}
		if fip.port != "" {
			return errFloatingIPPortAlreadyAssociated(fip.address, fip.id, port.id, port.fixedIP, fip.network)
		}
		router, err := forwardingRouter(ctx, tx, fip, port)
		if err != nil {
			return err
		}
		locked, err := readForwardingFloatingIP(ctx, tx, floatingIPID, scopeOf(c), true)
		if err != nil {
			return err
		}
		if locked.port != "" {
			return errFloatingIPPortAlreadyAssociated(locked.address, locked.id, port.id, port.fixedIP, locked.network)
		}
		if err := requireRouterMatch(locked, port, router); err != nil {
			return err
		}
		if locked.router == "" {
			if _, err := tx.Exec(ctx, "UPDATE floating_ips SET router_id = $1, status = 'ACTIVE', updated_at = $2 WHERE id = $3",
				router, now, locked.id); err != nil {
				return err
			}
			bound = true
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id,
				internal_ip_address, external_port, internal_port, protocol, status, description,
				created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			pfID, fip.owner, floatingIPID, pf.InternalPortID, pf.InternalIPAddress, pf.ExternalPort,
			pf.InternalPort, protocol, "ACTIVE", pf.Description, now, now)
		if database.IsUniqueViolation(err) {
			return common.NewConflictError(fmt.Sprintf("port forwarding for %s:%d already exists", protocol, pf.ExternalPort))
		}
		routerID = router
		return err
	})
	if err != nil {
		sendChangeError(c, err, "create", "port forwarding")
		return
	}

	// Stored. From here the change is applied or undone, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	if err := svc.convergeRouterUnit(cctx, routerID); err != nil {
		if uerr := database.WithTx(cctx, svc.pool, func(tx database.Tx) error {
			if _, err := tx.Exec(cctx, `
				DELETE FROM port_forwardings WHERE id = $1 AND internal_port_id::text = $2 AND internal_ip_address = $3
				   AND internal_port = $4`, pfID, pf.InternalPortID, pf.InternalIPAddress, pf.InternalPort); err != nil {
				return err
			}
			if !bound {
				return nil
			}
			return releaseForwardingRouters(cctx, tx, []string{floatingIPID})
		}); uerr != nil {
			log.Error().Err(uerr).Str("pf_id", pfID).Msg("remove a port forwarding the network node could not apply; the reconciliation pass applies its record")
		}
		if cerr := svc.convergeRouterUnit(cctx, routerID); cerr != nil {
			log.Error().Err(cerr).Str("router_id", routerID).Msg("converge a router after an undo; the reconciliation pass converges it")
		}
		log.Error().Err(err).Str("pf_id", pfID).Msg("the network node could not apply the port forwarding; the change was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the port forwarding"))
		return
	}

	result := gin.H{
		"id":                  pfID,
		"internal_port_id":    pf.InternalPortID,
		"internal_ip_address": pf.InternalIPAddress,
		"external_port":       pf.ExternalPort,
		"internal_port":       pf.InternalPort,
		"protocol":            protocol,
		"created_at":          now.Format(time.RFC3339),
		"updated_at":          now.Format(time.RFC3339),
	}
	if pf.Description != "" {
		result["description"] = pf.Description
	}
	c.JSON(http.StatusCreated, gin.H{"port_forwarding": result})
}

// forwardingFloatingIP is a floating IP as a change to its port forwardings reads it.
type forwardingFloatingIP struct {
	id, owner, network, address string
	port, router                string // "" for none
}

// readForwardingFloatingIP reads a floating IP for a change to its port forwardings — the caller's
// project's, any project's for an administrator (FR-058) — locking it when forUpdate.
func readForwardingFloatingIP(ctx context.Context, tx database.Tx, floatingIPID string, caller callerScope, forUpdate bool) (forwardingFloatingIP, error) {
	query := `
		SELECT id::text, project_id::text, floating_network_id::text, floating_ip_address,
		       COALESCE(port_id::text, ''), COALESCE(router_id::text, '')
		  FROM floating_ips WHERE id::text = $1 AND ` + ownerOrAdmin("project_id", "$2", "$3")
	if forUpdate {
		query += " FOR UPDATE"
	}
	var f forwardingFloatingIP
	err := tx.QueryRow(ctx, query, floatingIPID, caller.project, caller.admin).Scan(&f.id, &f.owner, &f.network, &f.address, &f.port, &f.router)
	if errors.Is(err, database.ErrNoRows) {
		return f, common.NewNotFoundError("floating IP")
	}
	if err != nil {
		return f, fmt.Errorf("read floating IP %s: %w", floatingIPID, err)
	}
	return f, nil
}

// forwardingRouter is the router a port forwarding on fip to port goes through, upstream's
// _find_a_router_for_fip_port_forwarding: the one a floating IP on the internal port would go through,
// locked (routerForFloatingIP). None is upstream's 400. The router the floating IP records, once a
// forwarding recorded it, must be this one (requireRouterMatch).
func forwardingRouter(ctx context.Context, tx database.Tx, fip forwardingFloatingIP, port floatingPort) (string, error) {
	router, err := routerForFloatingIP(ctx, tx, port, fip.network)
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) && refusal.NeutronType == "ExternalGatewayForFloatingIPNotFound" {
		return "", badRequest("port_forwarding", fmt.Sprintf(
			"External network %s is not reachable from subnet %s. Cannot set Port forwarding for Port %s with Floating IP %s",
			fip.network, port.subnetID, port.id, fip.id))
	}
	return router, err
}

// checkInternalPort makes upstream's checks of a port forwarding's internal port: the floating IP's
// project's port holding the address (requireInternalAddress), with no floating IP of its own
// (refusePortWithFloatingIP).
func checkInternalPort(ctx context.Context, tx database.Tx, fip forwardingFloatingIP, portID, address string) (floatingPort, error) {
	port, err := requireInternalAddress(ctx, tx, portID, fip.owner, address)
	if err != nil {
		return port, err
	}
	return port, refusePortWithFloatingIP(ctx, tx, port)
}

// checkForwardingMove checks an update that moves a port forwarding to another internal port or address
// as its create checks one, as upstream's update_floatingip_port_forwarding: the internal port, and the
// router the floating IP records reaching it (SPEC-033 FR-049, T256). fip is locked.
func checkForwardingMove(ctx context.Context, tx database.Tx, fip forwardingFloatingIP, portID, address string) error {
	port, err := checkInternalPort(ctx, tx, fip, portID, address)
	if err != nil {
		return err
	}
	router, err := forwardingRouter(ctx, tx, fip, port)
	if err != nil {
		return err
	}
	return requireRouterMatch(fip, port, router)
}

// requireRouterMatch is upstream's _check_router_match: a floating IP whose forwardings recorded a router
// takes another forwarding only to a port that router reaches (400).
func requireRouterMatch(fip forwardingFloatingIP, port floatingPort, router string) error {
	if fip.router != "" && fip.router != router {
		return badRequest("port_forwarding", fmt.Sprintf("The floating IP %s had been set on router %s, the internal Neutron port %s can not reach it",
			fip.id, fip.router, port.id))
	}
	return nil
}

// refusePortWithFloatingIP refuses an internal port with a floating IP of its own, upstream's
// PortHasBindingFloatingIP.
func refusePortWithFloatingIP(ctx context.Context, tx database.Tx, port floatingPort) error {
	var id, address string
	err := tx.QueryRow(ctx, "SELECT id::text, floating_ip_address FROM floating_ips WHERE port_id::text = $1 ORDER BY id LIMIT 1", port.id).Scan(&id, &address)
	if errors.Is(err, database.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the floating IPs of port %s: %w", port.id, err)
	}
	return common.NewNeutronError(http.StatusConflict, "PortHasBindingFloatingIP",
		fmt.Sprintf("Cannot create port forwarding to floating IP %s (%s) with port %s using fixed IP %s, as that port already has a binding floating IP.",
			address, id, port.id, port.fixedIP))
}

// releaseForwardingRouters frees each of floatingIPs left with no port forwarding and no port: its
// forwardings recorded the router on it, and upstream clears it with the last (SPEC-033 T192).
func releaseForwardingRouters(ctx context.Context, tx database.Tx, floatingIPs []string) error {
	if len(floatingIPs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE floating_ips SET router_id = NULL, status = 'DOWN', updated_at = $2
		 WHERE id::text = ANY($1) AND port_id IS NULL AND router_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM port_forwardings pf WHERE pf.floatingip_id = floating_ips.id)`, floatingIPs, time.Now())
	if err != nil {
		return fmt.Errorf("free the floating IPs %v of their routers: %w", floatingIPs, err)
	}
	return nil
}

// deletePortForwardingsOf deletes the port forwardings to ports that go, as their rows would go with
// them, and frees the floating IPs they leave without one (releaseForwardingRouters), as upstream's
// port deletion does.
func deletePortForwardingsOf(ctx context.Context, tx database.Tx, ports []string) error {
	if len(ports) == 0 {
		return nil
	}
	floatingIPs, err := queryStrings(ctx, tx,
		"SELECT DISTINCT floatingip_id::text FROM port_forwardings WHERE internal_port_id::text = ANY($1)", ports)
	if err != nil {
		return fmt.Errorf("read the port forwardings of ports %v: %w", ports, err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM port_forwardings WHERE internal_port_id::text = ANY($1)", ports); err != nil {
		return fmt.Errorf("delete the port forwardings of ports %v: %w", ports, err)
	}
	return releaseForwardingRouters(ctx, tx, floatingIPs)
}

// requireInternalAddress refuses an internal port that is not the floating IP's project's, or that
// does not hold the internal address, and returns the port as a floating IP reaches it.
func requireInternalAddress(ctx context.Context, tx database.Tx, portID, project, address string) (floatingPort, error) {
	p := floatingPort{id: portID}
	var fixed common.FixedIPs
	err := tx.QueryRow(ctx, "SELECT COALESCE(network_id::text, ''), fixed_ips FROM ports WHERE id::text = $1 AND project_id::text = $2", portID, project).Scan(&p.networkID, &fixed)
	if errors.Is(err, database.ErrNoRows) {
		return p, common.NewNotFoundError("port")
	}
	if err != nil {
		return p, fmt.Errorf("read port %s: %w", portID, err)
	}
	for _, f := range fixed {
		if f.IPAddress == address {
			p.subnetID, p.fixedIP = f.SubnetID, f.IPAddress
			return p, nil
		}
	}
	return p, common.NewBadRequestError(fmt.Sprintf("port %s does not have fixed IP %s", portID, address))
}

// GetPortForwarding handles GET /v2.0/floatingips/:id/port_forwardings/:pf_id
func (svc *Service) GetPortForwarding(c *gin.Context) {
	floatingIPID := c.Param("id")
	pfID := c.Param("pf_id")
	projectID := c.GetString("project_id")

	var pf PortForwarding
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, project_id, floatingip_id, internal_port_id, internal_ip_address,
		       external_port, internal_port, protocol, status, description,
		       created_at, updated_at
		FROM port_forwardings
		WHERE id = $1 AND floatingip_id = $2 AND `+ownerOrAdmin("project_id", "$3", "$4")+`
	`, pfID, floatingIPID, projectID, isAdmin(c)).Scan(&pf.ID, &pf.ProjectID, &pf.FloatingIPID,
		&pf.InternalPortID, &pf.InternalIPAddress, &pf.ExternalPort, &pf.InternalPort,
		&pf.Protocol, &pf.Status, &pf.Description, &pf.CreatedAt, &pf.UpdatedAt)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port forwarding"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_port_forwarding").Msg("failed to get port forwarding")
		common.SendError(c, common.NewInternalServerError("failed to get port forwarding"))
		return
	}

	result := gin.H{
		"id":                  pf.ID,
		"internal_port_id":    pf.InternalPortID,
		"internal_ip_address": pf.InternalIPAddress,
		"external_port":       pf.ExternalPort,
		"internal_port":       pf.InternalPort,
		"protocol":            pf.Protocol,
		"created_at":          pf.CreatedAt.Format(time.RFC3339),
		"updated_at":          pf.UpdatedAt.Format(time.RFC3339),
	}

	if pf.Description.Valid {
		result["description"] = pf.Description.String
	}

	c.JSON(http.StatusOK, gin.H{"port_forwarding": result})
}

// UpdatePortForwarding handles PUT /v2.0/floatingips/:id/port_forwardings/:pf_id. One transaction
// locks the forwarding, checks a new internal port and address as the create does, and stores the
// change. A change to where it forwards then converges the router on the network node before the
// answer; when that fails, the old values are put back, compare-and-set, and the request answers 500
// (SPEC-033 FR-045, FR-049).
func (svc *Service) UpdatePortForwarding(c *gin.Context) {
	floatingIPID := c.Param("id")
	pfID := c.Param("pf_id")

	var req UpdatePortForwardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	change := req.PortForwarding
	if change.InternalPort != nil && (*change.InternalPort < 1 || *change.InternalPort > 65535) {
		common.SendError(c, common.NewBadRequestError("internal_port must be between 1 and 65535"))
		return
	}

	ctx := c.Request.Context()
	type target struct {
		port, address string
		number        int
	}
	var old, written target
	var routerID string
	moves := false
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		fip, err := readForwardingFloatingIP(ctx, tx, floatingIPID, scopeOf(c), true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			SELECT internal_port_id::text, internal_ip_address, internal_port FROM port_forwardings
			 WHERE id::text = $1 AND floatingip_id::text = $2 FOR UPDATE`, pfID, floatingIPID,
		).Scan(&old.port, &old.address, &old.number)
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("port forwarding")
		}
		if err != nil {
			return fmt.Errorf("lock port forwarding %s: %w", pfID, err)
		}
		written = old
		u := &common.SQLUpdate{}
		if change.InternalPortID != nil {
			written.port = *change.InternalPortID
			u.Set("internal_port_id", written.port)
		}
		if change.InternalIPAddress != nil {
			written.address = *change.InternalIPAddress
			u.Set("internal_ip_address", written.address)
		}
		if change.InternalPort != nil {
			written.number = *change.InternalPort
			u.Set("internal_port", written.number)
		}
		if change.Description != nil {
			u.Set("description", *change.Description)
		}
		if u.Empty() {
			return nil
		}
		moves = written != old
		if written.port != old.port || written.address != old.address {
			if err := checkForwardingMove(ctx, tx, fip, written.port, written.address); err != nil {
				return err
			}
		}
		u.Set("updated_at", time.Now())
		routerID = fip.router
		_, err = tx.Exec(ctx, "UPDATE port_forwardings SET "+u.Assignments()+" WHERE id = "+u.Arg(pfID), u.Args()...)
		return err
	})
	if err != nil {
		sendChangeError(c, err, "update", "port forwarding")
		return
	}

	if moves && routerID != "" {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		cctx := context.WithoutCancel(ctx)
		if err := svc.convergeRouterUnit(cctx, routerID); err != nil {
			if _, uerr := svc.pool.Exec(cctx, `
				UPDATE port_forwardings SET internal_port_id = $2, internal_ip_address = $3, internal_port = $4
				 WHERE id = $1 AND internal_port_id::text = $5 AND internal_ip_address = $6 AND internal_port = $7`,
				pfID, old.port, old.address, old.number, written.port, written.address, written.number); uerr != nil {
				log.Error().Err(uerr).Str("pf_id", pfID).Msg("put a port forwarding back; the reconciliation pass applies its record")
			}
			if cerr := svc.convergeRouterUnit(cctx, routerID); cerr != nil {
				log.Error().Err(cerr).Str("router_id", routerID).Msg("converge a router after an undo; the reconciliation pass converges it")
			}
			log.Error().Err(err).Str("pf_id", pfID).Msg("the network node could not apply the port forwarding's change; it was not applied")
			common.SendError(c, common.NewInternalServerError("failed to apply the port forwarding"))
			return
		}
	}
	svc.GetPortForwarding(c)
}

// DeletePortForwarding handles DELETE /v2.0/floatingips/:id/port_forwardings/:pf_id, in FR-040's
// order: the forwarding is found, its router converged without it on the network node, and only then
// its record deleted. When the router cannot be converged, or the deletion fails, the record stays, the
// router is converged from it again, and the request answers 500 (SPEC-033 FR-040, research R34).
func (svc *Service) DeletePortForwarding(c *gin.Context) {
	floatingIPID := c.Param("id")
	pfID := c.Param("pf_id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var router sql.NullString
	err := svc.pool.QueryRow(ctx, `
		SELECT fi.router_id::text FROM port_forwardings pf JOIN floating_ips fi ON pf.floatingip_id = fi.id
		 WHERE pf.id::text = $1 AND pf.floatingip_id::text = $2 AND `+ownerOrAdmin("pf.project_id", "$3", "$4"),
		pfID, floatingIPID, projectID, isAdmin(c)).Scan(&router)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port forwarding"))
		return
	}
	if err != nil {
		sendChangeError(c, err, "delete", "port forwarding")
		return
	}

	// From here the removal is applied or put back, whether or not the client is still there.
	cctx := context.WithoutCancel(ctx)
	reapply := func(cause error, message string) {
		if router.Valid {
			if cerr := svc.convergeRouterUnit(cctx, router.String); cerr != nil {
				log.Error().Err(cerr).Str("router_id", router.String).Msg("converge a router from its records again; the reconciliation pass converges it")
			}
		}
		log.Error().Err(cause).Str("pf_id", pfID).Msg(message)
		common.SendError(c, common.NewInternalServerError("failed to delete port forwarding"))
	}
	if router.Valid {
		if err := svc.convergeRouterUnit(cctx, router.String, pfID); err != nil {
			reapply(err, "the network node could not remove the port forwarding; it was kept")
			return
		}
	}
	deleted := false
	err = database.WithTx(cctx, svc.pool, func(tx database.Tx) error {
		tag, err := tx.Exec(cctx, "DELETE FROM port_forwardings WHERE id::text = $1 AND floatingip_id::text = $2", pfID, floatingIPID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		deleted = true
		return releaseForwardingRouters(cctx, tx, []string{floatingIPID})
	})
	if err != nil {
		reapply(err, "the port forwarding's record could not be deleted; it was kept")
		return
	}
	if !deleted {
		common.SendError(c, common.NewNotFoundError("port forwarding")) // deleted meanwhile
		return
	}
	c.Status(http.StatusNoContent)
}
