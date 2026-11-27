package neutron

import (
	"errors"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// GetAutoAllocatedTopology returns the auto-allocated network topology for a project
func (svc *Service) GetAutoAllocatedTopology(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Check if auto-allocated network exists for this project
	var networkID, networkName string
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name
		FROM networks
		WHERE project_id = $1 AND name = 'auto-allocated-network'
		LIMIT 1
	`, projectID).Scan(&networkID, &networkName)

	if errors.Is(err, database.ErrNoRows) {
		// Hand-rolled here long before the rest of the service emitted Neutron's envelope. Now that
		// the shared path does, keeping a second copy would leave two descriptions of one shape free
		// to drift — the defect B9 is, in a different place (SPEC-024 FR-009).
		common.SendError(c, common.NewNotFoundError("auto-allocated topology"))
		return
	}

	if err != nil {
		log.Error().Err(err).Str("operation", "get_auto_allocated_topology").Str("project_id", projectID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get auto-allocated topology"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"auto_allocated_topology": gin.H{
			"id": networkID,
		},
	})
}

// CreateAutoAllocatedTopology creates an auto-allocated network topology for a project
func (svc *Service) CreateAutoAllocatedTopology(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Check if auto-allocated network already exists
	var existingNetworkID string
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id
		FROM networks
		WHERE project_id = $1 AND name = 'auto-allocated-network'
		LIMIT 1
	`, projectID).Scan(&existingNetworkID)

	if err == nil {
		// Already exists, return it
		c.JSON(http.StatusOK, gin.H{
			"auto_allocated_topology": gin.H{
				"id": existingNetworkID,
			},
		})
		return
	}

	// The topology through the same functions as its parts (SPEC-033 R40): one transaction makes the
	// network with its zone, its subnet and the subnet's DHCP port; then the network's bridge on this node
	// and its DHCP on the network node. A part that cannot be applied takes the whole topology back,
	// compare-and-set, and answers 500.
	ctx := c.Request.Context()
	networkID, subnetID := uuid.New().String(), uuid.New().String()
	now := time.Now()
	networkType, mtu := svc.networkTypeAndMTU()
	err = database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := database.AdvisoryLock(ctx, tx, ctZoneLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at, ct_zone)
			VALUES ($1, 'auto-allocated-network', $2, true, 'ACTIVE', false, $4, $5, $3, $3, `+nextZone+`)`,
			networkID, projectID, now, networkType, mtu); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
			VALUES ($1, 'auto-allocated-subnet', $2, $3, '192.168.100.0/24', '192.168.100.1', 4, true, $4, $4)`,
			subnetID, networkID, projectID, now); err != nil {
			return err
		}
		// The network is this transaction's alone: no lock to take before its DHCP port.
		return syncDHCPPort(ctx, tx, networkID, []string{subnetID})
	})
	if err != nil {
		log.Error().Err(err).Str("operation", "create_auto_allocated_topology").Str("project_id", projectID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create auto-allocated topology"))
		return
	}
	apply := func() error {
		if err := svc.brManager.CreateBridge(networking.BridgeNameForNetwork(networkID)); err != nil {
			return err
		}
		return svc.convergeDHCPUnit(ctx, networkID)
	}
	if err := apply(); err != nil {
		log.Error().Err(err).Str("network_id", networkID).Msg("the auto-allocated topology could not be applied; undoing it")
		svc.undoNetworkCreate(ctx, networkID, now, subnetID)
		svc.reapplyDHCP(ctx, networkID)
		common.SendError(c, common.NewInternalServerError("failed to create auto-allocated topology"))
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"auto_allocated_topology": gin.H{
			"id": networkID,
		},
	})
}

// DeleteAutoAllocatedTopology deletes the auto-allocated network topology for a project
func (svc *Service) DeleteAutoAllocatedTopology(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Find auto-allocated network
	var networkID string
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id
		FROM networks
		WHERE project_id = $1 AND name = 'auto-allocated-network'
		LIMIT 1
	`, projectID).Scan(&networkID)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("auto-allocated topology"))
		return
	}

	if err == nil {
		// The same deletion as any network's, upstream's delete_network: refused while in use, under the
		// network's lock (SPEC-033 T185).
		err = svc.removeNetwork(c.Request.Context(), networkID)
	}
	if err != nil {
		sendChangeError(c, err, "delete", "auto-allocated topology")
		return
	}
	c.Status(http.StatusNoContent)
}
