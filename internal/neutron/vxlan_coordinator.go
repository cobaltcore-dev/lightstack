package neutron

import (
	"context"
	"fmt"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// VXLANCoordinator coordinates VXLAN network state across compute nodes
type VXLANCoordinator struct {
	pool          *pgxpool.Pool
	vxlanManager  *networking.VXLANManager
	nodeRegistry  *compute.NodeRegistry
	nsManager     *networking.NetworkNamespaceManager
	pollInterval  time.Duration
	vniRangeStart int
	vniRangeEnd   int
	stopChan      chan struct{}
}

// NewVXLANCoordinator creates a new VXLAN coordinator
func NewVXLANCoordinator(
	pool *pgxpool.Pool,
	vxlanManager *networking.VXLANManager,
	nodeRegistry *compute.NodeRegistry,
	nsManager *networking.NetworkNamespaceManager,
	pollInterval time.Duration,
	vniRangeStart, vniRangeEnd int,
) *VXLANCoordinator {
	if pollInterval == 0 {
		pollInterval = 1 * time.Second
	}

	return &VXLANCoordinator{
		pool:          pool,
		vxlanManager:  vxlanManager,
		nodeRegistry:  nodeRegistry,
		nsManager:     nsManager,
		pollInterval:  pollInterval,
		vniRangeStart: vniRangeStart,
		vniRangeEnd:   vniRangeEnd,
		stopChan:      make(chan struct{}),
	}
}

// Start begins the coordination loop
func (vc *VXLANCoordinator) Start(ctx context.Context) {
	ticker := time.NewTicker(vc.pollInterval)
	defer ticker.Stop()

	// Do initial sync
	vc.syncNetworks(ctx)
	vc.syncPorts(ctx)

	for {
		select {
		case <-ticker.C:
			vc.syncNetworks(ctx)
			vc.syncPorts(ctx)
		case <-vc.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the coordination loop
func (vc *VXLANCoordinator) Stop() {
	close(vc.stopChan)
}

// syncNetworks ensures all VXLAN networks exist on this node
func (vc *VXLANCoordinator) syncNetworks(ctx context.Context) {
	// Get all networks that need VXLAN
	rows, err := vc.pool.Query(ctx, `
		SELECT n.id, n.name, n.project_id, v.vni
		FROM networks n
		LEFT JOIN network_vni_allocations v ON n.id = v.network_id
		WHERE n.network_type = 'vxlan'
	`)

	if err != nil {
		log.Error().Err(err).Msg("failed to query networks for VXLAN coordination")
		return
	}
	defer rows.Close()

	localIP := vc.nodeRegistry.GetTunnelIP()

	for rows.Next() {
		var networkID, name, projectID string
		var vni *int

		if err := rows.Scan(&networkID, &name, &projectID, &vni); err != nil {
			log.Warn().Err(err).Str("operation", "syncNetworks").Msg("failed to scan row")
			continue
		}

		// Allocate VNI if not already allocated
		if vni == nil {
			allocatedVNI, err := vc.allocateVNI(ctx, networkID)
			if err != nil {
				log.Error().Err(err).Str("network_id", networkID).Msg("failed to allocate VNI")
				continue
			}
			vni = &allocatedVNI
		}

		// Create VXLAN interface
		if err := vc.vxlanManager.CreateVXLAN(networkID, *vni, localIP); err != nil {
			log.Error().Err(err).Str("network_id", networkID).Msg("failed to create VXLAN")
			continue
		}

		// Attach to bridge
		bridgeName := "br-" + networkID[:8]
		nsName := vc.nsManager.GetNamespaceName(projectID)
		if err := vc.vxlanManager.AttachToBridge(networkID, bridgeName, true, nsName); err != nil {
			log.Error().Err(err).Str("network_id", networkID).Msg("failed to attach VXLAN to bridge")
		}
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}
}

// syncPorts ensures all port FDB entries are configured
func (vc *VXLANCoordinator) syncPorts(ctx context.Context) {
	// Get all active compute nodes
	nodes, err := vc.nodeRegistry.ListActiveNodes(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to list active nodes")
		return
	}

	// Get all FDB entries from database
	rows, err := vc.pool.Query(ctx, `
		SELECT network_id, mac_address, vtep_ip
		FROM vxlan_fdb_entries
	`)

	if err != nil {
		log.Error().Err(err).Msg("failed to query FDB entries")
		return
	}
	defer rows.Close()

	localIP := vc.nodeRegistry.GetTunnelIP()

	for rows.Next() {
		var networkID, macAddress, vtepIP string

		if err := rows.Scan(&networkID, &macAddress, &vtepIP); err != nil {
			log.Warn().Err(err).Str("operation", "syncPorts").Msg("failed to scan row")
			continue
		}

		// Skip entries for this node's own ports
		if vtepIP == localIP {
			continue
		}

		// Verify the remote node is still active
		nodeActive := false
		for _, node := range nodes {
			if node.TunnelIP == vtepIP {
				nodeActive = true
				break
			}
		}

		if !nodeActive {
			// Remote node is down, remove FDB entry (best-effort: the
			// loop continues regardless; a leftover entry will be
			// reconciled on the next sync cycle).
			if err := vc.vxlanManager.RemoveFDBEntry(networkID, macAddress); err != nil {
				log.Warn().Err(err).Str("mac", macAddress).Str("network_id", networkID).Msg("failed to remove FDB entry for inactive node")
			}
			continue
		}

		// Add FDB entry
		if err := vc.vxlanManager.AddFDBEntry(networkID, macAddress, vtepIP); err != nil {
			log.Error().Err(err).Str("mac", macAddress).Str("network_id", networkID).Msg("failed to add FDB entry")
		}
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}
}

// allocateVNI allocates a VNI for a network using a single atomic SQL query
func (vc *VXLANCoordinator) allocateVNI(ctx context.Context, networkID string) (int, error) {
	// Find and reserve the lowest available VNI in the configured range atomically.
	// This avoids the O(N) sequential-insert loop and races under concurrent allocation.
	var vni int
	err := vc.pool.QueryRow(ctx, `
		WITH next_vni AS (
			SELECT generate_series AS vni
			FROM generate_series($1::int, $2::int)
			WHERE generate_series NOT IN (SELECT vni FROM network_vni_allocations)
			LIMIT 1
		)
		INSERT INTO network_vni_allocations (network_id, vni)
		SELECT $3, vni FROM next_vni
		RETURNING vni
	`, vc.vniRangeStart, vc.vniRangeEnd, networkID).Scan(&vni)

	if err != nil {
		return 0, fmt.Errorf("no available VNI in range %d-%d: %w", vc.vniRangeStart, vc.vniRangeEnd, err)
	}

	return vni, nil
}

// DistributeFDBEntry adds an FDB entry to the database for distribution
// This should be called when a new port is created
func (vc *VXLANCoordinator) DistributeFDBEntry(ctx context.Context, networkID, portID, macAddress string) error {
	localIP := vc.nodeRegistry.GetTunnelIP()
	now := time.Now()

	_, err := vc.pool.Exec(ctx, `
		INSERT INTO vxlan_fdb_entries (network_id, mac_address, vtep_ip, port_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (network_id, mac_address)
		DO UPDATE SET
			vtep_ip = EXCLUDED.vtep_ip,
			port_id = EXCLUDED.port_id,
			updated_at = EXCLUDED.updated_at
	`, networkID, macAddress, localIP, portID, now, now)

	return err
}

// RemoveFDBEntry removes an FDB entry from the database
// This should be called when a port is deleted
func (vc *VXLANCoordinator) RemoveFDBEntry(ctx context.Context, portID string) error {
	_, err := vc.pool.Exec(ctx, `
		DELETE FROM vxlan_fdb_entries
		WHERE port_id = $1
	`, portID)

	return err
}

// GetVNI returns the VNI for a network (allocating if necessary)
func (vc *VXLANCoordinator) GetVNI(ctx context.Context, networkID string) (int, error) {
	// Try to get existing VNI
	var vni int
	err := vc.pool.QueryRow(ctx, `
		SELECT vni FROM network_vni_allocations WHERE network_id = $1
	`, networkID).Scan(&vni)

	if err == nil {
		return vni, nil
	}

	// Allocate new VNI
	return vc.allocateVNI(ctx, networkID)
}
