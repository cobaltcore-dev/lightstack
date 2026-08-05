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
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	} `json:"router"`
}

// UpdateRouterRequest represents a router update request
type UpdateRouterRequest struct {
	Router struct {
		Name                *string         `json:"name"`
		AdminStateUp        *bool           `json:"admin_state_up"`
		ExternalGatewayInfo *map[string]any `json:"external_gateway_info"`
	} `json:"router"`
}

// ListRouters lists all routers for the project
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

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT created_at FROM routers WHERE id = $1 AND project_id = $2",
			marker, projectID,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := database.DB.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT id, name, project_id, admin_state_up, status, external_gateway_info,
		       distributed, ha, created_at, updated_at
		FROM routers
		WHERE project_id = $1%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_routers").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list routers"))
		return
	}
	defer rows.Close()

	var routers []gin.H
	for rows.Next() {
		var r Router
		var gatewayInfo sql.NullString

		if err := rows.Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
			&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}

		// Parse external gateway info. A malformed value leaves
		// ExternalGatewayInfo nil — the API response then omits it,
		// which is the right behavior for a corrupt row.
		if gatewayInfo.Valid {
			if err := json.Unmarshal([]byte(gatewayInfo.String), &r.ExternalGatewayInfo); err != nil {
				log.Warn().Err(err).Str("router_id", r.ID).Msg("failed to unmarshal external_gateway_info; treating as unset")
			}
		}

		routers = append(routers, gin.H{
			"id":                    r.ID,
			"name":                  r.Name,
			"tenant_id":             r.ProjectID,
			"project_id":            r.ProjectID,
			"admin_state_up":        r.AdminStateUp,
			"status":                r.Status,
			"external_gateway_info": r.ExternalGatewayInfo,
			"distributed":           r.Distributed,
			"ha":                    r.HA,
			"created_at":            r.CreatedAt.Format(time.RFC3339),
			"updated_at":            r.UpdatedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if routers == nil {
		routers = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"routers": routers})
}

// CreateRouter creates a new L3 router
func (svc *Service) CreateRouter(c *gin.Context) {
	var req CreateRouterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
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

	// Serialize external gateway info to JSON
	var gatewayInfoJSON []byte
	if req.Router.ExternalGatewayInfo != nil {
		gatewayInfoJSON, _ = json.Marshal(req.Router.ExternalGatewayInfo)
	}

	// Create router namespace
	if err := svc.routerManager.CreateRouterNamespace(routerID); err != nil {
		log.Error().Err(err).Str("operation", "create_router_namespace").Str("router_id", routerID).Msg("failed to create router namespace")
		common.SendError(c, common.NewInternalServerError("failed to create router namespace"))
		return
	}

	// Insert into database
	now := time.Now()
	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO routers (id, name, project_id, admin_state_up, status, external_gateway_info, distributed, ha, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, routerID, req.Router.Name, projectID, adminStateUp, "ACTIVE", gatewayInfoJSON, distributed, ha, now, now)

	if err != nil {
		// Cleanup on error (best-effort; the DB error is the user-facing one)
		if cleanupErr := svc.routerManager.DeleteRouterNamespace(routerID); cleanupErr != nil {
			log.Warn().Err(cleanupErr).Str("router_id", routerID).Msg("rollback: failed to delete router namespace after DB insert failure")
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

	c.JSON(http.StatusCreated, gin.H{
		"router": gin.H{
			"id":                    routerID,
			"name":                  req.Router.Name,
			"tenant_id":             projectID,
			"project_id":            projectID,
			"admin_state_up":        adminStateUp,
			"status":                "ACTIVE",
			"external_gateway_info": req.Router.ExternalGatewayInfo,
			"distributed":           distributed,
			"ha":                    ha,
			"created_at":            now.Format(time.RFC3339),
			"updated_at":            now.Format(time.RFC3339),
		},
	})
}

// GetRouter retrieves a single router
func (svc *Service) GetRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")

	var r Router
	var gatewayInfo sql.NullString

	// id::text comparison resolves by UUID or name without crashing on a
	// non-UUID identifier — the CLI looks a router up by name this way
	// (consistent with GetNetwork/GetSubnet).
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT id, name, project_id, admin_state_up, status, external_gateway_info,
		       distributed, ha, created_at, updated_at
		FROM routers
		WHERE (id::text = $1 OR name = $1) AND project_id = $2
		LIMIT 1
	`, routerID, projectID).Scan(&r.ID, &r.Name, &r.ProjectID, &r.AdminStateUp, &r.Status,
		&gatewayInfo, &r.Distributed, &r.HA, &r.CreatedAt, &r.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get router"))
		return
	}

	// Parse external gateway info. A malformed value leaves
	// ExternalGatewayInfo nil — the API response then omits it, which
	// is the right behavior for a corrupt row.
	if gatewayInfo.Valid {
		if err := json.Unmarshal([]byte(gatewayInfo.String), &r.ExternalGatewayInfo); err != nil {
			log.Warn().Err(err).Str("router_id", r.ID).Msg("failed to unmarshal external_gateway_info; treating as unset")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"router": gin.H{
			"id":                    r.ID,
			"name":                  r.Name,
			"tenant_id":             r.ProjectID,
			"project_id":            r.ProjectID,
			"admin_state_up":        r.AdminStateUp,
			"status":                r.Status,
			"external_gateway_info": r.ExternalGatewayInfo,
			"distributed":           r.Distributed,
			"ha":                    r.HA,
			"created_at":            r.CreatedAt.Format(time.RFC3339),
			"updated_at":            r.UpdatedAt.Format(time.RFC3339),
		},
	})
}

// UpdateRouter updates an existing router
func (svc *Service) UpdateRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")

	var req UpdateRouterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	updates := []string{}
	args := []any{}
	argID := 1

	if req.Router.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", argID))
		args = append(args, *req.Router.Name)
		argID++
	}

	if req.Router.AdminStateUp != nil {
		updates = append(updates, fmt.Sprintf("admin_state_up = $%d", argID))
		args = append(args, *req.Router.AdminStateUp)
		argID++
	}

	if req.Router.ExternalGatewayInfo != nil {
		gatewayInfoJSON, _ := json.Marshal(*req.Router.ExternalGatewayInfo)
		updates = append(updates, fmt.Sprintf("external_gateway_info = $%d", argID))
		args = append(args, gatewayInfoJSON)
		argID++

		// Configure external gateway
		if err := svc.configureExternalGateway(c.Request.Context(), routerID, *req.Router.ExternalGatewayInfo); err != nil {
			log.Warn().Err(err).Str("router_id", routerID).Msg("failed to configure external gateway")
		}
	}

	if len(updates) == 0 {
		common.SendError(c, common.NewBadRequestError("no updates provided"))
		return
	}

	updates = append(updates, fmt.Sprintf("updated_at = $%d", argID))
	args = append(args, time.Now())
	argID++

	args = append(args, routerID, projectID)

	query := fmt.Sprintf("UPDATE routers SET %s WHERE id = $%d AND project_id = $%d",
		updateString(updates), argID, argID+1)

	_, err := database.DB.Exec(c.Request.Context(), query, args...)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update router"))
		return
	}

	// Return updated router
	svc.GetRouter(c)
}

// DeleteRouter deletes a router
func (svc *Service) DeleteRouter(c *gin.Context) {
	routerID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check if router has any interfaces attached
	var interfaceCount int
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT COUNT(*) FROM router_interfaces WHERE router_id = $1",
		routerID,
	).Scan(&interfaceCount)

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Error().Err(err).Str("operation", "delete_router_check").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to check router interfaces"))
		return
	}

	if interfaceCount > 0 {
		common.SendError(c, common.NewConflictError(
			fmt.Sprintf("router %s has %d interfaces attached", routerID, interfaceCount),
		))
		return
	}

	// Delete router namespace
	if err := svc.routerManager.DeleteRouterNamespace(routerID); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Msg("failed to delete router namespace")
	}

	// Delete from database
	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM routers WHERE id = $1 AND project_id = $2",
		routerID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_router").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete router"))
		return
	}

	c.Status(http.StatusNoContent)
}

// AddRouterInterface adds an interface (subnet) to a router
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

	// Verify router exists
	var routerExists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM routers WHERE id = $1 AND project_id = $2)",
		routerID, projectID,
	).Scan(&routerExists)

	if err != nil || !routerExists {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}

	// Get subnet information
	var subnetID, networkID, cidr, gatewayIP string
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT id, network_id, cidr, gateway_ip FROM subnets WHERE id = $1",
		req.SubnetID,
	).Scan(&subnetID, &networkID, &cidr, &gatewayIP)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("subnet"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface_subnet_lookup").Str("subnet_id", req.SubnetID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get subnet"))
		return
	}

	// Create a port for the router interface
	portID := uuid.New().String()
	macAddress := generateMAC()

	fixedIPs := []map[string]string{
		{
			"subnet_id":  subnetID,
			"ip_address": gatewayIP,
		},
	}
	fixedIPsJSON, _ := json.Marshal(fixedIPs)

	now := time.Now()
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, portID, "router-interface-"+routerID[:8], networkID, projectID, routerID, "network:router_interface",
		macAddress, true, "ACTIVE", fixedIPsJSON, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface_port").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create router interface port"))
		return
	}

	// Create router interface record
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO router_interfaces (id, router_id, port_id, subnet_id, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, uuid.New().String(), routerID, portID, subnetID, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "add_router_interface_record").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create router interface record"))
		return
	}

	// WS-3: if this router already has an external gateway, masquerade the
	// just-attached subnet's egress out the uplink now (real mode), so egress
	// works whether the gateway was set before or after this interface.
	if svc.mode != "stub" {
		var gwRaw []byte
		if err := database.DB.QueryRow(c.Request.Context(),
			"SELECT external_gateway_info FROM routers WHERE id = $1", routerID).Scan(&gwRaw); err == nil &&
			len(gwRaw) > 0 && string(gwRaw) != "null" {
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

	if err := svc.routerManager.AttachInterfaceToRouter(routerID, interfaceName, gatewayIP, cidrSuffix); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Str("interface", interfaceName).Msg("failed to attach interface to router")
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet_id": subnetID,
		"port_id":   portID,
	})
}

// RemoveRouterInterface removes an interface from a router
func (svc *Service) RemoveRouterInterface(c *gin.Context) {
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

	// Get port ID from subnet if not provided
	portID := req.PortID
	if portID == "" && req.SubnetID != "" {
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT port_id FROM router_interfaces WHERE router_id = $1 AND subnet_id = $2",
			routerID, req.SubnetID,
		).Scan(&portID)

		if errors.Is(err, pgx.ErrNoRows) {
			common.SendError(c, common.NewNotFoundError("router interface"))
			return
		}
	}

	// Detach interface from router namespace
	interfaceName := "qg-" + portID[:10]
	if err := svc.routerManager.DetachInterfaceFromRouter(routerID, interfaceName); err != nil {
		log.Warn().Err(err).Str("router_id", routerID).Str("interface", interfaceName).Msg("failed to detach interface from router")
	}

	// Delete router interface record
	_, err := database.DB.Exec(c.Request.Context(),
		"DELETE FROM router_interfaces WHERE router_id = $1 AND port_id = $2",
		routerID, portID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "remove_router_interface_record").Str("router_id", routerID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to remove router interface record"))
		return
	}

	// Delete the port
	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM ports WHERE id = $1 AND project_id = $2",
		portID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "remove_router_interface_port").Str("port_id", portID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete router interface port"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet_id": req.SubnetID,
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

	enableSNAT := true
	if val, ok := gatewayInfo["enable_snat"].(bool); ok {
		enableSNAT = val
	}

	// Get external network details
	var externalCIDR string
	err := database.DB.QueryRow(ctx,
		"SELECT cidr FROM subnets WHERE network_id = $1 LIMIT 1",
		networkID,
	).Scan(&externalCIDR)

	if err != nil {
		return fmt.Errorf("failed to get external network subnet: %w", err)
	}

	// Get all internal subnets connected to this router
	rows, err := database.DB.Query(ctx, `
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
