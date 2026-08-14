package neutron

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// CreatePortRequest represents a port creation request
type CreatePortRequest struct {
	Port struct {
		Name           string   `json:"name"`
		NetworkID      string   `json:"network_id" binding:"required"`
		AdminStateUp   *bool    `json:"admin_state_up"`
		DeviceID       string   `json:"device_id"`
		DeviceOwner    string   `json:"device_owner"`
		SecurityGroups []string `json:"security_groups"` // Security group IDs
	} `json:"port"`
}

// CreatePort creates a new port
func (svc *Service) CreatePort(c *gin.Context) {
	var req CreatePortRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	portID := uuid.New().String()
	tapName := "tap-" + portID[:8]
	macAddress := generateMAC()

	adminStateUp := true
	if req.Port.AdminStateUp != nil {
		adminStateUp = *req.Port.AdminStateUp
	}

	// Get network and subnet info to allocate IP
	var networkID string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id FROM networks WHERE id = $1 AND (project_id = $2 OR shared = true)",
		req.Port.NetworkID, projectID,
	).Scan(&networkID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}
	// Any OTHER error had to be handled too (second review, AGY-1). A network_id that is not a UUID
	// fails the cast rather than matching no row, so it is not ErrNoRows — and going unchecked it
	// fell through with networkID empty, all the way to `"br-" + req.Port.NetworkID[:8]`, which
	// panicked on anything shorter than 8 characters. AllocatePortForInstance, which runs the very
	// same lookup for the instance-boot path, has always checked both; the two disagreeing is the
	// asymmetry #546 noted between these paths.
	if err != nil {
		log.Warn().Err(err).Str("network_id", req.Port.NetworkID).Msg("port create: unusable network_id")
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}

	// Get subnet to allocate IP
	var subnetID, cidr string
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT id, cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		req.Port.NetworkID,
	).Scan(&subnetID, &cidr)

	var fixedIPs []map[string]any
	if err == nil {
		// Allocate IP from subnet
		allocatedIP := allocateIP(cidr)
		fixedIPs = []map[string]any{
			{
				"subnet_id":  subnetID,
				"ip_address": allocatedIP,
			},
		}
	}

	fixedIPsJSON, _ := json.Marshal(fixedIPs)

	// Create the TAP where the bridge is — the namespace o3k runs in. CreateTAPDevice no longer takes
	// a namespace at all, so this placement is now a property of the API rather than of the call.
	//
	// This used to place the TAP inside light-ns-<project> while CreateNetwork built the bridge in
	// the root namespace "for libvirt access" — and nothing on this path ever created that namespace,
	// so the attach could not succeed (SPEC-024 / cloud-storage#546 F10). The direction of the fix is
	// forced rather than chosen: the bridge (network.go), dnsmasq (reconciler.go, empty nsName) and
	// the egress SNAT rules are all already in the root namespace, so the TAP was the lone outlier.
	// Moving the bridge the other way would break libvirt's access to it, orphan dnsmasq and the NAT
	// rules, and need a veth pair per network.
	//
	// Per-network layer-2 isolation is unaffected: it comes from one bridge per network, not from a
	// namespace per project.
	if err := svc.tapManager.CreateTAPDevice(tapName); err != nil {
		log.Error().Err(err).Str("operation", "create_tap_device").Str("tap", tapName).Msg("failed to create TAP device")
		common.SendError(c, common.NewInternalServerError("failed to create TAP device "+tapName))
		return
	}

	// Attach TAP to the network's bridge, in the same namespace it was just created in.
	//
	// Ensure the bridge first, as AllocatePortForInstance already does on the instance-boot path:
	// in real mode the kernel object is gone after a host reboot until the reconciler's next pass,
	// and FR-001 requires port creation to succeed on any recorded network rather than depending on
	// that timing. Idempotent — an existing bridge is not an error.
	bridgeName := "br-" + req.Port.NetworkID[:8]
	if err := svc.brManager.CreateBridge(bridgeName); err != nil {
		log.Debug().Err(err).Str("bridge", bridgeName).Msg("ensure bridge before attach (may already exist)")
	}
	if err := svc.brManager.AttachToBridge(tapName, bridgeName); err != nil {
		log.Error().Err(err).Str("operation", "attach_tap_to_bridge").Str("tap", tapName).Str("bridge", bridgeName).Msg("failed to attach TAP to bridge")
		_ = svc.tapManager.DeleteTAPDevice(tapName)
		common.SendError(c, common.NewInternalServerError("failed to attach TAP "+tapName+" to bridge "+bridgeName))
		return
	}

	// Insert into database
	now := time.Now()
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, portID, req.Port.Name, req.Port.NetworkID, projectID, sql.NullString{String: req.Port.DeviceID, Valid: req.Port.DeviceID != ""},
		sql.NullString{String: req.Port.DeviceOwner, Valid: req.Port.DeviceOwner != ""}, macAddress, adminStateUp, "ACTIVE", fixedIPsJSON, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_port").Msg("database error")
		_ = svc.tapManager.DeleteTAPDevice(tapName)
		common.SendError(c, common.NewInternalServerError("failed to create port"))
		return
	}

	// Apply security groups (default to "default" security group if none specified)
	securityGroups := req.Port.SecurityGroups
	if len(securityGroups) == 0 {
		// Get default security group for project
		var defaultSGID string
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT id FROM security_groups WHERE project_id = $1 AND name = 'default'",
			projectID,
		).Scan(&defaultSGID)

		if err == nil {
			securityGroups = []string{defaultSGID}
		}
	}

	// Insert port-security group associations
	for _, sgID := range securityGroups {
		// Verify security group exists and belongs to project
		var exists bool
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT EXISTS(SELECT 1 FROM security_groups WHERE id = $1 AND project_id = $2)",
			sgID, projectID,
		).Scan(&exists)

		if err != nil || !exists {
			// Clean up port on failure (best-effort; the lookup error is
			// the user-facing one)
			if _, cleanupErr := database.DB.Exec(c.Request.Context(), "DELETE FROM ports WHERE id = $1", portID); cleanupErr != nil {
				log.Warn().Err(cleanupErr).Str("port_id", portID).Msg("rollback: failed to delete port after security-group lookup failure")
			}
			common.SendError(c, common.NewNotFoundError(fmt.Sprintf("security group %s", sgID)))
			return
		}

		_, err = database.DB.Exec(c.Request.Context(),
			"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2)",
			portID, sgID,
		)
		if err != nil {
			// Clean up port on failure (best-effort; the insert error
			// is the user-facing one)
			if _, cleanupErr := database.DB.Exec(c.Request.Context(), "DELETE FROM ports WHERE id = $1", portID); cleanupErr != nil {
				log.Warn().Err(cleanupErr).Str("port_id", portID).Msg("rollback: failed to delete port after security-group association failure")
			}
			log.Error().Err(err).Str("operation", "associate_security_group").Str("sg_id", sgID).Msg("failed to associate security group")
			common.SendError(c, common.NewInternalServerError("failed to associate security group"))
			return
		}
	}

	// Apply security group rules (iptables or eBPF based on mode)
	if svc.sgManager != nil && svc.mode == "ebpf" && len(fixedIPs) > 0 {
		// eBPF mode: Apply rules directly to port
		rules, err := svc.fetchSecurityGroupRulesForPort(c.Request.Context(), securityGroups)
		if err != nil {
			log.Warn().Err(err).Str("port_id", portID).Msg("failed to fetch security group rules")
		} else {
			// Parse MAC address
			mac, err := net.ParseMAC(macAddress)
			if err != nil {
				log.Warn().Err(err).Str("port_id", portID).Str("mac_address", macAddress).Msg("invalid MAC address")
			} else {
				if err := svc.sgManager.ApplySecurityGroupToPort(portID, mac, rules); err != nil {
					log.Warn().Err(err).Str("port_id", portID).Msg("failed to apply eBPF security group rules")
				} else {
					log.Debug().Str("port_id", portID).Int("rule_count", len(rules)).Msg("applied eBPF security group rules")

					// Attach XDP program to TAP interface
					if err := svc.sgManager.AttachToInterface(tapName); err != nil {
						log.Warn().Err(err).Str("port_id", portID).Str("tap", tapName).Msg("failed to attach XDP program to interface")
					} else {
						log.Debug().Str("tap", tapName).Msg("attached XDP security group filter to interface")
					}
				}
			}
		}
	}
	// Note: For iptables mode, rules are applied when security groups are created/updated
	// via CreateSecurityGroupChain() and AddRule() methods

	// Distribute FDB entry if VXLAN is enabled
	if svc.vxlanCoordinator != nil {
		if err := svc.vxlanCoordinator.DistributeFDBEntry(c.Request.Context(), req.Port.NetworkID, portID, macAddress); err != nil {
			// Log but don't fail - FDB will be synced on next poll
			log.Warn().Err(err).Str("port_id", portID).Str("network_id", req.Port.NetworkID).Msg("failed to distribute FDB entry")
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"port": gin.H{
			"id":              portID,
			"name":            req.Port.Name,
			"network_id":      req.Port.NetworkID,
			"tenant_id":       projectID,
			"device_id":       req.Port.DeviceID,
			"device_owner":    req.Port.DeviceOwner,
			"mac_address":     macAddress,
			"admin_state_up":  adminStateUp,
			"status":          "ACTIVE",
			"fixed_ips":       fixedIPs,
			"security_groups": securityGroups,
			"created_at":      now.Format(time.RFC3339),
			"updated_at":      now.Format(time.RFC3339),
		},
	})
}

// ListPorts lists all ports
func (svc *Service) ListPorts(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000
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

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT created_at FROM ports WHERE id = $1",
			marker,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND p.created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	// Standard Neutron port filters. Notably ?device_id=<server> is how
	// `openstack server add floating ip` finds a server's port — without it the
	// client gets every port on the network and may grab the wrong one (e.g. the
	// router interface), associating the FIP to the gateway instead of the VM.
	var filterCondition string
	for _, f := range []struct{ col, val string }{
		{"p.device_id", c.Query("device_id")},
		{"p.network_id", c.Query("network_id")},
		{"p.device_owner", c.Query("device_owner")},
		{"p.mac_address", c.Query("mac_address")},
		{"p.id", c.Query("id")},
	} {
		if f.val != "" {
			filterCondition += fmt.Sprintf(" AND %s = $%d", f.col, argIdx)
			queryArgs = append(queryArgs, f.val)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := database.DB.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT p.id, p.name, p.network_id, p.device_id, p.device_owner, p.mac_address, p.admin_state_up, p.status, p.fixed_ips, p.created_at, p.updated_at
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		WHERE (p.project_id = $1 OR n.shared = true)%s%s
		ORDER BY p.created_at DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, filterCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_ports").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list ports"))
		return
	}
	defer rows.Close()

	var ports []gin.H
	for rows.Next() {
		var id, name, networkID string
		var deviceID, deviceOwner sql.NullString
		var macAddress, status string
		var adminStateUp bool
		var fixedIPsJSON []byte
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &networkID, &deviceID, &deviceOwner, &macAddress, &adminStateUp, &status, &fixedIPsJSON, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan port row")
			continue
		}

		var fixedIPs []map[string]any
		if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil {
			log.Warn().Err(err).Str("port_id", id).Msg("failed to unmarshal port fixed_ips; treating as empty")
		}

		// Fetch associated security groups for this port
		securityGroups := []string{}
		sgRows, err := database.DB.Query(c.Request.Context(),
			"SELECT security_group_id FROM port_security_groups WHERE port_id = $1",
			id,
		)
		if err == nil {
			for sgRows.Next() {
				var sgID string
				if err := sgRows.Scan(&sgID); err == nil {
					securityGroups = append(securityGroups, sgID)
				}
			}
			if err := sgRows.Err(); err != nil {
				log.Warn().Err(err).Msg("sgRows iteration error; list may be truncated")
			}
			sgRows.Close()
		}

		ports = append(ports, gin.H{
			"id":              id,
			"name":            name,
			"network_id":      networkID,
			"tenant_id":       projectID,
			"device_id":       deviceID.String,
			"device_owner":    deviceOwner.String,
			"mac_address":     macAddress,
			"admin_state_up":  adminStateUp,
			"status":          status,
			"fixed_ips":       fixedIPs,
			"security_groups": securityGroups,
			"created_at":      createdAt.Format(time.RFC3339),
			"updated_at":      updatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if ports == nil {
		ports = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"ports": ports})
}

// GetPort returns a single port
func (svc *Service) GetPort(c *gin.Context) {
	portID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, networkID string
	var deviceID, deviceOwner sql.NullString
	var macAddress, status string
	var adminStateUp bool
	var fixedIPsJSON []byte
	var createdAt, updatedAt time.Time

	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT p.id, p.name, p.network_id, p.device_id, p.device_owner, p.mac_address, p.admin_state_up, p.status, p.fixed_ips, p.created_at, p.updated_at
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		WHERE p.id = $1 AND (p.project_id = $2 OR n.shared = true)
	`, portID, projectID).Scan(&id, &name, &networkID, &deviceID, &deviceOwner, &macAddress, &adminStateUp, &status, &fixedIPsJSON, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get port"))
		return
	}

	var fixedIPs []map[string]any
	if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil {
		log.Warn().Err(err).Str("port_id", portID).Msg("failed to unmarshal port fixed_ips; treating as empty")
	}

	// Fetch associated security groups
	securityGroups := []string{}
	sgRows, err := database.DB.Query(c.Request.Context(),
		"SELECT security_group_id FROM port_security_groups WHERE port_id = $1",
		portID,
	)
	if err == nil {
		defer sgRows.Close()
		for sgRows.Next() {
			var sgID string
			if err := sgRows.Scan(&sgID); err == nil {
				securityGroups = append(securityGroups, sgID)
			}
		}
		if err := sgRows.Err(); err != nil {
			log.Warn().Err(err).Msg("sgRows iteration error; list may be truncated")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"port": gin.H{
			"id":              id,
			"name":            name,
			"network_id":      networkID,
			"tenant_id":       projectID,
			"device_id":       deviceID.String,
			"device_owner":    deviceOwner.String,
			"mac_address":     macAddress,
			"admin_state_up":  adminStateUp,
			"status":          status,
			"fixed_ips":       fixedIPs,
			"security_groups": securityGroups,
			"created_at":      createdAt.Format(time.RFC3339),
			"updated_at":      updatedAt.Format(time.RFC3339),
		},
	})
}

// DeletePort deletes a port
func (svc *Service) DeletePort(c *gin.Context) {
	portID := c.Param("id")
	projectID := c.GetString("project_id")

	// Resolve the port BEFORE deriving anything from its id (second review, AGY-1).
	//
	// `tapName := "tap-" + portID[:8]` used to be the third statement of this handler, ahead of every
	// lookup, so DELETE /v2.0/ports/p1 sliced out of range and panicked — recovered into a 500 for a
	// request that names no port. Comparing as text rather than casting to uuid means a malformed id
	// simply matches nothing instead of erroring the query, which was a second 500 on the same path.
	//
	// Resolving first also stops the handler reporting 204 for a port it never found: the dishonest
	// success FR-033b removed from DeleteNetwork, still present here because the sweep that fixed
	// that one did not reach its siblings.
	var resolvedID string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id::text FROM ports WHERE id::text = $1 AND project_id = $2",
		portID, projectID,
	).Scan(&resolvedID)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("port"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete port"))
		return
	}
	portID = resolvedID

	tapName := "tap-" + portID[:8]

	// Detach XDP program if eBPF mode
	if svc.sgManager != nil && svc.mode == "ebpf" {
		if err := svc.sgManager.DetachFromInterface(tapName); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Str("tap", tapName).Msg("failed to detach XDP program from interface")
		}

		// Remove port from eBPF maps (need MAC address). A scan failure
		// leaves macAddress empty, which skips the eBPF cleanup below —
		// safe degradation; a leftover map entry will be reconciled on
		// the next sync.
		var macAddress string
		if err := database.DB.QueryRow(c.Request.Context(),
			"SELECT mac_address FROM ports WHERE id = $1",
			portID,
		).Scan(&macAddress); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Msg("failed to resolve port MAC during teardown")
		}

		if macAddress != "" {
			if mac, err := net.ParseMAC(macAddress); err == nil {
				if err := svc.sgManager.RemoveSecurityGroupFromPort(portID, mac); err != nil {
					log.Warn().Err(err).Str("port_id", portID).Str("mac", mac.String()).Msg("failed to remove security group from port")
				}
			}
		}
	}

	// Delete TAP device from the DEFAULT namespace, where CreatePort put it (F10). Best-effort: a
	// leftover TAP is hygiene, not correctness — the port row is about to be deleted.
	if err := svc.tapManager.DeleteTAPDevice(tapName); err != nil {
		log.Warn().Err(err).Str("tap", tapName).Msg("failed to delete TAP device")
	}

	// Remove FDB entry if VXLAN is enabled
	if svc.vxlanCoordinator != nil {
		if err := svc.vxlanCoordinator.RemoveFDBEntry(c.Request.Context(), portID); err != nil {
			// Log but don't fail
			log.Warn().Err(err).Str("port_id", portID).Msg("failed to remove FDB entry")
		}
	}

	// Delete from database. The row was resolved above, so this deletes by an id already known to
	// exist and to belong to the caller.
	if _, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM ports WHERE id = $1 AND project_id = $2",
		portID, projectID,
	); err != nil {
		log.Error().Err(err).Str("operation", "delete_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete port"))
		return
	}

	c.Status(http.StatusNoContent)
}

// UpdatePort updates a port
func (svc *Service) UpdatePort(c *gin.Context) {
	portID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Port struct {
			Name         *string `json:"name"`
			AdminStateUp *bool   `json:"admin_state_up"`
			DeviceID     *string `json:"device_id"`
			DeviceOwner  *string `json:"device_owner"`
		} `json:"port"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	updates := []string{}
	args := []any{}
	argID := 1

	if req.Port.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", argID))
		args = append(args, *req.Port.Name)
		argID++
	}

	if req.Port.AdminStateUp != nil {
		updates = append(updates, fmt.Sprintf("admin_state_up = $%d", argID))
		args = append(args, *req.Port.AdminStateUp)
		argID++
	}

	if req.Port.DeviceID != nil {
		updates = append(updates, fmt.Sprintf("device_id = $%d", argID))
		args = append(args, *req.Port.DeviceID)
		argID++
	}

	if req.Port.DeviceOwner != nil {
		updates = append(updates, fmt.Sprintf("device_owner = $%d", argID))
		args = append(args, *req.Port.DeviceOwner)
		argID++
	}

	if len(updates) == 0 {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	updates = append(updates, fmt.Sprintf("updated_at = $%d", argID))
	args = append(args, time.Now())
	argID++

	args = append(args, portID, projectID)

	query := fmt.Sprintf("UPDATE ports SET %s WHERE id = $%d AND project_id = $%d",
		updateString(updates), argID, argID+1)

	_, err := database.DB.Exec(c.Request.Context(), query, args...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update port"))
		return
	}

	// Return updated port
	svc.GetPort(c)
}

// allocateIP allocates an IP from a CIDR range using cryptographically random offset
func allocateIP(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}

	// Count usable host addresses (excluding network and broadcast)
	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return ""
	}
	maxOffset := int64(1<<uint(hostBits)) - 2 // exclude network(0) and broadcast

	n, err := rand.Int(rand.Reader, big.NewInt(maxOffset-10))
	offset := uint(10) // fallback to offset 10 if rand fails
	if err == nil {
		offset = uint(n.Int64() + 10)
	}

	ip := incrementIP(ipNet.IP, offset)
	return ip.String()
}

// Security Groups implementation

// CreateSecurityGroupRequest represents a security group creation request
type CreateSecurityGroupRequest struct {
	SecurityGroup struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	} `json:"security_group"`
}

// CreateSecurityGroup creates a new security group
func (svc *Service) CreateSecurityGroup(c *gin.Context) {
	var req CreateSecurityGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	sgID := uuid.New().String()

	// Create iptables chain for security group
	if svc.sgManager != nil {
		if err := svc.sgManager.CreateSecurityGroupChain(sgID); err != nil {
			log.Error().Err(err).Str("operation", "create_sg_chain").Str("sg_id", sgID).Msg("failed to create security group chain")
			common.SendError(c, common.NewInternalServerError("failed to create security group chain"))
			return
		}
	}

	// Insert into database
	now := time.Now()
	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO security_groups (id, name, project_id, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, sgID, req.SecurityGroup.Name, projectID, req.SecurityGroup.Description, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_security_group").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create security group"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"security_group": gin.H{
			"id":          sgID,
			"name":        req.SecurityGroup.Name,
			"tenant_id":   projectID,
			"description": req.SecurityGroup.Description,
			"created_at":  now.Format(time.RFC3339),
			"updated_at":  now.Format(time.RFC3339),
		},
	})
}

// securityGroupRulesByGroup loads the rules of the given security groups, keyed by
// security_group_id. Groups without rules are absent from the map; callers must
// substitute an empty slice rather than nil — see ListSecurityGroups.
func securityGroupRulesByGroup(ctx context.Context, sgIDs []string) (map[string][]gin.H, error) {
	byGroup := make(map[string][]gin.H, len(sgIDs))
	if len(sgIDs) == 0 {
		return byGroup, nil
	}

	rows, err := database.DB.Query(ctx, `
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

// securityGroupRow is one row of security_groups as the list view needs it.
type securityGroupRow struct {
	id, name, description string
	createdAt, updatedAt  time.Time
}

// listSecurityGroupRows loads a project's security groups.
//
// This lives in its own function so that its `defer rows.Close()` returns the pooled
// connection when it returns — before the caller issues the rules query. Scanning
// inline in ListSecurityGroups would hold two pool connections per in-flight request,
// which a busy endpoint can turn into pool exhaustion rather than mere slowness.
func listSecurityGroupRows(ctx context.Context, projectID string) ([]securityGroupRow, error) {
	rows, err := database.DB.Query(ctx, `
		SELECT id, name, description, created_at, updated_at
		FROM security_groups
		WHERE project_id = $1
		ORDER BY created_at DESC, id DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query security groups for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var out []securityGroupRow
	for rows.Next() {
		var r securityGroupRow
		if err := rows.Scan(&r.id, &r.name, &r.description, &r.createdAt, &r.updatedAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan security group row")
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate security groups for project %s: %w", projectID, err)
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
	projectID := c.GetString("project_id")

	sgRows, err := listSecurityGroupRows(c.Request.Context(), projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_security_groups").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security groups"))
		return
	}

	sgIDs := make([]string, 0, len(sgRows))
	for _, r := range sgRows {
		sgIDs = append(sgIDs, r.id)
	}

	rulesByGroup, err := securityGroupRulesByGroup(c.Request.Context(), sgIDs)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_security_group_rules").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security group rules"))
		return
	}

	securityGroups := []gin.H{}
	for _, r := range sgRows {
		groupRules := rulesByGroup[r.id]
		if groupRules == nil {
			groupRules = []gin.H{}
		}
		securityGroups = append(securityGroups, gin.H{
			"id":                   r.id,
			"name":                 r.name,
			"project_id":           projectID,
			"tenant_id":            projectID,
			"description":          r.description,
			"created_at":           r.createdAt.Format(time.RFC3339),
			"updated_at":           r.updatedAt.Format(time.RFC3339),
			"security_group_rules": groupRules,
		})
	}

	c.JSON(http.StatusOK, gin.H{"security_groups": securityGroups})
}

// GetSecurityGroup returns a single security group
func (svc *Service) GetSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, description string
	var createdAt, updatedAt time.Time

	// id::text resolves by UUID or name without crashing on a non-UUID
	// identifier — the CLI looks an SG up by name (e.g. `... rule create ... default`).
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT id, name, description, created_at, updated_at
		FROM security_groups
		WHERE (id::text = $1 OR name = $1) AND project_id = $2
		LIMIT 1
	`, sgID, projectID).Scan(&id, &name, &description, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("security group"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group"))
		return
	}

	// Fetch associated rules — keyed by the resolved UUID (id), not the
	// possibly-by-name request param (sgID).
	rulesByGroup, err := securityGroupRulesByGroup(c.Request.Context(), []string{id})
	if err != nil {
		log.Error().Err(err).Str("operation", "get_sg_rules").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group rules"))
		return
	}
	rules := rulesByGroup[id]
	if rules == nil {
		rules = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{
		"security_group": gin.H{
			"id":                   id,
			"name":                 name,
			"project_id":           projectID,
			"tenant_id":            projectID,
			"description":          description,
			"created_at":           createdAt.Format(time.RFC3339),
			"updated_at":           updatedAt.Format(time.RFC3339),
			"security_group_rules": rules,
		},
	})
}

// DeleteSecurityGroup deletes a security group
func (svc *Service) DeleteSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")

	// Delete iptables chain (best-effort: a leftover chain is hygiene,
	// not correctness — the security group row is about to be deleted).
	if svc.sgManager != nil {
		if err := svc.sgManager.DeleteSecurityGroupChain(sgID); err != nil {
			log.Warn().Err(err).Str("sg_id", sgID).Msg("failed to delete security group iptables chain")
		}
	}

	// Delete from database
	_, err := database.DB.Exec(c.Request.Context(),
		"DELETE FROM security_groups WHERE id = $1 AND project_id = $2",
		sgID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete security group"))
		return
	}

	c.Status(http.StatusNoContent)
}

// UpdateSecurityGroup updates a security group
func (svc *Service) UpdateSecurityGroup(c *gin.Context) {
	sgID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		SecurityGroup struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"security_group"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check security group exists
	var currentName, currentDesc string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT name, description FROM security_groups WHERE id = $1 AND project_id = $2",
		sgID, projectID,
	).Scan(&currentName, &currentDesc)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("security group"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update security group"))
		return
	}

	// Apply updates
	if req.SecurityGroup.Name != nil {
		currentName = *req.SecurityGroup.Name
	}
	if req.SecurityGroup.Description != nil {
		currentDesc = *req.SecurityGroup.Description
	}

	_, err = database.DB.Exec(c.Request.Context(),
		"UPDATE security_groups SET name = $1, description = $2 WHERE id = $3",
		currentName, currentDesc, sgID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_security_group").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update security group"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"security_group": gin.H{
			"id":          sgID,
			"name":        currentName,
			"description": currentDesc,
			"tenant_id":   projectID,
		},
	})
}

// CreateSecurityGroupRuleRequest represents a security group rule creation request
type CreateSecurityGroupRuleRequest struct {
	SecurityGroupRule struct {
		SecurityGroupID string  `json:"security_group_id" binding:"required"`
		Direction       string  `json:"direction" binding:"required"`
		EtherType       string  `json:"ethertype"`
		Protocol        *string `json:"protocol"`
		PortRangeMin    *int    `json:"port_range_min"`
		PortRangeMax    *int    `json:"port_range_max"`
		RemoteIPPrefix  *string `json:"remote_ip_prefix"`
		RemoteGroupID   *string `json:"remote_group_id"`
	} `json:"security_group_rule"`
}

// securityGroupOwnedByCaller reports whether a security group belongs to the caller's
// project. It is the ownership gate for the rule handlers, which address a rule (or its
// group) by id supplied in the request and would otherwise operate on any tenant's
// firewall state.
//
// A group in another project is reported as absent, not forbidden: 403 would confirm the
// id exists, which is itself the cross-tenant disclosure we are closing. Both ids are
// compared as text so a malformed id simply fails to match instead of raising
// "invalid input syntax for type uuid" as a 500 — that includes the empty project id an
// unscoped token carries.
func securityGroupOwnedByCaller(ctx context.Context, sgID, projectID string) (bool, error) {
	var owned bool
	err := database.DB.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM security_groups WHERE id::text = $1 AND project_id::text = $2)",
		sgID, projectID).Scan(&owned)
	return owned, err
}

// CreateSecurityGroupRule creates a new security group rule
func (svc *Service) CreateSecurityGroupRule(c *gin.Context) {
	var req CreateSecurityGroupRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// The target group must be the caller's. Checked before anything else, because the
	// steps below write the caller's rule into another tenant's iptables chain and
	// security_group_rules row without ever consulting project_id otherwise.
	owned, err := securityGroupOwnedByCaller(c.Request.Context(),
		req.SecurityGroupRule.SecurityGroupID, c.GetString("project_id"))
	if err != nil {
		log.Error().Err(err).Str("operation", "create_sg_rule").
			Str("sg_id", req.SecurityGroupRule.SecurityGroupID).Msg("ownership check failed")
		common.SendError(c, common.NewInternalServerError("failed to create security group rule"))
		return
	}
	if !owned {
		common.SendError(c, common.NewNotFoundError("security group"))
		return
	}

	ruleID := uuid.New().String()

	// The optional attributes stay POINTERS all the way to storage and to the response.
	//
	// They arrive as pointers already (the request struct above), and flattening them to ""/0
	// sentinels here is what made this the fourth and only disagreeing emitter: unset came back as
	// "" and 0 while every read reported null, and — worse — `Valid: portMin > 0` stored a deliberate
	// port 0, a legitimate ICMP type, as NULL (SPEC-024 / cloud-storage#546 B9, FR-026-028).
	protocol := req.SecurityGroupRule.Protocol
	portMin := req.SecurityGroupRule.PortRangeMin
	portMax := req.SecurityGroupRule.PortRangeMax
	remoteIP := req.SecurityGroupRule.RemoteIPPrefix
	remoteGroup := req.SecurityGroupRule.RemoteGroupID

	etherType := "IPv4"
	if req.SecurityGroupRule.EtherType != "" {
		etherType = req.SecurityGroupRule.EtherType
	}

	// Add iptables rule
	if svc.sgManager != nil {
		// networking.SecurityGroupRule is value-typed because a kernel rule cannot express
		// "unset" — a defaulted zero here is correct and is the only place a sentinel belongs.
		rule := networking.SecurityGroupRule{
			ID:             ruleID,
			Direction:      req.SecurityGroupRule.Direction,
			EtherType:      etherType,
			Protocol:       derefOr(protocol, ""),
			PortRangeMin:   derefOr(portMin, 0),
			PortRangeMax:   derefOr(portMax, 0),
			RemoteIPPrefix: derefOr(remoteIP, ""),
			RemoteGroupID:  derefOr(remoteGroup, ""),
		}

		// A seeded SG (e.g. the project default SG, created via migration rather
		// than the API) has no iptables chain yet — ensure it idempotently so
		// AddRule doesn't fail on a missing chain.
		if err := svc.sgManager.CreateSecurityGroupChain(req.SecurityGroupRule.SecurityGroupID); err != nil {
			log.Warn().Err(err).Str("sg_id", req.SecurityGroupRule.SecurityGroupID).Msg("ensure security group chain")
		}
		if err := svc.sgManager.AddRule(req.SecurityGroupRule.SecurityGroupID, rule); err != nil {
			log.Error().Err(err).Str("operation", "add_sg_rule").Msg("failed to add iptables rule")
			common.SendError(c, common.NewInternalServerError("failed to add security group rule"))
			return
		}
	}

	// Insert into database
	now := time.Now()
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix, remote_group_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, ruleID, req.SecurityGroupRule.SecurityGroupID, req.SecurityGroupRule.Direction, etherType,
		nullStringFrom(protocol), nullInt32From(portMin), nullInt32From(portMax),
		nullStringFrom(remoteIP), nullStringFrom(remoteGroup),
		now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_sg_rule").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create security group rule"))
		return
	}

	// Re-apply north-south ingress filters for any floating-IP'd port using this
	// SG, so the new rule takes effect immediately (SPEC-009 WS-3 / T034).
	svc.refreshIngressFiltersForSG(c.Request.Context(), req.SecurityGroupRule.SecurityGroupID)

	c.JSON(http.StatusCreated, gin.H{
		"security_group_rule": gin.H{
			"id":                ruleID,
			"security_group_id": req.SecurityGroupRule.SecurityGroupID,
			"direction":         req.SecurityGroupRule.Direction,
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
	projectID := c.GetString("project_id")
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT sgr.id, sgr.security_group_id, sgr.direction, sgr.ethertype, sgr.protocol, sgr.port_range_min, sgr.port_range_max, sgr.remote_ip_prefix, sgr.remote_group_id, sgr.created_at
		FROM security_group_rules sgr
		JOIN security_groups sg ON sg.id = sgr.security_group_id
		WHERE sg.project_id = $1
		ORDER BY sgr.created_at DESC
	`, projectID)

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
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT sgr.id, sgr.security_group_id, sgr.direction, sgr.ethertype, sgr.protocol,
		       sgr.port_range_min, sgr.port_range_max, sgr.remote_ip_prefix, sgr.remote_group_id, sgr.created_at
		FROM security_group_rules sgr
		JOIN security_groups sg ON sg.id = sgr.security_group_id
		WHERE sgr.id::text = $1 AND sg.project_id::text = $2
	`, ruleID, c.GetString("project_id")).Scan(&id, &sgID, &direction, &ethertype, &protocol, &portRangeMin, &portRangeMax,
		&remoteIPPrefix, &remoteGroupID, &createdAt)

	if errors.Is(err, pgx.ErrNoRows) {
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

	// Get rule details to remove from iptables
	var sgID, direction, etherType string
	var protocol, remoteIP, remoteGroup sql.NullString
	var portMin, portMax sql.NullInt32

	// Scoped by the owning group's project. Unscoped, this deleted any rule in the
	// cluster — and removed it from iptables too, so one request could open or close
	// another tenant's firewall. A rule outside the caller's project reads as absent.
	projectID := c.GetString("project_id")
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT sgr.security_group_id, sgr.direction, sgr.ethertype, sgr.protocol, sgr.port_range_min, sgr.port_range_max, sgr.remote_ip_prefix, sgr.remote_group_id
		FROM security_group_rules sgr
		JOIN security_groups sg ON sg.id = sgr.security_group_id
		WHERE sgr.id::text = $1 AND sg.project_id::text = $2
	`, ruleID, projectID).Scan(&sgID, &direction, &etherType, &protocol, &portMin, &portMax, &remoteIP, &remoteGroup)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("security group rule"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_sg_rule").Str("rule_id", ruleID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete security group rule"))
		return
	}

	if svc.sgManager != nil {
		rule := networking.SecurityGroupRule{
			ID:             ruleID,
			Direction:      direction,
			EtherType:      etherType,
			Protocol:       protocol.String,
			PortRangeMin:   int(portMin.Int32),
			PortRangeMax:   int(portMax.Int32),
			RemoteIPPrefix: remoteIP.String,
			RemoteGroupID:  remoteGroup.String,
		}

		if err := svc.sgManager.RemoveRule(sgID, rule); err != nil {
			log.Warn().Err(err).Str("sg_id", sgID).Str("rule_id", rule.ID).Msg("failed to remove security group iptables rule")
		}
	}

	// The row is already proven to be the caller's by the lookup above; the predicate is
	// repeated so this statement is self-evidently scoped when read on its own, and so a
	// concurrent delete surfaces as 404 rather than a misleading 204.
	tag, err := database.DB.Exec(c.Request.Context(), `
		DELETE FROM security_group_rules
		WHERE id::text = $1
		  AND security_group_id IN (SELECT id FROM security_groups WHERE project_id::text = $2)
	`, ruleID, projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_sg_rule").Str("rule_id", ruleID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete security group rule"))
		return
	}
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("security group rule"))
		return
	}

	// Re-apply north-south ingress filters so the removed rule stops applying
	// immediately (SPEC-009 WS-3 / T034).
	svc.refreshIngressFiltersForSG(c.Request.Context(), sgID)

	c.Status(http.StatusNoContent)
}

// PortInfo represents port allocation information for cross-service use
type PortInfo struct {
	ID        string
	NetworkID string
	MAC       string
	IPAddress string
	SubnetID  string
}

// AllocatePortForInstance creates a port for a VM instance (called from Nova)
func (svc *Service) AllocatePortForInstance(ctx context.Context, networkID, projectID, instanceID string) (any, error) {
	portID := uuid.New().String()
	macAddress := generateMAC()

	// Get network and subnet info to allocate IP
	var netID string
	err := database.DB.QueryRow(ctx,
		"SELECT id FROM networks WHERE id = $1 AND (project_id = $2 OR shared = true)",
		networkID, projectID,
	).Scan(&netID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("network %s not found", networkID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query network: %w", err)
	}

	// Get subnet to allocate IP
	var subnetID, cidr string
	err = database.DB.QueryRow(ctx,
		"SELECT id, cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		networkID,
	).Scan(&subnetID, &cidr)

	if err != nil {
		return nil, fmt.Errorf("no subnet found for network %s: %w", networkID, err)
	}

	// Allocate IP from subnet
	allocatedIP := allocateIP(cidr)
	if allocatedIP == "" {
		return nil, fmt.Errorf("failed to allocate IP from subnet %s", subnetID)
	}

	fixedIPs := []map[string]any{
		{
			"subnet_id":  subnetID,
			"ip_address": allocatedIP,
		},
	}
	fixedIPsJSON, _ := json.Marshal(fixedIPs)

	// Create TAP device beside the bridge, in the namespace o3k runs in, for libvirt access
	if svc.mode != "stub" {
		tapName := "tap-" + portID[:8]

		if err := svc.tapManager.CreateTAPDevice(tapName); err != nil {
			return nil, fmt.Errorf("failed to create TAP device: %w", err)
		}

		// Ensure bridge exists in default namespace
		bridgeName := "br-" + networkID[:8]
		if err := svc.brManager.CreateBridge(bridgeName); err != nil {
			// Bridge might already exist, log but don't fail
			log.Debug().Err(err).Str("bridge", bridgeName).Msg("bridge creation returned error (may already exist)")
		}

		// Attach TAP to bridge in default namespace
		if err := svc.brManager.AttachToBridge(tapName, bridgeName); err != nil {
			return nil, fmt.Errorf("failed to attach TAP to bridge: %w", err)
		}
	}

	// Insert into database
	now := time.Now()
	_, err = database.DB.Exec(ctx, `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, portID, fmt.Sprintf("port-for-%s", instanceID[:8]), networkID, projectID, instanceID,
		"compute:nova", macAddress, true, "ACTIVE", fixedIPsJSON, now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to insert port into database: %w", err)
	}

	// Associate the project's default security group (OpenStack: an instance's
	// port gets the default SG unless overridden), so security-group rules apply
	// to the VM's north-south traffic (SPEC-009 WS-3 / T034).
	var defaultSG string
	if err := database.DB.QueryRow(ctx,
		"SELECT id FROM security_groups WHERE project_id = $1 AND name = 'default' LIMIT 1", projectID).Scan(&defaultSG); err == nil {
		if _, err := database.DB.Exec(ctx,
			"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			portID, defaultSG); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Msg("failed to associate default security group with port")
		}
	}

	// Distribute FDB entry if VXLAN is enabled
	if svc.vxlanCoordinator != nil {
		if err := svc.vxlanCoordinator.DistributeFDBEntry(ctx, networkID, portID, macAddress); err != nil {
			// Log but don't fail - FDB will be synced on next poll
			log.Warn().Err(err).Str("port_id", portID).Str("network_id", networkID).Msg("failed to distribute FDB entry")
		}
	}

	// Apply the dnsmasq reservation for this new port now (synchronously) so the
	// VM about to boot leases its neutron-assigned fixed IP, not a dynamic one
	// (SPEC-009 WS-2/WS-4 — keeps OpenStack's view and the guest in agreement).
	svc.applyDHCPForNetwork(ctx, networkID, true)

	return &PortInfo{
		ID:        portID,
		NetworkID: networkID,
		MAC:       macAddress,
		IPAddress: allocatedIP,
		SubnetID:  subnetID,
	}, nil
}

// fetchSecurityGroupRulesForPort retrieves all security group rules for given security group IDs
func (svc *Service) fetchSecurityGroupRulesForPort(ctx context.Context, securityGroupIDs []string) ([]networking.SecurityGroupRule, error) {
	if len(securityGroupIDs) == 0 {
		return []networking.SecurityGroupRule{}, nil
	}

	// Build query with IN clause for multiple security groups
	query := `
		SELECT id, security_group_id, direction, ethertype, protocol,
		       port_range_min, port_range_max, remote_ip_prefix, remote_group_id
		FROM security_group_rules
		WHERE security_group_id = ANY($1)
	`

	rows, err := database.DB.Query(ctx, query, securityGroupIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to query security group rules: %w", err)
	}
	defer rows.Close()

	var rules []networking.SecurityGroupRule
	for rows.Next() {
		var rule networking.SecurityGroupRule
		var sgID string // Not part of SecurityGroupRule struct, just for WHERE clause
		var protocol, remoteIP, remoteGroup sql.NullString
		var portMin, portMax sql.NullInt32

		err := rows.Scan(
			&rule.ID,
			&sgID, // security_group_id (not stored in rule struct)
			&rule.Direction,
			&rule.EtherType,
			&protocol,
			&portMin,
			&portMax,
			&remoteIP,
			&remoteGroup,
		)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan security group rule row")
			continue
		}

		if protocol.Valid {
			rule.Protocol = protocol.String
		}
		if portMin.Valid {
			rule.PortRangeMin = int(portMin.Int32)
		}
		if portMax.Valid {
			rule.PortRangeMax = int(portMax.Int32)
		}
		if remoteIP.Valid {
			rule.RemoteIPPrefix = remoteIP.String
		}
		if remoteGroup.Valid {
			rule.RemoteGroupID = remoteGroup.String
		}

		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		return nil, fmt.Errorf("failed to iterate security group rules: %w", err)
	}

	return rules, nil
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
