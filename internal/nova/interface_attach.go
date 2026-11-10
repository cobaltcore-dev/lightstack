package nova

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// AttachInterfaceRequest represents a network interface attachment request
type AttachInterfaceRequest struct {
	InterfaceAttachment struct {
		NetID   string `json:"net_id"`   // Network ID to attach to
		PortID  string `json:"port_id"`  // Existing port ID (optional)
		FixedIP string `json:"fixed_ip"` // Specific IP address (optional)
	} `json:"interfaceAttachment"`
}

// AttachInterface attaches a network interface to an instance
func (svc *Service) AttachInterface(c *gin.Context) {
	instanceID, projectID, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}

	var req AttachInterfaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify instance exists
	var libvirtDomainID, status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, ''), status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID, &status)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("interface attach: instance lookup failed")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	var portID string
	var networkID string
	var fixedIP string
	var macAddress string
	// fixedIPEntries is what the response reports as the attachment's
	// fixed_ips: the port's actual entries (port_id path) or the one just
	// allocated (net_id path). Empty means none — not a fabricated one.
	var fixedIPEntries []gin.H
	// novaCreatedPort is true when this request had Nova create the port
	// (net_id path); false when it attached a port the tenant already owned
	// (port_id path). It decides the device_owner stamp below (N-8).
	novaCreatedPort := false

	// If port_id provided, use existing port
	if req.InterfaceAttachment.PortID != "" {
		portID = req.InterfaceAttachment.PortID

		// Verify port exists and is available. device_id is nullable — every
		// free port has NULL there — so it is read COALESCE'd rather than
		// scanned into a plain string (round 3 of this review): that scan
		// fails on NULL, and the error used to be dropped, leaving the attach
		// "correct" only because a failed Scan invalidates the lookup, even if some earlier columns were read. Any real database error on this lookup surfaced the same way:
		// a bogus 200 with an empty net_id and MAC.
		var deviceID string
		var fixedIPsJSON []byte
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT network_id, fixed_ips, mac_address, COALESCE(device_id::text, '') FROM ports WHERE id = $1 AND project_id = $2",
			portID, projectID,
		).Scan(&networkID, &fixedIPsJSON, &macAddress, &deviceID)

		if errors.Is(err, database.ErrNoRows) {
			common.SendError(c, common.NewNotFoundError("port"))
			return
		}
		if err != nil {
			log.Error().Err(err).Str("operation", "attach_interface").Str("port_id", portID).Msg("port lookup failed")
			common.SendError(c, common.NewInternalServerError("database error"))
			return
		}

		if deviceID != "" {
			common.SendError(c, common.NewConflictError("port already attached to another device"))
			return
		}

		// The port's actual fixed_ips, for the response and for nothing else:
		// its addressing lives in the JSONB, and a hardcoded placeholder used
		// to be reported instead (round 2 of this review). A port with no
		// addresses reports an empty list, as upstream does — not a fabricated
		// {"ip_address": ""}.
		fixedIPEntries = parseFixedIPEntries(fixedIPsJSON)
	} else if req.InterfaceAttachment.NetID != "" {
		// Create new port on the specified network
		networkID = req.InterfaceAttachment.NetID
		novaCreatedPort = true

		// The rule booting applies (neutron.GuestMayAttach): the server's own networks and shared
		// ones, and an unshared external network for administrators only — upstream applies
		// network:attach_external_network to os-interface exactly as to boot. This used to accept
		// the server's own networks alone. Answered as upstream's os-interface answers: 404 for a
		// network the caller may not use, 403 for an external one, 500 for a check that failed.
		if err := neutron.GuestMayAttach(c.Request.Context(), svc.pool, networkID, projectID,
			common.HasRole(c, common.RoleAdmin)); err != nil {
			var forbidden *neutron.ExternalNetworkAttachForbiddenError
			var visible tenantMessager
			switch {
			case errors.As(err, &forbidden):
				common.SendError(c, common.NewForbiddenError(forbidden.Error()))
			case errors.As(err, &visible):
				common.SendError(c, common.NewNotFoundError("network"))
			default:
				log.Error().Err(err).Str("network_id", networkID).Msg("interface attach: checking the network failed")
				common.SendError(c, common.NewInternalServerError("failed to check the network"))
			}
			return
		}

		// Generate port ID and MAC address
		portID = uuid.New().String()
		macAddress = generateMACAddress()

		// Resolve the subnet containing an explicit address. Networks can have
		// both IPv4 and IPv6 subnets, so an arbitrary first subnet can reject a
		// valid address from the other family.
		subnetRows, err := svc.pool.Query(c.Request.Context(),
			"SELECT id, cidr FROM subnets WHERE network_id = $1 ORDER BY id", networkID)
		if err != nil {
			log.Error().Err(err).Str("operation", "allocate_ip").Msg("subnet lookup error")
			common.SendError(c, common.NewInternalServerError("failed to allocate IP"))
			return
		}
		type subnetInfo struct{ id, cidr string }
		var subnets []subnetInfo
		for subnetRows.Next() {
			var subnet subnetInfo
			if err := subnetRows.Scan(&subnet.id, &subnet.cidr); err != nil {
				subnetRows.Close()
				log.Error().Err(err).Str("operation", "allocate_ip").Msg("subnet scan error")
				common.SendError(c, common.NewInternalServerError("failed to allocate IP"))
				return
			}
			subnets = append(subnets, subnet)
		}
		rowsErr := subnetRows.Err()
		subnetRows.Close()
		if rowsErr != nil {
			log.Error().Err(rowsErr).Str("operation", "allocate_ip").Msg("subnet iteration error")
			common.SendError(c, common.NewInternalServerError("failed to allocate IP"))
			return
		}
		if len(subnets) == 0 {
			common.SendError(c, common.NewBadRequestError("network has no subnet"))
			return
		}
		subnetID, cidr := subnets[0].id, subnets[0].cidr

		// Use provided fixed_ip or allocate one
		if req.InterfaceAttachment.FixedIP != "" {
			fixedIP = req.InterfaceAttachment.FixedIP
			// A caller-specified address is validated, not trusted (round 3
			// of this review): it must parse and lie in the subnet (400), and
			// no other port may hold it (409, upstream's "IP address already
			// allocated") — the explicit side of the N-4 collision rule the
			// allocator covers for generated addresses. Parsing also keeps
			// arbitrary strings out of the JSONB build below.
			ip := net.ParseIP(fixedIP)
			if ip == nil {
				common.SendError(c, common.NewBadRequestError("fixed_ip is not a valid address in the network's subnet"))
				return
			}
			matchedSubnet := false
			for _, subnet := range subnets {
				_, ipNet, cidrErr := net.ParseCIDR(subnet.cidr)
				if cidrErr == nil && ipNet.Contains(ip) {
					subnetID, cidr = subnet.id, subnet.cidr
					matchedSubnet = true
					break
				}
			}
			if !matchedSubnet {
				common.SendError(c, common.NewBadRequestError("fixed_ip is not a valid address in the network's subnet"))
				return
			}
			fixedIP = ip.String()
		}

		// Allocate/check and insert while holding the subnet lock shared with
		// Neutron's port-create and server-boot paths.
		err = database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
			cidr, err = neutron.LockSubnet(c.Request.Context(), tx, subnetID)
			if err != nil {
				return err
			}
			if fixedIP != "" {
				available, err := neutron.IPAvailable(c.Request.Context(), tx, subnetID, fixedIP)
				if err != nil {
					return err
				}
				if !available {
					return neutron.ErrNoAvailableIP
				}
			} else {
				fixedIP, err = neutron.AllocateFreeIP(c.Request.Context(), tx, subnetID, cidr)
				if err != nil {
					return err
				}
			}
			fixedIPEntries = []gin.H{{"ip_address": fixedIP, "subnet_id": subnetID}}
			fixedIPsJSON, _ := json.Marshal(fixedIPEntries)
			_, err = tx.Exec(c.Request.Context(), `
				INSERT INTO ports (id, network_id, project_id, name, mac_address, fixed_ips, device_id, device_owner, status, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11)
			`, portID, networkID, projectID, fmt.Sprintf("port-%s", portID[:8]), macAddress, fixedIPsJSON, instanceID, "compute:nova", "ACTIVE", time.Now(), time.Now())
			return err
		})
		if err != nil {
			if errors.Is(err, neutron.ErrNoAvailableIP) {
				message := "no IP address available in the network's subnet"
				if req.InterfaceAttachment.FixedIP != "" {
					message = "IP address already allocated in the subnet"
				}
				common.SendError(c, common.NewConflictError(message))
				return
			}
			log.Error().Err(err).Str("operation", "create_port").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to create port"))
			return
		}
	} else {
		common.SendError(c, common.NewBadRequestError("either net_id or port_id must be provided"))
		return
	}

	// Attach the port to the instance. device_owner is stamped
	// 'compute:nova' only on the port Nova itself just created — that stamp
	// is the ownership discriminator server delete and the boot-failure
	// rollback key on: a tenant's port attached by id
	// keeps its own device_owner, so deleting the server DETACHES it instead
	// of destroying it — upstream's deallocate_for_instance skips ports it
	// did not create for exactly this reason. The old blanket stamp is also
	// what made the tenant-port half of N-8 unreachable through the real
	// attach flow (round 2 of this review).
	var deviceOwner any
	if novaCreatedPort {
		deviceOwner = "compute:nova"
	}
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE ports
		SET device_id = $1, device_owner = COALESCE($2, device_owner), status = $3, updated_at = $4
		WHERE id = $5
	`, instanceID, deviceOwner, "ACTIVE", time.Now(), portID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_port_attach").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to attach port"))
		return
	}

	// Store interface attachment record
	attachmentID := uuid.New().String()
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO interface_attachments (id, instance_id, port_id, mac_address, attached_at)
		VALUES ($1, $2, $3, $4, $5)
	`, attachmentID, instanceID, portID, macAddress, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_interface_attachment").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create interface attachment"))
		return
	}

	// In real mode, would hot-plug interface to libvirt VM
	// For stub mode, just return success

	// Recorded against the owning project once it has happened, so the tenant sees
	// an administrator's change in their own history (SPEC-023 FR-007a).
	recordInstanceAction(c, svc.pool, instanceID, projectID, "attach_interface")

	// The port's actual fixed_ips — entries with their subnet ids, or an
	// empty list for a port with none (round 3 of this review: this used to
	// fabricate exactly one {"ip_address": ""}).
	if fixedIPEntries == nil {
		fixedIPEntries = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{
		"interfaceAttachment": gin.H{
			"id":         attachmentID,
			"port_id":    portID,
			"net_id":     networkID,
			"fixed_ips":  fixedIPEntries,
			"mac_addr":   macAddress,
			"port_state": "ACTIVE",
		},
	})
}

// parseFixedIPEntries reads a port's fixed_ips JSONB into response entries.
// NULL or malformed JSON yields no entries — a port with no addresses reports
// none, never a fabricated one.
func parseFixedIPEntries(fixedIPsJSON []byte) []gin.H {
	var entries []struct {
		IPAddress string `json:"ip_address"`
		SubnetID  string `json:"subnet_id"`
	}
	if err := json.Unmarshal(fixedIPsJSON, &entries); err != nil {
		return nil
	}
	out := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		out = append(out, gin.H{"ip_address": e.IPAddress, "subnet_id": e.SubnetID})
	}
	return out
}

// DetachInterface detaches a network interface from an instance
func (svc *Service) DetachInterface(c *gin.Context) {
	instanceID, ownerProject, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}
	portID := c.Param("port_id")

	// Verify port is attached to this instance. device_id is nullable — a
	// detached port has NULL there — so it is read COALESCE'd rather than
	// scanned into a plain string (same class as the attach-path fix in this
	// review: that scan fails on NULL, the error was dropped, and the "not
	// attached" answer below was right only by accident, with real database
	// errors 409-ing as "not attached" instead of 500-ing).
	var attachedInstanceID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(device_id::text, '') FROM ports WHERE id = $1",
		portID,
	).Scan(&attachedInstanceID)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "detach_interface").Str("port_id", portID).Msg("port lookup failed")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	if attachedInstanceID != instanceID {
		common.SendError(c, common.NewConflictError("port not attached to this instance"))
		return
	}

	// Delete interface attachment record
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM interface_attachments WHERE instance_id = $1 AND port_id = $2",
		instanceID, portID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_interface_attachment").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete interface attachment"))
		return
	}

	// Update port to detach from instance
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE ports
		SET device_id = NULL, device_owner = NULL, status = $1, updated_at = $2
		WHERE id = $3
	`, "DOWN", time.Now(), portID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_port_detach").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to detach port"))
		return
	}

	// In real mode, would hot-unplug interface from libvirt VM
	// For stub mode, just return success

	recordInstanceAction(c, svc.pool, instanceID, ownerProject, "detach_interface")
	c.Status(http.StatusAccepted)
}

// ListInterfaceAttachments lists all network interfaces attached to an instance
func (svc *Service) ListInterfaceAttachments(c *gin.Context) {
	instanceID, _, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}

	// Query all interface attachments
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT ia.id, ia.port_id, p.network_id, p.fixed_ips, ia.mac_address, p.status
		FROM interface_attachments ia
		JOIN ports p ON ia.port_id = p.id
		WHERE ia.instance_id = $1
		ORDER BY ia.attached_at
	`, instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_interface_attachments").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list interface attachments"))
		return
	}
	defer rows.Close()

	var attachments []gin.H
	for rows.Next() {
		var id, portID, networkID, macAddress string
		var fixedIPsJSON []byte
		var portState *string

		if err := rows.Scan(&id, &portID, &networkID, &fixedIPsJSON, &macAddress, &portState); err != nil {
			log.Warn().Err(err).Str("operation", "ListInterfaceAttachments").Msg("failed to scan row")
			continue
		}

		state := "DOWN"
		if portState != nil {
			state = *portState
		}

		// Parse fixed_ips from JSONB. An attachment whose port has no
		// addresses is still an attachment: it is listed with an empty
		// fixed_ips, exactly as the attach response reports it (round 3 of
		// this review — the guard here used to make an attachment invisible
		// in the list the moment it was created).
		fixedIPs := []map[string]any{}
		_ = json.Unmarshal(fixedIPsJSON, &fixedIPs)
		if fixedIPs == nil { // JSON null in the column
			fixedIPs = []map[string]any{}
		}
		attachments = append(attachments, gin.H{
			"id":         id,
			"port_id":    portID,
			"net_id":     networkID,
			"fixed_ips":  fixedIPs,
			"mac_addr":   macAddress,
			"port_state": state,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list interface attachments"))
		return
	}

	if attachments == nil {
		attachments = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"interfaceAttachments": attachments})
}

// generateMACAddress generates a random MAC address
func generateMACAddress() string {
	// Use OpenStack's OUI prefix: fa:16:3e
	return fmt.Sprintf("fa:16:3e:%02x:%02x:%02x",
		uuid.New()[0], uuid.New()[1], uuid.New()[2])
}
