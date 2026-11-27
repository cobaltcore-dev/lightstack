package neutron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreatePortRequest represents a port creation request
type CreatePortRequest struct {
	Port struct {
		Name         string `json:"name"`
		NetworkID    string `json:"network_id" binding:"required"`
		AdminStateUp *bool  `json:"admin_state_up"`
		DeviceID     string `json:"device_id"`
		DeviceOwner  string `json:"device_owner"`
		// Read raw: absent, null and a list mean different things (parseSecurityGroupsField).
		SecurityGroups json.RawMessage `json:"security_groups"`
		// The project the port belongs to: an administrator's to name (SPEC-033 FR-058).
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
		// The addresses asked for: absent takes one of the network's first subnet, a list exactly
		// those, each naming a subnet, an address or both (FR-032, FR-053).
		FixedIPs *[]struct {
			SubnetID  string `json:"subnet_id"`
			IPAddress string `json:"ip_address"`
		} `json:"fixed_ips"`
	} `json:"port"`
}

// CreatePort creates a new port
func (svc *Service) CreatePort(c *gin.Context) {
	var req CreatePortRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// The body's groups are checked before anything is looked up, as upstream does (research R12).
	securityGroups, groupsGiven, groupsErr := parseSecurityGroupsField(req.Port.SecurityGroups)
	if groupsErr != nil {
		common.SendError(c, groupsErr)
		return
	}
	var requested []requestedFixedIP
	if req.Port.FixedIPs != nil {
		requested = []requestedFixedIP{}
		for _, f := range *req.Port.FixedIPs {
			if f.SubnetID == "" && f.IPAddress == "" {
				common.SendError(c, invalidInput("each of fixed_ips names a subnet_id, an ip_address or both"))
				return
			}
			requested = append(requested, requestedFixedIP{subnetID: f.SubnetID, ipAddress: f.IPAddress})
		}
		if refusal := duplicateAddress("fixed_ips", requested); refusal != nil {
			common.SendError(c, refusal)
			return
		}
	}

	projectID, ok := svc.ownerOf(c, req.Port.ProjectID, req.Port.TenantID)
	if !ok {
		return
	}
	portID := uuid.New().String()
	macAddress := generateMAC()

	adminStateUp := true
	if req.Port.AdminStateUp != nil {
		adminStateUp = *req.Port.AdminStateUp
	}

	// Get network and subnet info to allocate IP
	var networkID, networkProject string
	// The port's project's network, a shared one, or for an administrator any project's, by id
	// (SPEC-033 FR-058).
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, COALESCE(project_id::text, '') FROM networks WHERE id = $1 AND "+scopeOrAdmin("(project_id = $2 OR shared = true)", "$3"),
		req.Port.NetworkID, c.GetString("project_id"), isAdmin(c),
	).Scan(&networkID, &networkProject)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	// Any OTHER error had to be handled too (second review, AGY-1). A network_id that is not a UUID
	// fails the cast rather than matching no row, so it is not ErrNoRows — and going unchecked it
	// fell through with networkID empty, all the way to networking.BridgeNameForNetwork, which
	// panicked on anything shorter than 8 characters. AllocatePortForInstance, which runs the very
	// same lookup for the instance-boot path, has always checked both; the two disagreeing is the
	// asymmetry #546 noted between these paths.
	if err != nil {
		log.Warn().Err(err).Str("network_id", req.Port.NetworkID).Msg("port create: unusable network_id")
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	if refusal := networkDeviceOwnerRefusal("create_port", req.Port.DeviceOwner, networkProject, c.GetString("project_id"), isAdmin(c)); refusal != nil {
		common.SendError(c, refusal)
		return
	}

	// Get subnet to allocate IP
	var subnetID string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT id FROM subnets WHERE network_id = $1 LIMIT 1",
		req.Port.NetworkID,
	).Scan(&subnetID)

	if err != nil && !errors.Is(err, database.ErrNoRows) {
		log.Error().Err(err).Msg("port create: subnet lookup failed")
		common.SendError(c, common.NewInternalServerError("failed to look up subnet"))
		return
	}
	fixedIPs := common.FixedIPs{}

	// The port's groups: those given, each the project's, or the project's default when the field
	// is absent. null and [] give none (SPEC-033 FR-017). A trusted port has none, whatever is given,
	// as upstream (FR-072). Resolved before anything is created, so a refused group leaves nothing
	// behind.
	if trustedOwner(req.Port.DeviceOwner) {
		securityGroups = []string{}
	} else if groupsGiven {
		if err := requireGroupsOfProject(c.Request.Context(), svc.pool, securityGroups, projectID, isAdmin(c)); err != nil {
			sendPortChangeError(c, err, "create")
			return
		}
	} else {
		// OpenStack guarantees every project has a default group (Neutron creates it lazily);
		// ensure it rather than silently creating a port with none.
		defaultSGID, err := EnsureDefaultSecurityGroup(c.Request.Context(), svc.pool, projectID)
		if err != nil {
			log.Error().Err(err).Str("port_id", portID).Msg("failed to ensure default security group")
			common.SendError(c, common.NewInternalServerError("failed to ensure default security group"))
			return
		}
		securityGroups = []string{defaultSGID}
	}

	// No device is made here: libvirt makes the guest's TAP, named for the port, and the node the port
	// is bound to holds its firewall and its network's bridge (SPEC-033 FR-041, FR-045, R32).

	// An unbound port starts DOWN, as in Neutron. A port already created with a
	// device owner represents an active attachment in this stub implementation.
	portStatus := "DOWN"
	if req.Port.DeviceOwner != "" {
		portStatus = "ACTIVE"
	}

	// Insert into database
	now := time.Now()
	var created time.Time
	err = database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		switch {
		case requested != nil:
			subnets, err := networkSubnets(c.Request.Context(), tx, req.Port.NetworkID)
			if err != nil {
				return err
			}
			routerPort := req.Port.DeviceOwner == ownerRouterInterface || req.Port.DeviceOwner == ownerRouterGateway
			if fixedIPs, err = requestedAddresses(c.Request.Context(), tx, addressRequest{networkID: req.Port.NetworkID, subnets: subnets, routerPort: routerPort}, requested); err != nil {
				return err
			}
		case subnetID != "":
			if _, err := LockSubnet(c.Request.Context(), tx, subnetID); err != nil {
				return err
			}
			ip, err := AllocateFreeIP(c.Request.Context(), tx, subnetID)
			if err != nil {
				return err
			}
			fixedIPs = common.FixedIPs{{SubnetID: subnetID, IPAddress: ip}}
		}
		fixedIPsJSON, _ := json.Marshal(fixedIPs)
		_, err := tx.Exec(c.Request.Context(), `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, portID, req.Port.Name, req.Port.NetworkID, projectID, sql.NullString{String: req.Port.DeviceID, Valid: req.Port.DeviceID != ""},
			sql.NullString{String: req.Port.DeviceOwner, Valid: req.Port.DeviceOwner != ""}, macAddress, adminStateUp, portStatus, fixedIPsJSON, now, now)
		if err != nil {
			return err
		}
		// With the port, in one transaction: a port never exists with fewer groups than it reports, nor
		// without its VXLAN forwarding row (SPEC-033 FR-045).
		if err := SetPortSecurityGroups(c.Request.Context(), tx, portID, securityGroups); err != nil {
			return err
		}
		if svc.fdb != nil {
			if err := svc.fdb.RecordFDB(c.Request.Context(), tx, req.Port.NetworkID, portID, macAddress); err != nil {
				return err
			}
		}
		// What this create left the port at, so that its undo can tell it unchanged (SPEC-033 FR-008).
		return tx.QueryRow(c.Request.Context(), "SELECT updated_at FROM ports WHERE id = $1", portID).Scan(&created)
	})
	if err != nil {
		sendPortChangeError(c, err, "create") // a requested address refused as upstream's IPAM refuses it
		return
	}

	// The port's DHCP reservation, on the network node, and the firewall of the ports whose rules expand
	// its groups, which it joins with its addresses, on their nodes — before the answer (SPEC-033
	// FR-008, R29, R32). A create they cannot apply is undone compare-and-set and answers 500.
	ctx := c.Request.Context()
	dhcp := servesDHCP(ctx, svc.pool, fixedIPs)
	if err := svc.applyPortCreate(ctx, req.Port.NetworkID, dhcp, securityGroups); err != nil {
		log.Error().Err(err).Str("port_id", portID).Msg("create a port: it could not be applied; the create was not applied")
		svc.undoPortCreate(ctx, portID, created)
		svc.reapplyPortCreate(ctx, req.Port.NetworkID, dhcp, securityGroups)
		common.SendError(c, common.NewInternalServerError("failed to create port"))
		return
	}

	svc.answerPort(c, http.StatusCreated, portID)
}

// portReadScope is the predicate under which a caller may READ a port (aliased p, its network n): its
// project's, or one on a network of its project — upstream's get_port (PROJECT_READER,
// NET_OWNER_READER). A shared network is every project's to read, the ports on it are not: they are
// their projects' and the network owner's (SPEC-033 FR-061). A router's gateway port, which has no
// project, is thereby the administrator's and the external network owner's, as upstream's. GetPort,
// the listing and the listing's marker share it so the three cannot disagree. caller is the
// placeholder carrying the caller's project id.
func portReadScope(caller string) string {
	return "(p.project_id = " + caller + " OR n.project_id = " + caller + ")"
}

// ListPorts lists all ports
func (svc *Service) ListPorts(c *gin.Context) {
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

	pf := &common.SQLFilter{}
	// The caller's scope, every project's for an administrator, narrowed by project_id/tenant_id
	// (SPEC-033 FR-057).
	projectScope(c, pf, "p.project_id", portReadScope)

	// Marker paging over a TOTAL order. p.created_at alone is not unique, so the order was undefined
	// among ties AND `p.created_at < $n` excluded every port sharing the marker's timestamp rather
	// than only those before it — ports created by one boot share it (as #549 established for
	// compute).
	// The marker itself resolves as resolveListMarker describes.
	m, ok := svc.resolveListMarker(c, limit, "port",
		"SELECT p.created_at, p.id FROM ports p JOIN networks n ON p.network_id = n.id WHERE p.id::text = $1 AND "+scopeOrAdmin(portReadScope("$2"), "$3"))
	if !ok {
		return
	}
	if m != nil {
		pf.Cond(fmt.Sprintf("(p.created_at, p.id) < (%s, %s)", pf.Arg(m.createdAt), pf.Arg(m.id)))
	}

	// Standard Neutron port filters. ?device_id=<server> is how `openstack
	// server add floating ip` finds the guest's port; without it the client may
	// select a router interface and associate the FIP with the gateway.
	// Parse UUID filters before querying: malformed ids match nothing, and
	// canonical UUIDs match case-insensitively without casting indexed columns.
	for _, filter := range []struct {
		col, val string
		isUUID   bool
	}{
		{"p.device_id", c.Query("device_id"), true},
		{"p.network_id", c.Query("network_id"), true},
		{"p.device_owner", c.Query("device_owner"), false},
		{"p.mac_address", c.Query("mac_address"), false},
		{"p.id", c.Query("id"), true},
	} {
		if filter.isUUID && filter.val != "" {
			id, err := uuid.Parse(filter.val)
			if err != nil {
				c.JSON(http.StatusOK, gin.H{"ports": []gin.H{}})
				return
			}
			filter.val = id.String()
		}
		if filter.val != "" {
			pf.Eq(filter.col, filter.val)
		}
	}

	where := pf.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", pf.Arg(limit), pf.Arg(offset))
	ctx := c.Request.Context()
	rows, err := svc.pool.Query(ctx, `
		SELECT `+portColumns+`
		FROM ports p
		JOIN networks n ON p.network_id = n.id`+where+`
		ORDER BY p.created_at DESC, p.id DESC`+limitOffset, pf.Args()...)
	if err != nil {
		sendChangeError(c, err, "list", "ports")
		return
	}
	var records []portRecord
	for rows.Next() {
		p, err := scanPort(rows)
		if err != nil {
			rows.Close()
			sendChangeError(c, err, "list", "ports")
			return
		}
		records = append(records, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		sendChangeError(c, err, "list", "ports")
		return
	}
	// The ports' groups in one query, once the rows are read: not one per port while they are open,
	// which held a second connection per listing (SPEC-033 T195).
	ids := make([]string, len(records))
	for i, p := range records {
		ids[i] = p.id
	}
	groups, err := portsSecurityGroups(ctx, svc.pool, ids)
	if err != nil {
		sendChangeError(c, err, "list", "ports")
		return
	}
	ports := make([]gin.H, 0, len(records))
	for _, p := range records {
		ports = append(ports, p.render(c, groups[p.id]))
	}
	c.JSON(http.StatusOK, gin.H{"ports": ports})
}

// GetPort returns a single port
func (svc *Service) GetPort(c *gin.Context) {
	portID := c.Param("id")

	// A non-UUID reference (a port name — the CLI resolves names with a list
	// fallback only after a 404) can only name nothing here; the uuid-typed
	// lookup used to surface the cast error as 500 instead, which broke the
	// SDK's fallback for 'port show/set/delete <name>' (SPEC-031).
	if _, err := uuid.Parse(portID); err != nil {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}
	svc.answerPort(c, http.StatusOK, portID)
}

// answerPort answers with a port as its show reads and renders it: GetPort's answer, and a create's and
// an update's, which answer the port as it was stored (SPEC-033 T217).
func (svc *Service) answerPort(c *gin.Context, status int, portID string) {
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()
	p, err := scanPort(svc.pool.QueryRow(ctx, `
		SELECT `+portColumns+`
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		WHERE p.id::text = $1 AND `+scopeOrAdmin(portReadScope("$2"), "$3"), portID, projectID, isAdmin(c)))
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}
	if err != nil {
		sendChangeError(c, err, "get", "port")
		return
	}
	groups, err := PortSecurityGroupIDs(ctx, svc.pool, p.id)
	if err != nil {
		sendChangeError(c, err, "get", "port")
		return
	}
	c.JSON(status, gin.H{"port": p.render(c, groups)})
}

// portColumns are the columns a port's show and listing read, in scanPort's order. A column no path of
// Lightstack's leaves NULL is read as empty, so one row it cannot scan never aborts a listing.
const portColumns = `p.id, COALESCE(p.name, ''), p.network_id, p.device_id, p.device_owner, COALESCE(p.mac_address, ''),
		       COALESCE(p.admin_state_up, true), COALESCE(p.status, ''), p.fixed_ips, p.created_at, p.updated_at,
		       COALESCE(p.project_id::text, ''), COALESCE(p.binding_host_id, '')`

// portRecord is a port as its show and listing read it.
type portRecord struct {
	id, name, networkID, mac, status, owner, host string
	deviceID, deviceOwner                         sql.NullString
	adminStateUp                                  bool
	fixedIPs                                      []byte
	created, updated                              time.Time
}

// scanPort reads a row of portColumns.
func scanPort(row interface{ Scan(...any) error }) (portRecord, error) {
	var p portRecord
	err := row.Scan(&p.id, &p.name, &p.networkID, &p.deviceID, &p.deviceOwner, &p.mac, &p.adminStateUp, &p.status,
		&p.fixedIPs, &p.created, &p.updated, &p.owner, &p.host)
	return p, err
}

// render is the port's body as a show and a listing answer it: its own project, not the caller's —
// an administrator's listing spans every project, and a member's includes the ports on shared
// networks (SPEC-033 FR-057) — its groups, and its binding to whom may see it.
func (p portRecord) render(c *gin.Context, groups []string) gin.H {
	var fixedIPs []map[string]any
	if err := json.Unmarshal(p.fixedIPs, &fixedIPs); err != nil {
		log.Warn().Err(err).Str("port_id", p.id).Msg("failed to unmarshal port fixed_ips; treating as empty")
	}
	body := gin.H{
		"id":              p.id,
		"name":            p.name,
		"network_id":      p.networkID,
		"tenant_id":       p.owner,
		"project_id":      p.owner,
		"device_id":       p.deviceID.String,
		"device_owner":    p.deviceOwner.String,
		"mac_address":     p.mac,
		"admin_state_up":  p.adminStateUp,
		"status":          p.status,
		"fixed_ips":       fixedIPs,
		"security_groups": groups,
		"created_at":      p.created.Format(time.RFC3339),
		"updated_at":      p.updated.Format(time.RFC3339),
	}
	showBinding(c, body, p.host)
	return body
}

// showBinding reports the node a port is bound to, "" for none, as binding:host_id, to an
// administrator only, as upstream's policy has it (SPEC-033 FR-056).
func showBinding(c *gin.Context, port gin.H, host string) {
	if isAdmin(c) {
		port["binding:host_id"] = host
	}
}

// DeletePort deletes a port: resolved first, as the caller's (404), never a router's (409); withdrawn from
// its units before its record goes (SPEC-033 FR-040, R34); then one transaction releases and deletes it.
// When the withdrawal or the delete fails, the port stays and its units are converged from the records again.
func (svc *Service) DeletePort(c *gin.Context) {
	ctx := c.Request.Context()
	portID, err := resolveDeletablePort(ctx, svc.pool, c.Param("id"), scopeOf(c))
	if err != nil {
		sendChangeError(c, err, "delete", "port")
		return
	}
	restore, err := svc.WithdrawPorts(ctx, []string{portID})
	if err != nil {
		log.Error().Err(err).Str("port_id", portID).Msg("delete a port: its units could not be converged without it; the port stays")
		common.SendError(c, common.NewInternalServerError("failed to delete port"))
		return
	}
	var floatingIPs []FloatingIPAssociation
	err = database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var err error
		floatingIPs, err = deletePortRecord(ctx, tx, portID, scopeOf(c))
		return err
	})
	if err != nil {
		restore()
		sendChangeError(c, err, "delete", "port")
		return
	}
	svc.ConvergeDisassociated(context.WithoutCancel(ctx), floatingIPs)
	c.Status(http.StatusNoContent)
}

// resolveDeletablePort resolves a port a delete names: the caller's, any project's for an administrator, by
// id compared as text, so a malformed id matches nothing (404). A router's interface and gateway ports go
// with the router's own requests, never through the port API: 409 ServicePortInUse, administrators
// included, as upstream's prevent_l3_port_deletion (FR-065). They are the router's by its records, which
// nothing adds to an existing port, so this check before the withdrawal holds through the delete.
func resolveDeletablePort(ctx context.Context, q subnetQuerier, portID string, caller callerScope) (string, error) {
	var resolved string
	err := q.QueryRow(ctx, "SELECT id::text FROM ports WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"),
		portID, caller.project, caller.admin).Scan(&resolved)
	if errors.Is(err, database.ErrNoRows) {
		return "", common.NewNotFoundError("port")
	}
	if err != nil {
		return "", fmt.Errorf("resolve port %s: %w", portID, err)
	}
	owner, err := routerPortOwner(ctx, q, resolved)
	if err != nil {
		return "", err
	}
	if owner != "" {
		return "", common.NewNeutronError(http.StatusConflict, "ServicePortInUse",
			fmt.Sprintf("Port %s cannot be deleted directly via the port API: has device owner %s.", resolved, owner))
	}
	return resolved, nil
}

// deletePortRecord deletes a port in the caller's transaction: its lock first, then what references it
// let go (ReleaseDeletedPorts), then the port (SPEC-033 FR-008d, research R25). An association committed
// while the lock was awaited is read after it, so none escapes with a stale fixed IP.
func deletePortRecord(ctx context.Context, tx database.Tx, portID string, caller callerScope) ([]FloatingIPAssociation, error) {
	var locked string
	err := tx.QueryRow(ctx, "SELECT id::text FROM ports WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
		portID, caller.project, caller.admin).Scan(&locked)
	if errors.Is(err, database.ErrNoRows) {
		return nil, common.NewNotFoundError("port")
	}
	if err != nil {
		return nil, err
	}
	floatingIPs, err := ReleaseDeletedPorts(ctx, tx, []string{portID})
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID)
	return floatingIPs, err
}

// routerPortOwner names the router role a port holds by the router's records: network:router_interface
// for an interface's port, network:router_gateway for the gateway port of a router that exists, "" for
// any other. Upstream's guard reads the port's own device_owner and device_id; a router here is its
// records, as its dataplane is (SPEC-033 T170, T184).
func routerPortOwner(ctx context.Context, q subnetQuerier, portID string) (string, error) {
	var iface, gateway bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM router_interfaces WHERE port_id::text = $1),
		       EXISTS (SELECT 1 FROM ports p JOIN routers r ON r.id::text = p.device_id::text
		                WHERE p.id::text = $1 AND `+gatewayPortWhere("p.")+`)`,
		portID).Scan(&iface, &gateway)
	switch {
	case err != nil:
		return "", fmt.Errorf("read the router records of port %s: %w", portID, err)
	case iface:
		return ownerRouterInterface, nil
	case gateway:
		return ownerRouterGateway, nil
	}
	return "", nil
}

// undoPortCreate takes back a port create whose units — its network's DHCP, its node's firewall — could
// not be converged (SPEC-033 FR-008): the port goes unless another request has changed it since the create
// left it at created. It logs what it did; the caller converges the units from the records again either way.
func (svc *Service) undoPortCreate(ctx context.Context, portID string, created time.Time) {
	tag, err := svc.pool.Exec(context.WithoutCancel(ctx), "DELETE FROM ports WHERE id = $1 AND updated_at = $2", portID, created)
	switch {
	case err != nil:
		log.Error().Err(err).Str("port_id", portID).Msg("undo a port create; the port stays")
	case tag.RowsAffected() == 0:
		log.Info().Str("port_id", portID).Msg("undo a port create: another change reached the port since; it keeps that change")
	}
}

// ReleaseDeletedPorts lets go what references ports about to be deleted, on the caller's transaction, as
// upstream's port deletion does: their floating IPs' associations (FR-008d) and their port forwardings,
// a forwarding's floating IP freed with its last one (FR-049). Every path that deletes ports — the port
// API, a failed boot's release, a server's deletion — goes through it (SPEC-033 T251). It returns the
// disassociated floating IPs, which the caller converges after its commit. The caller holds the ports'
// locks.
func ReleaseDeletedPorts(ctx context.Context, tx database.Tx, portIDs []string) ([]FloatingIPAssociation, error) {
	disassociated, err := disassociatePortFloatingIPs(ctx, tx, portIDs)
	if err != nil {
		return nil, err
	}
	return disassociated, deletePortForwardingsOf(ctx, tx, portIDs)
}

// disassociatePortFloatingIPs clears the association of every floating IP whose port is one of
// portIDs, on the caller's transaction, as upstream's port deletion does: no port, fixed IP or router,
// and status DOWN, which upstream's L3 agent reports afterwards (SPEC-033 FR-008d, research R25). It
// returns their addresses.
func disassociatePortFloatingIPs(ctx context.Context, tx database.Tx, portIDs []string) ([]FloatingIPAssociation, error) {
	if len(portIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT floating_ip_address, COALESCE(fixed_ip_address, ''), COALESCE(router_id::text, '') FROM floating_ips
		 WHERE port_id::text = ANY($1) ORDER BY floating_ip_address`, portIDs)
	if err != nil {
		return nil, fmt.Errorf("read the floating IPs of the ports: %w", err)
	}
	var was []FloatingIPAssociation
	for rows.Next() {
		var a FloatingIPAssociation
		if err := rows.Scan(&a.Address, &a.FixedIP, &a.RouterID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a floating IP of the ports: %w", err)
		}
		was = append(was, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the floating IPs of the ports: %w", err)
	}
	if len(was) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE floating_ips SET port_id = NULL, fixed_ip_address = NULL, router_id = NULL, status = 'DOWN', updated_at = $2
		WHERE port_id::text = ANY($1)`, portIDs, time.Now()); err != nil {
		return nil, fmt.Errorf("disassociate the floating IPs of the ports: %w", err)
	}
	return was, nil
}

// UpdatePort updates a port. security_groups replaces the port's groups: null and [] leave none, and
// absent leaves them as they are (SPEC-033 FR-017, FR-018). A change to the port's groups, or to its
// device_owner into or out of network:* — such a port has no chains, its leg trusted by name — reaches the
// firewall of the port's node, and of every port whose rules expand a group it left or joined, before the
// answer; when it cannot, the whole update goes back, compare-and-set, and the request answers 500
// (FR-008, R12, T263, T264).
func (svc *Service) UpdatePort(c *gin.Context) {
	portID := c.Param("id")

	// See GetPort: a non-UUID reference must 404, not 500 (SPEC-031).
	if _, err := uuid.Parse(portID); err != nil {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}

	var req updatePortRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	// Checks 1 and 2 read only the body, before anything is looked up (research R12).
	written, refusal := req.parsed()
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}
	ctx := c.Request.Context()
	var before portState
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var err error
		before, err = storePortUpdate(ctx, tx, portID, written, scopeOf(c))
		return err
	})
	if err != nil {
		sendPortChangeError(c, err, "update")
		return
	}
	if written.reachesFirewall(before) {
		// Stored. From here the change is applied or undone, whether or not the client is still there.
		if err := svc.applyPortUpdate(context.WithoutCancel(ctx), before.id, written, before); err != nil {
			common.SendError(c, common.NewInternalServerError("failed to apply the security group change"))
			return
		}
	}
	svc.answerPort(c, http.StatusOK, before.id)
}

// updatePortRequest is a port update's body.
type updatePortRequest struct {
	Port struct {
		Name           *string         `json:"name"`
		AdminStateUp   *bool           `json:"admin_state_up"`
		DeviceID       *string         `json:"device_id"`
		DeviceOwner    *string         `json:"device_owner"`
		SecurityGroups json.RawMessage `json:"security_groups"`
	} `json:"port"`
}

// parsed is the update as it is written: the columns it sets and, when given, the groups. An update that
// sets nothing is 400.
func (r updatePortRequest) parsed() (portState, *common.OpenStackError) {
	w := portState{name: r.Port.Name, adminStateUp: r.Port.AdminStateUp, deviceID: r.Port.DeviceID, deviceOwner: r.Port.DeviceOwner}
	groups, given, refusal := parseSecurityGroupsField(r.Port.SecurityGroups)
	if refusal != nil {
		return w, refusal
	}
	w.groups, w.groupsGiven = groups, given
	if w.fields().Empty() && !given {
		return w, common.NewBadRequestError("no updates provided")
	}
	return w, nil
}

// portState is what a port update sets — the columns, nil for those it leaves, and the groups when
// groupsGiven — or, for the port before it, every one of them.
type portState struct {
	id, project                 string
	name, deviceID, deviceOwner *string
	adminStateUp                *bool
	groups                      []string
	groupsGiven                 bool
}

// fields is the UPDATE of the columns s sets.
func (s portState) fields() *common.SQLUpdate {
	u := &common.SQLUpdate{}
	for _, c := range []struct {
		column string
		value  *string
	}{{"name", s.name}, {"device_id", s.deviceID}, {"device_owner", s.deviceOwner}} {
		if c.value != nil {
			u.Set(c.column, *c.value)
		}
	}
	if s.adminStateUp != nil {
		u.Set("admin_state_up", *s.adminStateUp)
	}
	return u
}

// over is the port after an update w wrote over s.
func (w portState) over(s portState) portState {
	after := s
	for _, f := range []struct{ to, from **string }{{&after.name, &w.name}, {&after.deviceID, &w.deviceID}, {&after.deviceOwner, &w.deviceOwner}} {
		if *f.from != nil {
			*f.to = *f.from
		}
	}
	if w.adminStateUp != nil {
		after.adminStateUp = w.adminStateUp
	}
	if w.groupsGiven {
		after.groups, after.groupsGiven = w.groups, true
	}
	return after
}

// reachesFirewall reports whether an update w of the port before changes a firewall: its groups, or its
// device_owner into or out of network:*.
func (w portState) reachesFirewall(before portState) bool {
	return w.groupsGiven || (w.deviceOwner != nil && trustedOwner(*w.deviceOwner) != trustedOwner(derefOr(before.deviceOwner, "")))
}

// storePortUpdate writes a port update in the caller's transaction and returns the port as it was. Check 3:
// the port is the caller's, its lock the check, which serialises the update with every other change to the
// port's groups. A network:* device_owner is the network owner's or an administrator's (FR-062). Check 4,
// after the lock: each group is one the caller can see — the port's project's for a member, any project's
// for an administrator, as upstream (FR-018, R52).
func storePortUpdate(ctx context.Context, tx database.Tx, portID string, w portState, caller callerScope) (portState, error) {
	var before portState
	var project *string
	err := tx.QueryRow(ctx, "SELECT id::text, project_id::text, name, device_id::text, device_owner, admin_state_up FROM ports WHERE id::text = $1 AND "+
		ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE", portID, caller.project, caller.admin,
	).Scan(&before.id, &project, &before.name, &before.deviceID, &before.deviceOwner, &before.adminStateUp)
	if errors.Is(err, database.ErrNoRows) {
		return before, common.NewNotFoundError("port")
	}
	if err != nil {
		return before, fmt.Errorf("lock port: %w", err)
	}
	before.project = derefOr(project, caller.project)
	if w.deviceOwner != nil && trustedOwner(*w.deviceOwner) {
		if err := requireNetworkDeviceOwner(ctx, tx, before.id, *w.deviceOwner, caller); err != nil {
			return before, err
		}
	}
	if w.groupsGiven {
		if err := requireGroupsOfProject(ctx, tx, w.groups, before.project, caller.admin); err != nil {
			return before, err
		}
		if before.groups, err = PortSecurityGroupIDs(ctx, tx, before.id); err != nil {
			return before, fmt.Errorf("read the port's groups: %w", err)
		}
		if err := SetPortSecurityGroups(ctx, tx, before.id, w.groups); err != nil {
			return before, err
		}
	}
	return before, writePortColumns(ctx, tx, before.id, w.fields())
}

// writePortColumns writes the columns an update sets, with its time.
func writePortColumns(ctx context.Context, tx database.Tx, portID string, u *common.SQLUpdate) error {
	if u.Empty() {
		return nil
	}
	u.Set("updated_at", time.Now())
	_, err := tx.Exec(ctx, "UPDATE ports SET "+u.Assignments()+" WHERE id = "+u.Arg(portID), u.Args()...)
	return err
}

// trustedOwner reports whether a port with the device_owner is trusted, as upstream's is_port_trusted: a
// router's or a DHCP port (network:*), which carries no security group (FR-072) and has no chains, its leg
// trusted by name (FR-041).
func trustedOwner(deviceOwner string) bool { return strings.HasPrefix(deviceOwner, "network:") }

// requireNetworkDeviceOwner refuses a network:* device_owner on a port of a network the caller does not own,
// administrators excepted (FR-062).
func requireNetworkDeviceOwner(ctx context.Context, tx database.Tx, portID, owner string, caller callerScope) error {
	var networkProject string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(n.project_id::text, '') FROM ports p JOIN networks n ON n.id = p.network_id
		 WHERE p.id::text = $1`, portID).Scan(&networkProject); err != nil {
		return fmt.Errorf("read the port's network: %w", err)
	}
	if refusal := networkDeviceOwnerRefusal("update_port", owner, networkProject, caller.project, caller.admin); refusal != nil {
		return refusal
	}
	return nil
}

// applyPortUpdate converges what a stored port update changes: the port's chains on its node, and those of
// every port whose rules expand a group it left or joined (SPEC-033 R32). When that fails, the update goes
// back, compare-and-set, and the firewalls are converged again.
func (svc *Service) applyPortUpdate(ctx context.Context, portID string, written, before portState) error {
	changed := append(slices.Clone(before.groups), written.groups...)
	err := svc.RefreshPortFilters(ctx, []string{portID}, changed)
	if err == nil {
		return nil
	}
	if uerr := svc.restorePort(ctx, written.over(before), before); uerr != nil {
		log.Error().Err(uerr).Str("port_id", portID).Msg("put a port back; the reconciliation pass applies what the port holds")
	}
	if rerr := svc.RefreshPortFilters(ctx, []string{portID}, changed); rerr != nil {
		log.Error().Err(rerr).Str("port_id", portID).Msg("rebuild the filters after an undo; the reconciliation pass converges them")
	}
	log.Error().Err(err).Str("port_id", portID).Msg("this node could not apply the port's update; it was not applied")
	return err
}

// restorePort puts a port back as it was before an update the node could not apply, but only what still
// holds that update: groups another request has written since stay (research R5), and so do columns another
// request has written since.
func (svc *Service) restorePort(ctx context.Context, after, before portState) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, "SELECT id::text FROM ports WHERE id::text = $1 FOR UPDATE", before.id).Scan(&locked)
		if errors.Is(err, database.ErrNoRows) {
			return nil // deleted since: nothing to put back
		}
		if err != nil {
			return fmt.Errorf("lock port %s: %w", before.id, err)
		}
		if after.groupsGiven {
			if err := restorePortGroups(ctx, tx, before.id, after.groups, before.groups); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE ports SET name = $2, device_id = $3, device_owner = $4, admin_state_up = $5
			 WHERE id::text = $1 AND COALESCE(name, '') = $6 AND COALESCE(device_id::text, '') = $7
			   AND COALESCE(device_owner, '') = $8 AND admin_state_up = $9`,
			before.id, before.name, before.deviceID, before.deviceOwner, before.adminStateUp,
			derefOr(after.name, ""), derefOr(after.deviceID, ""), derefOr(after.deviceOwner, ""), after.adminStateUp)
		return err
	})
}

// restorePortGroups puts a port's previous groups back in the caller's transaction, which holds the port's
// lock, but only while the port still carries the list the undone change wrote: a list another request has
// written since stays (research R5).
func restorePortGroups(ctx context.Context, tx database.Tx, portID string, written, previous []string) error {
	now, err := PortSecurityGroupIDs(ctx, tx, portID)
	if err != nil {
		return fmt.Errorf("read the groups of port %s: %w", portID, err)
	}
	want := slices.Clone(written)
	slices.Sort(want)
	if !slices.Equal(now, want) {
		log.Info().Str("port_id", portID).Msg("undo: another change reached the port since; it keeps that change")
		return nil
	}
	return SetPortSecurityGroups(ctx, tx, portID, previous)
}

// parseSecurityGroupsField reads a port body's security_groups as upstream's attribute does
// (neutron/extensions/securitygroup.py, research R12): absent is "not given"; null converts to the
// empty list; anything else must be a list of UUIDs without repeats. The ids come back in their
// canonical form. It reads only the body, so its answers come before any lookup.
func parseSecurityGroupsField(raw json.RawMessage) (ids []string, given bool, refusal *common.OpenStackError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false, nil
	}
	if trimmed == "null" {
		return []string{}, true, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, true, invalidAttribute("security_groups", fmt.Sprintf("'%s' is not a list", trimmed))
	}
	ids = make([]string, 0, len(items))
	for _, item := range items {
		var s string
		if json.Unmarshal(item, &s) != nil {
			return nil, true, invalidAttribute("security_groups", fmt.Sprintf("'%s' is not a valid UUID", item))
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, true, invalidAttribute("security_groups", fmt.Sprintf("'%s' is not a valid UUID", s))
		}
		ids = append(ids, id.String())
	}
	seen := make(map[string]bool, len(ids))
	var dups []string
	for _, id := range ids {
		if seen[id] && !slices.Contains(dups, id) {
			dups = append(dups, id)
		}
		seen[id] = true
	}
	if len(dups) > 0 {
		return nil, true, invalidAttribute("security_groups", fmt.Sprintf("Duplicate items in the list: '%s'", strings.Join(dups, ", ")))
	}
	return ids, true, nil
}

// requireGroupsOfProject answers 404 for the first id that names no group the caller can see: a group
// of projectID, or for an administrator a group of any project, as upstream (SPEC-033 FR-018, research
// R52). A group's project never changes, so the read needs no lock; the foreign key re-checks each
// group's existence at the write, where its loss is ErrSecurityGroupNotFound.
func requireGroupsOfProject(ctx context.Context, q subnetQuerier, ids []string, projectID string, admin bool) error {
	for _, id := range ids {
		var ok bool
		if err := q.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM security_groups WHERE id = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+")", id, projectID, admin,
		).Scan(&ok); err != nil {
			return fmt.Errorf("look up security group %s: %w", id, err)
		}
		if !ok {
			// The resource and its id separately: folded into one string, the resource missed the
			// envelope's type table and came back as a plain NotFound (SPEC-024 FR-007).
			return common.NewResourceNotFoundError("security group", id)
		}
	}
	return nil
}

// networkDeviceOwnerRefusal is upstream's create_port:device_owner and update_port:device_owner: a
// device_owner starting "network:" gives a port a role its network plays — its DHCP port, a router's
// port — so only an administrator or a member of the network's own project may set one (SPEC-033
// FR-062). It is nil for any other owner, and for a caller who may.
func networkDeviceOwnerRefusal(action, deviceOwner, networkProject, caller string, admin bool) *common.OpenStackError {
	if !trustedOwner(deviceOwner) || admin || networkProject == caller {
		return nil
	}
	return common.NewNeutronError(http.StatusForbidden, "PolicyNotAuthorized",
		fmt.Sprintf("(rule:%s:device_owner) is disallowed by policy", action))
}

// sendPortChangeError answers a failed port create or update: no address left as 409, a group deleted
// since it was checked as 404, stateful and stateless groups together as 409 (FR-051), and the rest as
// sendChangeError does.
func sendPortChangeError(c *gin.Context, err error, operation string) {
	switch {
	case errors.Is(err, ErrNoAvailableIP):
		common.SendError(c, common.NewConflictError("no IP address available in the network's subnet"))
	case errors.Is(err, ErrSecurityGroupNotFound):
		common.SendError(c, common.NewNotFoundError("security group"))
	case errors.Is(err, ErrSecurityGroupConflict):
		common.SendError(c, errSecurityGroupConflict())
	default:
		sendChangeError(c, err, operation, "port")
	}
}

type subnetQuerier interface {
	Query(context.Context, string, ...any) (database.Rows, error)
	QueryRow(context.Context, string, ...any) database.Row
}

// ErrNoAvailableIP distinguishes exhaustion from a database failure.
var ErrNoAvailableIP = errors.New("no IP address available in the subnet")

// ErrSecurityGroupNotFound reports a security group that does not exist in the project it has to
// belong to: unknown by name or id, another project's, or deleted after it was resolved.
var ErrSecurityGroupNotFound = errors.New("security group not found")

// ResolveSecurityGroup finds a group by id or by name. An id match wins, so a group named after
// another's id cannot shadow it. An id names a group of projectID, or for an administrator a group of
// any project, as upstream (SPEC-033 FR-004, FR-018, research R52); a name resolves in projectID only,
// administrators included (constitution register, entry 5). It is the one resolver for server create,
// addSecurityGroup and removeSecurityGroup (SPEC-033 R2). The id is compared as text, so a malformed id
// is simply not found. Names may repeat, as upstream's may: a name several of the project's groups share
// resolves to none of them, as an *AmbiguousSecurityGroupError, which compute answers 409 as upstream's
// NoUniqueMatch (FR-026).
func ResolveSecurityGroup(ctx context.Context, q subnetQuerier, ref, projectID string, admin bool) (string, error) {
	var id string
	err := q.QueryRow(ctx,
		"SELECT id FROM security_groups WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"), ref, projectID, admin).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, database.ErrNoRows) {
		return "", fmt.Errorf("resolving security group %q: %w", ref, err)
	}
	rows, err := q.Query(ctx,
		"SELECT id FROM security_groups WHERE name = $1 AND project_id = $2 LIMIT 2", ref, projectID)
	if err != nil {
		return "", fmt.Errorf("resolving security group %q: %w", ref, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("resolving security group %q: %w", ref, err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("resolving security group %q: %w", ref, err)
	}
	switch len(found) {
	case 0:
		return "", ErrSecurityGroupNotFound
	case 1:
		return found[0], nil
	}
	return "", &AmbiguousSecurityGroupError{Name: ref}
}

// PortSecurityGroupIDs returns the groups a port carries, in order (portsSecurityGroups). A port with
// none gives an empty list.
func PortSecurityGroupIDs(ctx context.Context, q subnetQuerier, portID string) ([]string, error) {
	groups, err := portsSecurityGroups(ctx, q, []string{portID})
	if err != nil {
		return nil, err
	}
	return groups[portID], nil
}

// portsSecurityGroups reads the groups each of ports carries, in order, in one query: the only reader of
// a port's groups, which the filter, a server's report and a port's show and listing are built from
// (SPEC-033 R1; SPEC-033 T195). A port with none has an empty list.
func portsSecurityGroups(ctx context.Context, q subnetQuerier, ports []string) (map[string][]string, error) {
	groups := make(map[string][]string, len(ports))
	for _, p := range ports {
		groups[p] = []string{}
	}
	if len(ports) == 0 {
		return groups, nil
	}
	rows, err := q.Query(ctx, `
		SELECT port_id::text, security_group_id::text FROM port_security_groups
		 WHERE port_id::text = ANY($1) ORDER BY port_id, security_group_id`, ports)
	if err != nil {
		return nil, fmt.Errorf("reading the security groups of ports %v: %w", ports, err)
	}
	defer rows.Close()
	for rows.Next() {
		var port, group string
		if err := rows.Scan(&port, &group); err != nil {
			return nil, fmt.Errorf("scanning a port's security group: %w", err)
		}
		groups[port] = append(groups[port], group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the security groups of ports %v: %w", ports, err)
	}
	return groups, nil
}

// AddPortSecurityGroup puts a group on a port and moves the port's updated_at, as upstream's
// update_port does. The caller holds the port's row lock, so a foreign-key violation can only mean the
// group: it is reported as ErrSecurityGroupNotFound, a group deleted after it was resolved. A port left
// with stateful and stateless groups is refused with ErrSecurityGroupConflict (FR-051).
func AddPortSecurityGroup(ctx context.Context, tx database.Tx, portID, groupID string) error {
	if err := insertPortSecurityGroup(ctx, tx, portID, groupID); err != nil {
		return err
	}
	if err := requireOneStatefulness(ctx, tx, portID); err != nil {
		return err
	}
	return touchPort(ctx, tx, portID)
}

// RemovePortSecurityGroup takes a group off a port and moves its updated_at. It reports whether the
// port carried the group; one it did not carry is no error, and leaves updated_at alone.
func RemovePortSecurityGroup(ctx context.Context, tx database.Tx, portID, groupID string) (bool, error) {
	tag, err := tx.Exec(ctx,
		"DELETE FROM port_security_groups WHERE port_id = $1 AND security_group_id::text = $2", portID, groupID)
	if err != nil {
		return false, fmt.Errorf("removing security group %s from port %s: %w", groupID, portID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, touchPort(ctx, tx, portID)
}

// SetPortSecurityGroups replaces a port's groups with groupIDs, which may be empty, and moves its
// updated_at. The caller has checked the ids for format and repeats. A list mixing stateful and
// stateless groups is refused with ErrSecurityGroupConflict (FR-051).
func SetPortSecurityGroups(ctx context.Context, tx database.Tx, portID string, groupIDs []string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM port_security_groups WHERE port_id = $1", portID); err != nil {
		return fmt.Errorf("clearing the security groups of port %s: %w", portID, err)
	}
	for _, g := range groupIDs {
		if err := insertPortSecurityGroup(ctx, tx, portID, g); err != nil {
			return err
		}
	}
	if err := requireOneStatefulness(ctx, tx, portID); err != nil {
		return err
	}
	return touchPort(ctx, tx, portID)
}

func insertPortSecurityGroup(ctx context.Context, tx database.Tx, portID, groupID string) error {
	// A malformed id cannot name a group. Checked here because PostgreSQL would answer it with a
	// syntax error that aborts the caller's transaction.
	if uuid.Validate(groupID) != nil {
		return ErrSecurityGroupNotFound
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2)", portID, groupID); err != nil {
		if database.IsForeignKeyViolation(err) {
			return ErrSecurityGroupNotFound
		}
		return fmt.Errorf("adding security group %s to port %s: %w", groupID, portID, err)
	}
	return nil
}

func touchPort(ctx context.Context, tx database.Tx, portID string) error {
	if _, err := tx.Exec(ctx, "UPDATE ports SET updated_at = now() WHERE id = $1", portID); err != nil {
		return fmt.Errorf("updating port %s: %w", portID, err)
	}
	return nil
}

// LockSubnet serializes allocation and insertion across Neutron and Nova. Hold
// this transaction until the port's fixed_ips have been inserted.
func LockSubnet(ctx context.Context, tx database.Tx, subnetID string) (string, error) {
	var cidr string
	err := tx.QueryRow(ctx, "SELECT cidr FROM subnets WHERE id = $1 FOR UPDATE", subnetID).Scan(&cidr)
	return cidr, err
}

// Security Groups implementation

// CreateSecurityGroupRequest represents a security group creation request
type CreateSecurityGroupRequest struct {
	SecurityGroup struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		// Absent means stateful, as upstream's default (FR-051).
		Stateful *bool `json:"stateful"`
		// The project the group belongs to: an administrator's to name (SPEC-033 FR-058).
		ProjectID *string `json:"project_id"`
		TenantID  *string `json:"tenant_id"`
	} `json:"security_group"`
}

// CreateSecurityGroup creates a security group with upstream's starting rules, in one transaction
// (FR-043). Its name may repeat another group's, as upstream's may; only default is reserved, in any
// case (FR-025, FR-026). It may be stateless (FR-051). Its revision starts at 0: upstream does not
// count a new group's own starting rules (FR-050).
func (svc *Service) CreateSecurityGroup(c *gin.Context) {
	var req CreateSecurityGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	if isDefaultName(req.SecurityGroup.Name) {
		common.SendError(c, errDefaultAlreadyExists())
		return
	}
	stateful := true
	if req.SecurityGroup.Stateful != nil {
		stateful = *req.SecurityGroup.Stateful
	}

	projectID, ok := svc.ownerOf(c, req.SecurityGroup.ProjectID, req.SecurityGroup.TenantID)
	if !ok {
		return
	}
	if err := svc.ensureDefaultOf(c.Request.Context(), projectID); err != nil {
		sendChangeError(c, err, "create", "security group")
		return
	}
	sgID := uuid.New().String()

	ctx := c.Request.Context()
	now := time.Now()
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO security_groups (id, name, project_id, description, stateful, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			sgID, req.SecurityGroup.Name, projectID, req.SecurityGroup.Description, stateful, now, now); err != nil {
			return err
		}
		return seedDefaultSecurityGroupRules(ctx, tx, sgID)
	})
	if err != nil {
		log.Error().Err(err).Str("operation", "create_security_group").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create security group"))
		return
	}

	g := securityGroupRow{id: sgID, name: req.SecurityGroup.Name, description: req.SecurityGroup.Description,
		projectID: projectID, stateful: stateful, createdAt: now, updatedAt: now}
	body, err := renderSecurityGroup(ctx, svc.pool, g)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_security_group").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read the created security group"))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"security_group": body})
}

// securityGroupRulesByGroup loads the rules of the given security groups, keyed by
// security_group_id. Groups without rules are absent from the map; callers must
// substitute an empty slice rather than nil — see ListSecurityGroups.
func securityGroupRulesByGroup(ctx context.Context, pool database.DB, sgIDs []string) (map[string][]gin.H, error) {
	byGroup := make(map[string][]gin.H, len(sgIDs))
	if len(sgIDs) == 0 {
		return byGroup, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT id, security_group_id, direction, ethertype, protocol, port_range_min,
		       port_range_max, remote_ip_prefix, remote_group_id, created_at
		FROM security_group_rules
		WHERE security_group_id = ANY($1)
		ORDER BY created_at, id
	`, sgIDs)
	if err != nil {
		return nil, fmt.Errorf("query security group rules for %d group(s): %w", len(sgIDs), err)
	}
	defer rows.Close()

	for rows.Next() {
		var ruleID, sgID, direction, ethertype string
		var protocol, remoteIPPrefix, remoteGroupID *string
		var portRangeMin, portRangeMax *int
		var createdAt time.Time

		if err := rows.Scan(&ruleID, &sgID, &direction, &ethertype, &protocol,
			&portRangeMin, &portRangeMax, &remoteIPPrefix, &remoteGroupID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan security group rule row: %w", err)
		}

		// Emit every key unconditionally — null when unset — exactly as upstream
		// Neutron does. Horizon's SecurityGroupRule.__init__ dereferences protocol,
		// port_range_min, port_range_max, remote_ip_prefix and remote_group_id as
		// sgr['...'], so an *absent* key is a KeyError (HTTP 500 "'protocol'"), not a
		// missing value. Nil pointers marshal to JSON null, which it handles.
		//
		// remote_ip_prefix / remote_group_id are passed through as pointers rather than
		// normalized, so a NULL column is null here exactly as it is in the two flat
		// emitters — and as upstream reports it ("remote_ip_prefix": null in the api-ref
		// examples). An earlier version substituted "" for these two to match what
		// GetSecurityGroup used to emit, which left one emitter disagreeing with the
		// other two: the thing FR-023 exists to prevent. Horizon reads either as unset
		// (`if not sgr[...]`), so the only observable change is "" → null on group-by-id.
		byGroup[sgID] = append(byGroup[sgID], gin.H{
			"id":                ruleID,
			"security_group_id": sgID,
			"direction":         direction,
			"ethertype":         ethertype,
			"protocol":          protocol,
			"port_range_min":    portRangeMin,
			"port_range_max":    portRangeMax,
			"remote_ip_prefix":  remoteIPPrefix,
			"remote_group_id":   remoteGroupID,
			"created_at":        createdAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate security group rules: %w", err)
	}
	return byGroup, nil
}

// listSecurityGroupRows loads the groups filter selects (securityGroupListFilter).
//
// This lives in its own function so that its `defer rows.Close()` returns the pooled
// connection when it returns — before the caller issues the rules query. Scanning
// inline in ListSecurityGroups would hold two pool connections per in-flight request,
// which a busy endpoint can turn into pool exhaustion rather than mere slowness.
func listSecurityGroupRows(ctx context.Context, pool database.DB, filter *common.SQLFilter) ([]securityGroupRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT `+securityGroupColumns+`
		FROM security_groups`+filter.Where()+`
		ORDER BY created_at DESC, id DESC
	`, filter.Args()...)
	if err != nil {
		return nil, fmt.Errorf("query security groups: %w", err)
	}
	defer rows.Close()

	var out []securityGroupRow
	for rows.Next() {
		g, err := scanSecurityGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan security group row: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate security groups: %w", err)
	}
	return out, nil
}

// ListSecurityGroups lists all security groups.
//
// Every group MUST carry a security_group_rules array, as upstream Neutron does.
// Horizon reads this list through openstacksdk, whose to_dict() emits the key
// unconditionally — so omitting it yields null rather than a missing key, and
// SecurityGroup.__init__ ("if 'security_group_rules' not in sg") does not catch it:
// the panel and the launch wizard's security-group step both died with
// "'NoneType' object is not iterable".
func (svc *Service) ListSecurityGroups(c *gin.Context) {
	filter, refusal := securityGroupListFilter(c)
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}
	if err := svc.ensureDefaultOf(c.Request.Context(), listedProject(c)); err != nil {
		sendChangeError(c, err, "list", "security groups")
		return
	}
	sgRows, err := listSecurityGroupRows(c.Request.Context(), svc.pool, filter)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_security_groups").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security groups"))
		return
	}

	sgIDs := make([]string, 0, len(sgRows))
	for _, r := range sgRows {
		sgIDs = append(sgIDs, r.id)
	}

	rulesByGroup, err := securityGroupRulesByGroup(c.Request.Context(), svc.pool, sgIDs)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_security_group_rules").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security group rules"))
		return
	}

	securityGroups := []gin.H{}
	for _, r := range sgRows {
		securityGroups = append(securityGroups, r.render(rulesByGroup[r.id]))
	}

	c.JSON(http.StatusOK, gin.H{"security_groups": securityGroups})
}

// lookupSecurityGroup finds a group by id — of projectID, or for an administrator of any project
// (SPEC-033 FR-057) — or, failing that, by name in projectID. The CLI looks a group
// up by name (e.g. `... rule create ... default`). A name several groups share names none of them, as
// upstream's group path answers any name with 404, so a client falls back to listing by name (FR-026).
// The id is compared as text, so a malformed one is simply not found.
func lookupSecurityGroup(ctx context.Context, q subnetQuerier, ref, projectID string, admin bool) (securityGroupRow, error) {
	g, err := scanSecurityGroup(q.QueryRow(ctx,
		"SELECT "+securityGroupColumns+" FROM security_groups WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3"), ref, projectID, admin))
	if !errors.Is(err, database.ErrNoRows) {
		return g, err
	}
	rows, err := q.Query(ctx,
		"SELECT "+securityGroupColumns+" FROM security_groups WHERE name = $1 AND project_id = $2 LIMIT 2", ref, projectID)
	if err != nil {
		return securityGroupRow{}, err
	}
	defer rows.Close()
	var found []securityGroupRow
	for rows.Next() {
		g, err := scanSecurityGroup(rows)
		if err != nil {
			return securityGroupRow{}, err
		}
		found = append(found, g)
	}
	if err := rows.Err(); err != nil {
		return securityGroupRow{}, err
	}
	if len(found) != 1 {
		return securityGroupRow{}, database.ErrNoRows
	}
	return found[0], nil
}

// GetSecurityGroup returns a single security group
func (svc *Service) GetSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")

	g, err := lookupSecurityGroup(c.Request.Context(), svc.pool, sgID, projectID, isAdmin(c))
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("security group"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group"))
		return
	}

	// Rules keyed by the resolved id, not the possibly-by-name request param.
	body, err := renderSecurityGroup(c.Request.Context(), svc.pool, g)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_sg_rules").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group rules"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"security_group": body})
}

// lockSecurityGroup reads a group of projectID by id with its row lock. FOR UPDATE, not the weaker
// lock an UPDATE takes: a port's membership insert holds a key-share lock on the group through its
// foreign key, and only FOR UPDATE waits for it, so an in-use check made under this lock cannot miss a
// membership being written.
func lockSecurityGroup(ctx context.Context, tx database.Tx, id, projectID string, admin bool) (securityGroupRow, error) {
	g, err := scanSecurityGroup(tx.QueryRow(ctx,
		"SELECT "+securityGroupColumns+" FROM security_groups WHERE id::text = $1 AND "+ownerOrAdmin("project_id", "$2", "$3")+" FOR UPDATE",
		id, projectID, admin))
	if errors.Is(err, database.ErrNoRows) {
		return g, common.NewNotFoundError("security group")
	}
	return g, err
}

func securityGroupInUse(ctx context.Context, tx database.Tx, id string) (bool, error) {
	var inUse bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM port_security_groups WHERE security_group_id = $1)", id).Scan(&inUse)
	return inUse, err
}

// DeleteSecurityGroup deletes a security group, refusing as upstream does, in upstream's order: a group
// a port carries is in use, for everyone (FR-008c); a project's default may be deleted by an
// administrator only (FR-024). If-Match: revision_number=<n> must name the group's revision (FR-050).
// A group that is not the caller's project's is 404 (register entry 5).
func (svc *Service) DeleteSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()
	ifMatch, constrained, refusal := ifMatchRevision(c)
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}

	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		g, err := lockSecurityGroup(ctx, tx, sgID, projectID, isAdmin(c))
		if err != nil {
			return err
		}
		sgID = g.id
		inUse, err := securityGroupInUse(ctx, tx, g.id)
		if err != nil {
			return err
		}
		if inUse {
			return errSecurityGroupInUse(g.id)
		}
		if g.name == "default" && !common.HasRole(c, common.RoleAdmin) {
			return errCannotRemoveDefault()
		}
		if constrained && ifMatch != g.revision {
			return errRevisionConstraint(ifMatch, g.revision)
		}
		// Its rules, and other groups' rules that name it as their remote group, go with it
		// (the foreign keys cascade), as upstream.
		_, err = tx.Exec(ctx, "DELETE FROM security_groups WHERE id = $1", g.id)
		return err
	})
	var osErr *common.OpenStackError
	if errors.As(err, &osErr) {
		common.SendError(c, osErr)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete security group"))
		return
	}

	c.Status(http.StatusNoContent)
}

// UpdateSecurityGroup changes a group's name, description and statefulness, refusing as upstream does:
// default as the new name, in any case, before anything is looked up (FR-025); a change of
// statefulness while a port carries the group (FR-051); a new name for default (FR-025). If-Match:
// revision_number=<n> must name the group's revision. A change adds one to the revision; a request
// that changes nothing writes nothing (FR-050).
func (svc *Service) UpdateSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	var req struct {
		SecurityGroup struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Stateful    *bool   `json:"stateful"`
		} `json:"security_group"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	update := req.SecurityGroup
	if update.Name != nil && isDefaultName(*update.Name) {
		common.SendError(c, errDefaultAlreadyExists())
		return
	}
	ifMatch, constrained, refusal := ifMatchRevision(c)
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}

	var g securityGroupRow
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var err error
		if g, err = lockSecurityGroup(ctx, tx, sgID, projectID, isAdmin(c)); err != nil {
			return err
		}
		if update.Stateful != nil && *update.Stateful != g.stateful {
			inUse, err := securityGroupInUse(ctx, tx, g.id)
			if err != nil {
				return err
			}
			if inUse {
				return errSecurityGroupInUse(g.id)
			}
		}
		if g.name == "default" && update.Name != nil {
			return errCannotUpdateDefault()
		}
		if constrained && ifMatch != g.revision {
			return errRevisionConstraint(ifMatch, g.revision)
		}
		changed := false
		if update.Name != nil && *update.Name != g.name {
			g.name, changed = *update.Name, true
		}
		if update.Description != nil && *update.Description != g.description {
			g.description, changed = *update.Description, true
		}
		if update.Stateful != nil && *update.Stateful != g.stateful {
			g.stateful, changed = *update.Stateful, true
		}
		if !changed {
			return nil
		}
		g.updatedAt = time.Now()
		return tx.QueryRow(ctx, `
			UPDATE security_groups
			   SET name = $1, description = $2, stateful = $3, updated_at = $4, revision_number = revision_number + 1
			 WHERE id = $5
			RETURNING revision_number`,
			g.name, g.description, g.stateful, g.updatedAt, g.id).Scan(&g.revision)
	})
	var osErr *common.OpenStackError
	if errors.As(err, &osErr) {
		common.SendError(c, osErr)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update security group"))
		return
	}

	body, err := renderSecurityGroup(ctx, svc.pool, g)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to read the updated security group"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"security_group": body})
}

// CreateSecurityGroupRuleRequest represents a security group rule creation request
type CreateSecurityGroupRuleRequest struct {
	SecurityGroupRule securityGroupRule `json:"security_group_rule"`
}

// securityGroupOwnedByCaller reports whether a security group belongs to the caller's
// project — or, for an administrator, exists in any project (SPEC-033 FR-058). It is the ownership gate for the rule handlers, which address a rule (or its
// group) by id supplied in the request and would otherwise operate on any tenant's
// firewall state.
//
// A group in another project is reported as absent, not forbidden: 403 would confirm the
// id exists, which is itself the cross-tenant disclosure we are closing. Both ids are
// compared as text so a malformed id simply fails to match instead of raising
// "invalid input syntax for type uuid" as a 500 — that includes the empty project id an
// unscoped token carries.
func securityGroupOwnedByCaller(ctx context.Context, pool subnetQuerier, sgID, projectID string, admin bool) (bool, error) {
	var owned bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM security_groups WHERE id::text = $1 AND "+ownerOrAdmin("project_id::text", "$2", "$3")+")",
		sgID, projectID, admin).Scan(&owned)
	return owned, err
}

// CreateSecurityGroupRule creates a new security group rule
func (svc *Service) CreateSecurityGroupRule(c *gin.Context) {
	var req CreateSecurityGroupRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	// The rule as upstream stores it, or upstream's refusal, before anything is read or written: its
	// remote and protocol go into iptables-restore's input on every node binding a port with the
	// group (SPEC-033 FR-059).
	rule, refusal := req.SecurityGroupRule.checked()
	if refusal != nil {
		common.SendError(c, refusal)
		return
	}

	// The group ids are well formed before anything is read, as upstream's type:uuid validation has it
	// (400). Whether the groups are the caller's — the group, and the remote group whose members the
	// rule admits — the transaction that stores the rule answers (404, upstream's SecurityGroupNotFound),
	// so another project's group is never written into, nor stored as a cross-tenant reference.
	for _, id := range []struct {
		field string
		value *string
	}{{"security_group_id", &rule.SecurityGroupID}, {"remote_group_id", rule.RemoteGroupID}} {
		if id.value != nil && uuid.Validate(*id.value) != nil {
			common.SendError(c, common.NewBadRequestError(id.field+" is not a valid security group id"))
			return
		}
	}

	ruleID := uuid.New().String()

	// The optional attributes stay POINTERS all the way to storage and to the response.
	//
	// They arrive as pointers already (the request struct above), and flattening them to ""/0
	// sentinels here is what made this the fourth and only disagreeing emitter: unset came back as
	// "" and 0 while every read reported null, and — worse — `Valid: portMin > 0` stored a deliberate
	// port 0, a legitimate ICMP type, as NULL (SPEC-024 / cloud-storage#546 B9, FR-026-028).
	protocol := rule.Protocol
	portMin := rule.PortRangeMin
	portMax := rule.PortRangeMax
	remoteIP := rule.RemoteIPPrefix
	remoteGroup := rule.RemoteGroupID

	etherType := *rule.EtherType

	// One transaction makes the checks and stores the rule (constitution v1.5.0, limit 2): the group,
	// and the remote group when named, both the caller's. No lock: a group's project never changes,
	// and the foreign keys re-check that both still exist at the insert (SPEC-033 research R21).
	now := time.Now()
	sgID := rule.SecurityGroupID
	var revision int
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		groups := []string{sgID}
		if remoteGroup != nil {
			groups = append(groups, *remoteGroup)
		}
		for _, g := range groups {
			owned, err := securityGroupOwnedByCaller(c.Request.Context(), tx, g, c.GetString("project_id"), isAdmin(c))
			if err != nil {
				return err
			}
			if !owned {
				return common.NewNotFoundError("security group")
			}
		}
		_, err := tx.Exec(c.Request.Context(), `
			INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix, remote_group_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, ruleID, sgID, rule.Direction, etherType,
			nullStringFrom(protocol), nullInt32From(portMin), nullInt32From(portMax),
			nullStringFrom(remoteIP), nullStringFrom(remoteGroup),
			now)
		if err != nil {
			return err
		}
		// A rule added revises its group, as upstream's (FR-050).
		revision, err = bumpRevision(c.Request.Context(), tx, sgID)
		return err
	})
	if err != nil {
		sendRuleStoreError(c, err, "create")
		return
	}

	// Stored. Every filter of a port carrying the group is rebuilt before the answer; when one
	// cannot be, the rule is removed again, and the revision put back with it (SPEC-033 FR-008b, FR-050).
	ctx := context.WithoutCancel(c.Request.Context())
	if err := svc.convergeGroupRules(ctx, sgID); err != nil {
		if uerr := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
			tag, err := tx.Exec(ctx, "DELETE FROM security_group_rules WHERE id = $1", ruleID)
			if err != nil || tag.RowsAffected() == 0 {
				return err // deleted meanwhile, with its group or by another request: nothing to put back
			}
			return unbumpRevision(ctx, tx, sgID, revision)
		}); uerr != nil {
			log.Error().Err(uerr).Str("rule_id", ruleID).Msg("remove a rule the node could not apply; the reconciliation pass applies it")
		}
		if rerr := svc.convergeGroupRules(ctx, sgID); rerr != nil {
			log.Error().Err(rerr).Str("sg_id", sgID).Msg("rebuild the filters after an undo; the reconciliation pass converges them")
		}
		log.Error().Err(err).Str("rule_id", ruleID).Msg("this node could not apply the rule; the change was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the security group rule"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"security_group_rule": gin.H{
			"id":                ruleID,
			"security_group_id": rule.SecurityGroupID,
			"direction":         rule.Direction,
			"ethertype":         etherType,
			"protocol":          protocol,
			"port_range_min":    portMin,
			"port_range_max":    portMax,
			"remote_ip_prefix":  remoteIP,
			"remote_group_id":   remoteGroup,
			"created_at":        now.Format(time.RFC3339),
			// The five above are pointers: a nil marshals to JSON null, which is what the group
			// list, the rule list and the rule-by-id view have always reported (B9).
		},
	})
}

// ListSecurityGroupRules lists all security group rules
func (svc *Service) ListSecurityGroupRules(c *gin.Context) {
	if err := svc.ensureDefaultOf(c.Request.Context(), listedProject(c)); err != nil {
		sendChangeError(c, err, "list", "security group rules")
		return
	}
	// A rule is its group's project's: the group's project scopes the listing, every project's for an
	// administrator, and project_id/tenant_id narrow it (SPEC-033 FR-057); upstream's attribute filters
	// narrow it further (SPEC-033 T194).
	f := &common.SQLFilter{}
	projectScope(c, f, "sg.project_id", ownedBy("sg.project_id"))
	if refusal := filterByColumns(c, f, securityGroupRuleFilters...); refusal != nil {
		common.SendError(c, refusal)
		return
	}
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT sgr.id, sgr.security_group_id, sgr.direction, sgr.ethertype, sgr.protocol, sgr.port_range_min, sgr.port_range_max, sgr.remote_ip_prefix, sgr.remote_group_id, sgr.created_at
		FROM security_group_rules sgr
		JOIN security_groups sg ON sg.id = sgr.security_group_id`+f.Where()+`
		ORDER BY sgr.created_at DESC
	`, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_sg_rules").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security group rules"))
		return
	}
	defer rows.Close()

	var rules []gin.H
	for rows.Next() {
		// Nullable columns scan into pointers, matching securityGroupRulesByGroup and
		// GetSecurityGroupRule: a nil pointer marshals to JSON null, so the full key
		// set falls out without adapters.
		var id, sgID, direction, etherType string
		var protocol, remoteIP, remoteGroup *string
		var portMin, portMax *int
		var createdAt time.Time

		if err := rows.Scan(&id, &sgID, &direction, &etherType, &protocol, &portMin, &portMax, &remoteIP, &remoteGroup, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan security group rule row")
			continue
		}

		// Full key set with explicit nulls, as in securityGroupRulesByGroup — an
		// absent key is a KeyError in every client that indexes the rule directly.
		rules = append(rules, gin.H{
			"id":                id,
			"security_group_id": sgID,
			"direction":         direction,
			"ethertype":         etherType,
			"protocol":          protocol,
			"port_range_min":    portMin,
			"port_range_max":    portMax,
			"remote_ip_prefix":  remoteIP,
			"remote_group_id":   remoteGroup,
			"created_at":        createdAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if rules == nil {
		rules = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"security_group_rules": rules})
}

// GetSecurityGroupRule returns a single security group rule (the CLI GETs the
// rule before deleting it).
func (svc *Service) GetSecurityGroupRule(c *gin.Context) {
	ruleID := c.Param("id")

	var id, sgID, direction, ethertype string
	var protocol, remoteIPPrefix, remoteGroupID *string
	var portRangeMin, portRangeMax *int
	var createdAt time.Time

	// Scoped by the owning group's project, exactly as ListSecurityGroupRules is: a rule
	// carries protocol, port ranges and remote CIDRs, so an unscoped lookup hands another
	// tenant's firewall configuration to any authenticated caller. A rule outside the
	// caller's project is indistinguishable from one that does not exist (ErrNoRows → 404).
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT sgr.id, sgr.security_group_id, sgr.direction, sgr.ethertype, sgr.protocol,
		       sgr.port_range_min, sgr.port_range_max, sgr.remote_ip_prefix, sgr.remote_group_id, sgr.created_at
		FROM security_group_rules sgr
		JOIN security_groups sg ON sg.id = sgr.security_group_id
		WHERE sgr.id::text = $1 AND `+ownerOrAdmin("sg.project_id::text", "$2", "$3")+`
	`, ruleID, c.GetString("project_id"), isAdmin(c)).Scan(&id, &sgID, &direction, &ethertype, &protocol, &portRangeMin, &portRangeMax,
		&remoteIPPrefix, &remoteGroupID, &createdAt)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("security group rule"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_sg_rule").Str("rule_id", ruleID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group rule"))
		return
	}

	// Full key set with explicit nulls, as in securityGroupRulesByGroup.
	c.JSON(http.StatusOK, gin.H{"security_group_rule": gin.H{
		"id":                id,
		"security_group_id": sgID,
		"direction":         direction,
		"ethertype":         ethertype,
		"protocol":          protocol,
		"port_range_min":    portRangeMin,
		"port_range_max":    portRangeMax,
		"remote_ip_prefix":  remoteIPPrefix,
		"remote_group_id":   remoteGroupID,
		"created_at":        createdAt.Format(time.RFC3339),
	}})
}

// DeleteSecurityGroupRule deletes a security group rule
func (svc *Service) DeleteSecurityGroupRule(c *gin.Context) {
	ruleID := c.Param("id")
	projectID := c.GetString("project_id")

	// One transaction reads the whole row, locked and scoped to the caller's project — a rule outside it
	// reads as absent, since one request must never open or close another tenant's firewall — and
	// deletes it. The row is kept for the undo (SPEC-033 research R21). A concurrent delete surfaces as
	// 404 rather than a misleading 204.
	var row ruleRecord
	var revision int
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		var err error
		row, revision, err = deleteRuleRecord(c.Request.Context(), tx, ruleID, projectID, isAdmin(c))
		return err
	})
	if err != nil {
		sendRuleStoreError(c, err, "delete")
		return
	}

	// Stored. Every filter of a port carrying the group is rebuilt before the answer; when one
	// cannot be, the rule is restored, and the revision put back with it (SPEC-033 FR-008b, FR-050).
	ctx := context.WithoutCancel(c.Request.Context())
	if err := svc.convergeGroupRules(ctx, row.group); err != nil {
		if rerr := svc.restoreRule(ctx, row, revision); rerr != nil {
			log.Error().Err(rerr).Str("rule_id", row.id).Msg("restore a rule the node could not remove; the reconciliation pass applies what the records hold")
		}
		if rerr := svc.convergeGroupRules(ctx, row.group); rerr != nil {
			log.Error().Err(rerr).Str("sg_id", row.group).Msg("rebuild the filters after an undo; the reconciliation pass converges them")
		}
		log.Error().Err(err).Str("rule_id", row.id).Msg("this node could not remove the rule; the change was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the security group rule"))
		return
	}

	c.Status(http.StatusNoContent)
}

// deleteRuleRecord deletes a rule in the caller's transaction and returns its row, for the undo, and its
// group's revision after the delete revised it (FR-050). The group's lock comes first, by that revision,
// then the rule's: the order a group's delete takes them in, the group and then its rules through the
// cascade, so the two cannot deadlock (SPEC-033 T250). A rule or group deleted meanwhile is 404.
func deleteRuleRecord(ctx context.Context, tx database.Tx, ruleID, projectID string, admin bool) (ruleRecord, int, error) {
	var row ruleRecord
	notFound := common.NewNotFoundError("security group rule")
	err := tx.QueryRow(ctx, `
		SELECT security_group_id::text FROM security_group_rules
		 WHERE id::text = $1
		   AND security_group_id IN (SELECT id FROM security_groups WHERE `+ownerOrAdmin("project_id::text", "$2", "$3")+`)`,
		ruleID, projectID, admin).Scan(&row.group)
	if errors.Is(err, database.ErrNoRows) {
		return row, 0, notFound
	}
	if err != nil {
		return row, 0, err
	}
	revision, err := bumpRevision(ctx, tx, row.group)
	var missing *common.OpenStackError
	if errors.As(err, &missing) {
		return row, 0, notFound
	}
	if err != nil {
		return row, 0, err
	}
	err = tx.QueryRow(ctx, `
		SELECT id::text, security_group_id::text, direction, ethertype, protocol, port_range_min, port_range_max,
		       remote_ip_prefix, remote_group_id::text, created_at
		  FROM security_group_rules WHERE id::text = $1 AND security_group_id::text = $2
		FOR UPDATE`, ruleID, row.group,
	).Scan(&row.id, &row.group, &row.direction, &row.ethertype, &row.protocol, &row.portMin, &row.portMax,
		&row.remoteIP, &row.remoteGroup, &row.created)
	if errors.Is(err, database.ErrNoRows) {
		return row, 0, notFound
	}
	if err != nil {
		return row, 0, err
	}
	_, err = tx.Exec(ctx, "DELETE FROM security_group_rules WHERE id = $1", row.id)
	return row, revision, err
}

// ruleRecord is a rule's whole row, kept to restore a deletion the node could not apply.
type ruleRecord struct {
	id, group, direction, ethertype string
	protocol, remoteIP, remoteGroup sql.NullString
	portMin, portMax                sql.NullInt32
	created                         time.Time
}

// restoreRule re-inserts a deleted rule with every column as it was, if its group, and its remote
// group when it names one, still exist and its id is free: a rule whose group went meanwhile would
// have gone with it (research R21). The group's revision, which the deletion bumped to bumped, is put
// back compare-and-set with it (FR-050).
func (svc *Service) restoreRule(ctx context.Context, r ruleRecord, bumped int) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM security_groups WHERE id::text = $1)
			   AND ($2::text IS NULL OR EXISTS(SELECT 1 FROM security_groups WHERE id::text = $2))
			   AND NOT EXISTS(SELECT 1 FROM security_group_rules WHERE id::text = $3)`,
			r.group, r.remoteGroup, r.id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix, remote_group_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			r.id, r.group, r.direction, r.ethertype, r.protocol, r.portMin, r.portMax, r.remoteIP, r.remoteGroup, r.created); err != nil {
			return err
		}
		return unbumpRevision(ctx, tx, r.group, bumped)
	})
}

// sendRuleStoreError answers a rule change whose transaction failed: a refusal as it is, a group
// deleted meanwhile (a foreign-key violation) as 404, anything else as 500.
func sendRuleStoreError(c *gin.Context, err error, operation string) {
	if database.IsForeignKeyViolation(err) {
		common.SendError(c, common.NewNotFoundError("security group"))
		return
	}
	sendChangeError(c, err, operation, "security group rule")
}

// PortInfo represents port allocation information for cross-service use
type PortInfo struct {
	ID        string
	NetworkID string
	MAC       string
	IPAddress string
	SubnetID  string
}

// ExternalNetworkAttachForbiddenError refuses a guest an interface on an unshared external network.
// It is upstream Nova's ExternalNetworkAttachForbidden, in its wording, and compute answers it with
// the 403 upstream's create handler gives it. Naming the network discloses nothing: an external
// network is visible to every project (FR-033a).
type ExternalNetworkAttachForbiddenError struct {
	NetworkID string
}

func (e *ExternalNetworkAttachForbiddenError) Error() string {
	return "It is not allowed to create an interface on external network " + e.NetworkID
}

// TenantMessage marks the refusal as fit for the tenant; see tenantVisibleError.
func (e *ExternalNetworkAttachForbiddenError) TenantMessage() string { return e.Error() }

// tenantVisibleError is a failure whose text the tenant may read: it names a condition of their
// network they can act on, and nothing about how the service is built. Compute shows a failed boot's
// reason in the server's own fault, so it shows only these as worded; anything else — a database or
// host failure — reaches the log alone. Upstream does the same: a non-Nova exception's fault records
// its class name, never its text (nova/compute/utils.py exception_to_dict). A wrapped database error
// used to reach `server show`.
type tenantVisibleError struct{ msg string }

func (e *tenantVisibleError) Error() string { return e.msg }

// TenantMessage marks the error as fit for the tenant. Compute asserts the behaviour rather than the
// type.
func (e *tenantVisibleError) TenantMessage() string { return e.msg }

// errNoSuchNetwork refuses every other network a guest may not use, in the same words whether the
// network is absent or belongs to someone else: telling the two apart confirms that another
// project's network exists.
var errNoSuchNetwork error = &tenantVisibleError{msg: "no such network"}

// guestMayAttach is GuestMayAttach on this service's pool.
func (svc *Service) guestMayAttach(ctx context.Context, networkID, projectID string, admin bool) error {
	return GuestMayAttach(ctx, svc.pool, networkID, projectID, admin)
}

// GuestMayAttach decides whether a guest of projectID may have an interface on networkID; admin is
// whether the caller holds the administrator role. It is the one rule every path that plugs a guest
// into a network applies: the boot pre-flight (NetworkUsableBy), the allocation it vouches for
// (AllocatePortForInstance), and compute's interface attach — so none of them can disagree
// (SPEC-025 FR-016a). They did while the pre-flight borrowed the read scope, and while the
// interface attach accepted the server's own networks only.
//
// It is narrower than that scope, because seeing a network is not being allowed to plug into it.
// Upstream's rule is _check_external_network_attach (nova/network/neutron.py) under the policy
// network:attach_external_network, admin by default:
//
//   - a shared network takes anyone's guest, a shared external network included;
//   - an unshared external network takes only an administrator's, whoever owns it;
//   - any other network takes only its owner's. Upstream would also let an administrator onto
//     another project's private network; Lightstack does not.
func GuestMayAttach(ctx context.Context, pool database.DB, networkID, projectID string, admin bool) error {
	var owned, shared, external bool
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(project_id = $2, false), COALESCE(shared, false), external
		   FROM networks WHERE id = $1`,
		networkID, projectID,
	).Scan(&owned, &shared, &external)
	if errors.Is(err, database.ErrNoRows) {
		return errNoSuchNetwork
	}
	if err != nil {
		return fmt.Errorf("checking network availability: %w", err)
	}
	switch {
	case shared:
		return nil
	case external:
		if admin {
			return nil
		}
		return &ExternalNetworkAttachForbiddenError{NetworkID: networkID}
	case owned:
		return nil
	default:
		return errNoSuchNetwork
	}
}

// NetworkUsableBy reports whether a project may attach a guest to a network, naming the reason
// when it may not (SPEC-025 FR-017); admin is whether the caller holds the administrator role.
//
// Compute calls this before creating anything, so a wrong or unusable network id is a synchronous
// refusal rather than a boot that fails in the background. The rule is guestMayAttach, which the
// allocation applies again.
func (svc *Service) NetworkUsableBy(ctx context.Context, networkID, projectID string, admin bool) error {
	return svc.guestMayAttach(ctx, networkID, projectID, admin)
}

// InstancePort is a boot's request for one of its server's ports: the network, the server's project and
// the server; the groups the server was created with, already resolved and validated by Nova, empty for
// "none requested", which gets the project default; or, CopyGroupsFrom, the boot's first port, whose
// current groups this port then carries instead, so a change accepted between the two allocations
// reaches it (SPEC-033 FR-011, research R11) — a first port without a group gives none, not the default;
// and whether the caller is an administrator.
type InstancePort struct {
	NetworkID, ProjectID, InstanceID string
	SecurityGroupIDs                 []string
	CopyGroupsFrom                   string
	Admin                            bool
}

// AllocatePortForInstance creates a port for a VM instance (called from Nova).
func (svc *Service) AllocatePortForInstance(ctx context.Context, req InstancePort) (any, error) {
	networkID, projectID, instanceID, copyGroupsFrom := req.NetworkID, req.ProjectID, req.InstanceID, req.CopyGroupsFrom
	portID := uuid.New().String()
	macAddress := generateMAC()

	// The rule the boot pre-flight already applied, applied again because this is the attach
	// itself; upstream re-checks on every allocation for the same reason.
	if err := svc.guestMayAttach(ctx, networkID, projectID, req.Admin); err != nil {
		if errors.Is(err, errNoSuchNetwork) {
			return nil, &tenantVisibleError{msg: fmt.Sprintf("network %s not found", networkID)}
		}
		return nil, err
	}

	// Get subnet to allocate IP
	var subnetID, cidr string
	err := svc.pool.QueryRow(ctx,
		"SELECT id, cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		networkID,
	).Scan(&subnetID, &cidr)

	// The two cases are not the same answer: no subnet is something the tenant can fix, so it is
	// marked for them and shown in the server's fault (SPEC-025 FR-018); a failed query is
	// something only an operator can, so its text stays out of it. Surfacing database.ErrNoRows used
	// to put "no rows in result set" in front of a tenant.
	if errors.Is(err, database.ErrNoRows) {
		return nil, &tenantVisibleError{msg: fmt.Sprintf("no subnet found for network %s", networkID)}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up a subnet for network %s: %w", networkID, err)
	}

	var allocatedIP string

	// The security groups the instance was created with. port_security_groups is what
	// the port's firewall is rendered from on its node (SPEC-033 R32), so a port carrying
	// the wrong group is filtered by the wrong rules, whatever Nova reports back
	// (cloud-storage#579).
	// Falls back to the project's default group when none were requested (OpenStack:
	// an instance's port gets the default SG unless overridden). Ensured, not looked
	// up: a fresh project has no default group until its first use. Resolved here,
	// before any host-side work, because it is pure database access and its own
	// transaction (EnsureDefaultSecurityGroup) cannot nest in the one below.
	// A later port of a boot copies the first port's groups instead, inside the transaction below.
	portSGs := req.SecurityGroupIDs
	if copyGroupsFrom == "" && len(portSGs) == 0 {
		defaultSG, err := EnsureDefaultSecurityGroup(ctx, svc.pool, projectID)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure default security group: %w", err)
		}
		portSGs = []string{defaultSG}
	}

	// No device is made here: libvirt makes the guest's TAP, named for the port, when Nova boots the
	// guest after binding the port, and the binding's node makes the network's bridge (SPEC-033 FR-041,
	// FR-045, R32).

	// Insert the port and its security-group associations atomically: a port that
	// exists with a subset of its groups is filtered by fewer rules than Nova
	// reports, so a failed association fails the allocation and takes the port row
	// with it. The caller (Nova CreateServer) then boots without this network,
	// which keeps the API and the dataplane in agreement.
	now := time.Now()
	var created time.Time
	if err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		// The instance first, then the subnet (research R3). A change to the server's groups holds
		// the instance while it changes the ports, so the copy below reads its committed result.
		// No row is not an error: callers outside a boot allocate for ids that have none.
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 FOR UPDATE", instanceID).Scan(&one); err != nil && !errors.Is(err, database.ErrNoRows) {
			return fmt.Errorf("lock instance %s: %w", instanceID, err)
		}
		if copyGroupsFrom != "" {
			if portSGs, err = PortSecurityGroupIDs(ctx, tx, copyGroupsFrom); err != nil {
				return fmt.Errorf("read the groups of port %s: %w", copyGroupsFrom, err)
			}
		}
		if _, err = LockSubnet(ctx, tx, subnetID); err != nil {
			return err
		}
		allocatedIP, err = AllocateFreeIP(ctx, tx, subnetID)
		if errors.Is(err, ErrNoAvailableIP) {
			return &tenantVisibleError{msg: fmt.Sprintf("failed to allocate a free IP from subnet %s", subnetID)}
		}
		if err != nil {
			return err
		}
		fixedIPsJSON, _ := json.Marshal([]map[string]any{{"subnet_id": subnetID, "ip_address": allocatedIP}})

		if _, err := tx.Exec(ctx, `
			INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, portID, fmt.Sprintf("port-for-%s", instanceID[:8]), networkID, projectID, instanceID,
			"compute:nova", macAddress, true, "ACTIVE", fixedIPsJSON, now, now); err != nil {
			return fmt.Errorf("insert port: %w", err)
		}
		for _, sgID := range portSGs {
			if _, err := tx.Exec(ctx,
				"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
				portID, sgID); err != nil {
				return fmt.Errorf("associate security group %s: %w", sgID, err)
			}
		}
		// A boot whose groups mix both kinds fails here, as upstream's port creation does; the
		// server ends ERROR with this as its fault (FR-051).
		if err := requireOneStatefulness(ctx, tx, portID); err != nil {
			return err
		}
		if svc.fdb != nil {
			if err := svc.fdb.RecordFDB(ctx, tx, networkID, portID, macAddress); err != nil {
				return err
			}
		}
		// What this allocation left the port at, so that its undo can tell it unchanged (FR-008).
		return tx.QueryRow(ctx, "SELECT updated_at FROM ports WHERE id = $1", portID).Scan(&created)
	}); err != nil {
		return nil, fmt.Errorf("failed to insert port into database: %w", err)
	}

	// The port's DHCP reservation, on the network node before the guest boots, so it leases the address
	// Neutron gave its port, and the firewall of the ports whose rules expand its groups (SPEC-033
	// FR-008, R29, R32). An allocation they cannot apply is undone compare-and-set and fails the boot.
	dhcp := servesDHCP(ctx, svc.pool, common.FixedIPs{{SubnetID: subnetID}})
	if err := svc.applyPortCreate(ctx, networkID, dhcp, portSGs); err != nil {
		svc.undoPortCreate(ctx, portID, created)
		svc.reapplyPortCreate(ctx, networkID, dhcp, portSGs)
		return nil, fmt.Errorf("apply port %s: %w", portID, err)
	}

	return &PortInfo{
		ID:        portID,
		NetworkID: networkID,
		MAC:       macAddress,
		IPAddress: allocatedIP,
		SubnetID:  subnetID,
	}, nil
}

// Small helpers for the three-valued optional attributes of a security-group rule (SPEC-024 B9).
//
// The wire distinguishes absent, an explicit zero/empty, and a real value; the columns are nullable
// and can hold all three. These keep that distinction intact instead of collapsing it into a
// sentinel, which is what made the create response disagree with every read.

// nullStringFrom maps a nil pointer to SQL NULL, and any supplied value — including "" — to itself.
func nullStringFrom(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}

// nullInt32From maps a nil pointer to SQL NULL, and any supplied value — including a deliberate 0,
// a valid ICMP type — to itself. The previous `Valid: portMin > 0` silently discarded that zero.
func nullInt32From(v *int) sql.NullInt32 {
	if v == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*v), Valid: true} //nolint:gosec // port/ICMP values are bounded by validation
}

// derefOr flattens a pointer for consumers that cannot express "unset" — currently only the kernel
// rule builder. Deliberately not used on any storage or response path.
func derefOr[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}
	return *v
}

// ReleaseInstancePorts deletes the ports Neutron allocated for an instance
// (device_owner 'compute:nova'), with the same best-effort host cleanup
// DeletePort does. It is the rollback for a boot that allocated ports and then
// failed on a later network: the loop used to
// markInstanceError and return with the earlier networks' ports still in the
// database — rows the API still listed for a server that would never exist,
// with leases still reserved, that only an operator could clean up.
//
// Scoped to 'compute:nova' on purpose: a port the tenant created and attached
// (os-interface with port_id) is theirs, not the instance's — releasing a
// failed boot must not delete it (same discriminator as server delete).
func (svc *Service) ReleaseInstancePorts(ctx context.Context, instanceID string) error {
	ids, err := queryStrings(ctx, svc.pool, `
		SELECT id::text FROM ports WHERE device_id = $1 AND device_owner = 'compute:nova' ORDER BY id`, instanceID)
	if err != nil {
		return fmt.Errorf("looking up instance ports: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	// The ports leave their units before their records go (SPEC-033 FR-040, R34); when that or the
	// release fails, the ports stay and the units are converged from the records again.
	restore, err := svc.WithdrawPorts(ctx, ids)
	if err != nil {
		return fmt.Errorf("withdraw server %s's ports: %w", instanceID, err)
	}

	// One transaction: the instance, then its ports in id order, as every change to a server's ports
	// takes them (research R3); then the ports' floating IPs' association; then the ports (SPEC-033
	// research R25). No instance row means the server was deleted meanwhile: the release carries on
	// with whatever ports remain.
	var floatingIPs []FloatingIPAssociation
	if err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 FOR UPDATE", instanceID).Scan(&one); err != nil && !errors.Is(err, database.ErrNoRows) {
			return fmt.Errorf("lock instance %s: %w", instanceID, err)
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text FROM ports WHERE device_id = $1 AND device_owner = 'compute:nova' ORDER BY id FOR UPDATE`, instanceID)
		if err != nil {
			return fmt.Errorf("lock instance ports: %w", err)
		}
		var ports []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("lock instance ports: %w", err)
			}
			ports = append(ports, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("lock instance ports: %w", err)
		}
		if floatingIPs, err = ReleaseDeletedPorts(ctx, tx, ports); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM ports WHERE device_id = $1 AND device_owner = 'compute:nova'
		`, instanceID); err != nil {
			return fmt.Errorf("deleting instance ports: %w", err)
		}
		return nil
	}); err != nil {
		restore()
		return err
	}
	svc.ConvergeDisassociated(context.WithoutCancel(ctx), floatingIPs)
	return nil
}
