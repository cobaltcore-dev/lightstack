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

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
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

	// Create TAP device in namespace
	nsName := svc.nsManager.GetNamespaceName(projectID)
	if err := svc.tapManager.CreateTAPDevice(tapName, true, nsName); err != nil {
		log.Error().Err(err).Str("operation", "create_tap_device").Str("tap", tapName).Msg("failed to create TAP device")
		common.SendError(c, common.NewInternalServerError("failed to create TAP device"))
		return
	}

	// Attach TAP to bridge
	bridgeName := "br-" + req.Port.NetworkID[:8]
	if err := svc.brManager.AttachToBridge(tapName, bridgeName, true, nsName); err != nil {
		log.Error().Err(err).Str("operation", "attach_tap_to_bridge").Str("tap", tapName).Msg("failed to attach TAP to bridge")
		_ = svc.tapManager.DeleteTAPDevice(tapName, true, nsName)
		common.SendError(c, common.NewInternalServerError("failed to attach TAP to bridge"))
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
		_ = svc.tapManager.DeleteTAPDevice(tapName, true, nsName)
		common.SendError(c, common.NewInternalServerError("failed to create port"))
		return
	}

	// From here on the port row AND the TAP exist; every failure path below must
	// undo both, or a transient error leaks a host interface per retry.
	rollbackPort := func(reason string) {
		if _, err := database.DB.Exec(c.Request.Context(), "DELETE FROM ports WHERE id = $1", portID); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Msg("rollback: failed to delete port after " + reason)
		}
		if err := svc.tapManager.DeleteTAPDevice(tapName, true, nsName); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Str("tap", tapName).Msg("rollback: failed to delete TAP after " + reason)
		}
	}

	// Apply security groups (default to "default" security group if none specified)
	securityGroups := req.Port.SecurityGroups
	if len(securityGroups) == 0 {
		// OpenStack guarantees every project has a default group (Neutron creates it
		// lazily); ensure it rather than silently creating a port with none.
		defaultSGID, err := EnsureDefaultSecurityGroup(c.Request.Context(), projectID)
		if err != nil {
			// Never return a port the caller believes is in the default group when it isn't.
			rollbackPort("default security-group failure")
			log.Error().Err(err).Str("port_id", portID).Msg("failed to ensure default security group")
			common.SendError(c, common.NewInternalServerError("failed to ensure default security group"))
			return
		}
		securityGroups = []string{defaultSGID}
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
			rollbackPort("security-group lookup failure")
			common.SendError(c, common.NewNotFoundError(fmt.Sprintf("security group %s", sgID)))
			return
		}

		_, err = database.DB.Exec(c.Request.Context(),
			"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2)",
			portID, sgID,
		)
		if err != nil {
			rollbackPort("security-group association failure")
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
	// iptables mode: nothing to program per port here. Enforcement is the
	// per-fixed-IP north-south ingress filter (EnsureIngressFilter, hooked from
	// FORWARD), built from port_security_groups by portIngressRules at floating-IP
	// association, rebuilt by refreshIngressFiltersForSG on rule changes and
	// replayed by the reconciler — so a group is enforced by its rows, whether it
	// was created through the API, seeded, or ensured lazily. The per-group LS-SG-*
	// chains that CreateSecurityGroupChain/AddRule maintain are not on that path:
	// ApplyToInterface, which would jump an interface into them, has no callers.

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

	tapName := "tap-" + portID[:8]
	nsName := svc.nsManager.GetNamespaceName(projectID)

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

	// Delete TAP device (best-effort: a leftover TAP is hygiene, not
	// correctness — the port row is about to be deleted).
	if err := svc.tapManager.DeleteTAPDevice(tapName, true, nsName); err != nil {
		log.Warn().Err(err).Str("tap", tapName).Str("namespace", nsName).Msg("failed to delete TAP device")
	}

	// Remove FDB entry if VXLAN is enabled
	if svc.vxlanCoordinator != nil {
		if err := svc.vxlanCoordinator.RemoveFDBEntry(c.Request.Context(), portID); err != nil {
			// Log but don't fail
			log.Warn().Err(err).Str("port_id", portID).Msg("failed to remove FDB entry")
		}
	}

	// Delete from database
	_, err := database.DB.Exec(c.Request.Context(),
		"DELETE FROM ports WHERE id = $1 AND project_id = $2",
		portID, projectID,
	)
	if err != nil {
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

// ListSecurityGroups lists all security groups
func (svc *Service) ListSecurityGroups(c *gin.Context) {
	projectID := c.GetString("project_id")

	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT id, name, description, created_at, updated_at
		FROM security_groups
		WHERE project_id = $1
		ORDER BY created_at DESC, id DESC
	`, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_security_groups").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security groups"))
		return
	}
	defer rows.Close()

	var securityGroups []gin.H
	for rows.Next() {
		var id, name, description string
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &description, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan security group row")
			continue
		}

		securityGroups = append(securityGroups, gin.H{
			"id":          id,
			"name":        name,
			"tenant_id":   projectID,
			"description": description,
			"created_at":  createdAt.Format(time.RFC3339),
			"updated_at":  updatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if securityGroups == nil {
		securityGroups = []gin.H{}
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
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT id, direction, ethertype, protocol, port_range_min, port_range_max,
		       remote_ip_prefix, remote_group_id, created_at
		FROM security_group_rules
		WHERE security_group_id = $1
	`, id)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_sg_rules").Str("sg_id", sgID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security group rules"))
		return
	}
	defer rows.Close()

	rules := []gin.H{}
	for rows.Next() {
		var ruleID, direction, ethertype string
		var protocol, remoteIPPrefix, remoteGroupID *string
		var portRangeMin, portRangeMax *int
		var ruleCreatedAt time.Time

		err := rows.Scan(&ruleID, &direction, &ethertype, &protocol, &portRangeMin, &portRangeMax,
			&remoteIPPrefix, &remoteGroupID, &ruleCreatedAt)
		if err != nil {
			log.Warn().Err(err).Msg("failed to scan security group rule row")
			continue
		}

		rule := gin.H{
			"id":                ruleID,
			"security_group_id": id,
			"direction":         direction,
			"ethertype":         ethertype,
			"created_at":        ruleCreatedAt.Format(time.RFC3339),
		}

		if protocol != nil {
			rule["protocol"] = *protocol
		}
		if portRangeMin != nil {
			rule["port_range_min"] = *portRangeMin
		}
		if portRangeMax != nil {
			rule["port_range_max"] = *portRangeMax
		}
		if remoteIPPrefix != nil {
			rule["remote_ip_prefix"] = *remoteIPPrefix
		} else {
			rule["remote_ip_prefix"] = ""
		}
		if remoteGroupID != nil {
			rule["remote_group_id"] = *remoteGroupID
		} else {
			rule["remote_group_id"] = ""
		}

		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"security_group": gin.H{
			"id":                   id,
			"name":                 name,
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

// CreateSecurityGroupRule creates a new security group rule
func (svc *Service) CreateSecurityGroupRule(c *gin.Context) {
	var req CreateSecurityGroupRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	ruleID := uuid.New().String()

	protocol := ""
	if req.SecurityGroupRule.Protocol != nil {
		protocol = *req.SecurityGroupRule.Protocol
	}

	portMin := 0
	if req.SecurityGroupRule.PortRangeMin != nil {
		portMin = *req.SecurityGroupRule.PortRangeMin
	}

	portMax := 0
	if req.SecurityGroupRule.PortRangeMax != nil {
		portMax = *req.SecurityGroupRule.PortRangeMax
	}

	remoteIP := ""
	if req.SecurityGroupRule.RemoteIPPrefix != nil {
		remoteIP = *req.SecurityGroupRule.RemoteIPPrefix
	}

	remoteGroup := ""
	if req.SecurityGroupRule.RemoteGroupID != nil {
		remoteGroup = *req.SecurityGroupRule.RemoteGroupID
	}

	etherType := "IPv4"
	if req.SecurityGroupRule.EtherType != "" {
		etherType = req.SecurityGroupRule.EtherType
	}

	// Add iptables rule
	if svc.sgManager != nil {
		rule := networking.SecurityGroupRule{
			ID:             ruleID,
			Direction:      req.SecurityGroupRule.Direction,
			EtherType:      etherType,
			Protocol:       protocol,
			PortRangeMin:   portMin,
			PortRangeMax:   portMax,
			RemoteIPPrefix: remoteIP,
			RemoteGroupID:  remoteGroup,
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
	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix, remote_group_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, ruleID, req.SecurityGroupRule.SecurityGroupID, req.SecurityGroupRule.Direction, etherType,
		sql.NullString{String: protocol, Valid: protocol != ""},
		sql.NullInt32{Int32: int32(portMin), Valid: portMin > 0},
		sql.NullInt32{Int32: int32(portMax), Valid: portMax > 0},
		sql.NullString{String: remoteIP, Valid: remoteIP != ""},
		sql.NullString{String: remoteGroup, Valid: remoteGroup != ""},
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
		var id, sgID, direction, etherType string
		var protocol, remoteIP, remoteGroup sql.NullString
		var portMin, portMax sql.NullInt32
		var createdAt time.Time

		if err := rows.Scan(&id, &sgID, &direction, &etherType, &protocol, &portMin, &portMax, &remoteIP, &remoteGroup, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan security group rule row")
			continue
		}

		rule := gin.H{
			"id":                id,
			"security_group_id": sgID,
			"direction":         direction,
			"ethertype":         etherType,
			"created_at":        createdAt.Format(time.RFC3339),
		}

		if protocol.Valid {
			rule["protocol"] = protocol.String
		}
		if portMin.Valid {
			rule["port_range_min"] = portMin.Int32
		}
		if portMax.Valid {
			rule["port_range_max"] = portMax.Int32
		}
		if remoteIP.Valid {
			rule["remote_ip_prefix"] = remoteIP.String
		}
		if remoteGroup.Valid {
			rule["remote_group_id"] = remoteGroup.String
		}

		rules = append(rules, rule)
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

	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT id, security_group_id, direction, ethertype, protocol,
		       port_range_min, port_range_max, remote_ip_prefix, remote_group_id, created_at
		FROM security_group_rules
		WHERE id = $1
	`, ruleID).Scan(&id, &sgID, &direction, &ethertype, &protocol, &portRangeMin, &portRangeMax,
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

	rule := gin.H{
		"id":                id,
		"security_group_id": sgID,
		"direction":         direction,
		"ethertype":         ethertype,
		"created_at":        createdAt.Format(time.RFC3339),
	}
	if protocol != nil {
		rule["protocol"] = *protocol
	}
	if portRangeMin != nil {
		rule["port_range_min"] = *portRangeMin
	}
	if portRangeMax != nil {
		rule["port_range_max"] = *portRangeMax
	}
	if remoteIPPrefix != nil {
		rule["remote_ip_prefix"] = *remoteIPPrefix
	}
	if remoteGroupID != nil {
		rule["remote_group_id"] = *remoteGroupID
	}
	c.JSON(http.StatusOK, gin.H{"security_group_rule": rule})
}

// DeleteSecurityGroupRule deletes a security group rule
func (svc *Service) DeleteSecurityGroupRule(c *gin.Context) {
	ruleID := c.Param("id")

	// Get rule details to remove from iptables
	var sgID, direction, etherType string
	var protocol, remoteIP, remoteGroup sql.NullString
	var portMin, portMax sql.NullInt32

	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix, remote_group_id
		FROM security_group_rules
		WHERE id = $1
	`, ruleID).Scan(&sgID, &direction, &etherType, &protocol, &portMin, &portMax, &remoteIP, &remoteGroup)

	if err == nil && svc.sgManager != nil {
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

	// Delete from database
	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM security_group_rules WHERE id = $1",
		ruleID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_sg_rule").Str("rule_id", ruleID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete security group rule"))
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

// AllocatePortForInstance creates a port for a VM instance (called from Nova).
// securityGroupIDs are the groups the server was created with, already resolved and
// validated by Nova; empty means "none requested", which gets the project default.
func (svc *Service) AllocatePortForInstance(ctx context.Context, networkID, projectID, instanceID string, securityGroupIDs []string) (any, error) {
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

	// The security groups the instance was created with, so rules apply to the
	// VM's north-south traffic (SPEC-009 WS-3 / T034). port_security_groups is what
	// enforcement reads — portIngressRules joins it for the iptables dataplane,
	// fetchSecurityGroupRulesForPort for eBPF — so a port carrying the wrong group
	// is filtered by the wrong rules, whatever Nova reports back (cloud-storage#579).
	// Falls back to the project's default group when none were requested (OpenStack:
	// an instance's port gets the default SG unless overridden). Ensured, not looked
	// up: a fresh project has no default group until its first use. Resolved here,
	// before any host-side work, because it is pure database access and its own
	// transaction (EnsureDefaultSecurityGroup) cannot nest in the one below.
	portSGs := securityGroupIDs
	if len(portSGs) == 0 {
		defaultSG, err := EnsureDefaultSecurityGroup(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure default security group: %w", err)
		}
		portSGs = []string{defaultSG}
	}

	// Create TAP device in default namespace (not project namespace) for libvirt access.
	// The host-side work happens before the database transaction below, so every
	// error return after the TAP exists must release it, or a retried boot leaves an
	// orphan tap-<id> on the host per attempt (mirrors CreatePort's rollbackPort).
	// Deleting the TAP detaches it from the bridge; nothing else needs undoing.
	tapName := "tap-" + portID[:8]
	releaseTAP := func(reason string) {
		if svc.mode == "stub" {
			return
		}
		if err := svc.tapManager.DeleteTAPDevice(tapName, false, ""); err != nil {
			log.Warn().Err(err).Str("port_id", portID).Str("tap", tapName).Msg("rollback: failed to delete TAP after " + reason)
		}
	}
	if svc.mode != "stub" {
		// Create TAP device in default namespace
		if err := svc.tapManager.CreateTAPDevice(tapName, false, ""); err != nil {
			return nil, fmt.Errorf("failed to create TAP device: %w", err)
		}

		// Ensure bridge exists in default namespace
		bridgeName := "br-" + networkID[:8]
		if err := svc.brManager.CreateBridge(bridgeName, false, ""); err != nil {
			// Bridge might already exist, log but don't fail
			log.Debug().Err(err).Str("bridge", bridgeName).Msg("bridge creation returned error (may already exist)")
		}

		// Attach TAP to bridge in default namespace
		if err := svc.brManager.AttachToBridge(tapName, bridgeName, false, ""); err != nil {
			releaseTAP("bridge attach failure")
			return nil, fmt.Errorf("failed to attach TAP to bridge: %w", err)
		}
	}

	// Insert the port and its security-group associations atomically: a port that
	// exists with a subset of its groups is filtered by fewer rules than Nova
	// reports, so a failed association fails the allocation and takes the port row
	// with it. The caller (Nova CreateServer) then boots without this network,
	// which keeps the API and the dataplane in agreement.
	now := time.Now()
	if err := database.WithTx(ctx, func(tx pgx.Tx) error {
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
		return nil
	}); err != nil {
		releaseTAP("port insert failure")
		return nil, fmt.Errorf("failed to insert port into database: %w", err)
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
