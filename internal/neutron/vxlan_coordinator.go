package neutron

import (
	"context"
	"fmt"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// VXLANCoordinator keeps this node's VXLAN devices and forwarding entries matching the records, in both
// directions (SPEC-033 FR-040, FR-044): a device per VXLAN network, attached to its bridge, and an entry
// per port on another active node. An entry it wrote whose row is gone — its port deleted, which takes
// the row — is removed, and so is a deleted network's device; neither after an incomplete read.
type VXLANCoordinator struct {
	pool          database.DB
	vxlan         vxlanDataplane
	nodeRegistry  activeNodes
	pollInterval  time.Duration
	vniRangeStart int
	vniRangeEnd   int
	stopChan      chan struct{}

	// readNetworks and readRows read the VXLAN networks and the forwarding rows, and whether every row
	// was read. Fields so a test can scope a pass to its own records, or make a read partial.
	readNetworks func(ctx context.Context) ([]vxlanNetwork, bool, error)
	readRows     func(ctx context.Context) ([]fdbRow, bool, error)
}

// vxlanDataplane is the node's VXLAN devices and their forwarding entries: *networking.VXLANManager, or
// a fake in tests.
type vxlanDataplane interface {
	CreateVXLAN(networkID string, vni int, localIP string) error
	AttachToBridge(networkID, bridgeName string) error
	AddFDBEntry(networkID, macAddress, remoteIP string) error
	RemoveFDBEntry(networkID, macAddress string) error
	ListFDBEntries(networkID string) (map[string]string, error)
	VXLANDevices() ([]string, error)
	DeleteVXLANDevice(name string) error
	GetVXLANName(networkID string) string
}

// activeNodes is the node registry as the coordinator reads it: *compute.NodeRegistry.
type activeNodes interface {
	ListActiveNodes(ctx context.Context) ([]compute.ComputeNode, error)
	GetTunnelIP() string
}

// vxlanNetwork is a VXLAN network and its VNI, nil before one is allocated.
type vxlanNetwork struct {
	id  string
	vni *int
}

// fdbRow is a port's forwarding row: its network, MAC and node's tunnel address.
type fdbRow struct{ network, mac, vtep string }

// VNIRange is the range of VXLAN network identifiers a coordinator allocates from, both ends included.
type VNIRange struct{ Start, End int }

// NewVXLANCoordinator creates a new VXLAN coordinator.
func NewVXLANCoordinator(pool database.DB, vxlan vxlanDataplane, nodeRegistry activeNodes, pollInterval time.Duration, vnis VNIRange) *VXLANCoordinator {
	if pollInterval == 0 {
		pollInterval = 1 * time.Second
	}

	vc := &VXLANCoordinator{
		pool:          pool,
		vxlan:         vxlan,
		nodeRegistry:  nodeRegistry,
		pollInterval:  pollInterval,
		vniRangeStart: vnis.Start,
		vniRangeEnd:   vnis.End,
		stopChan:      make(chan struct{}),
	}
	vc.readNetworks, vc.readRows = vc.readVXLANNetworks, vc.readFDBRows
	return vc
}

// Start begins the coordination loop
func (vc *VXLANCoordinator) Start(ctx context.Context) {
	ticker := time.NewTicker(vc.pollInterval)
	defer ticker.Stop()

	vc.sync(ctx)
	for {
		select {
		case <-ticker.C:
			vc.sync(ctx)
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

// sync is one pass: the devices are listed first, then the records read (R24), then every network's
// device and entries converged, then the devices of networks without a record removed.
func (vc *VXLANCoordinator) sync(ctx context.Context) {
	devices, listErr := vc.vxlan.VXLANDevices()
	if listErr != nil {
		log.Warn().Err(listErr).Msg("VXLAN: list the devices")
	}
	networks, complete, err := vc.readNetworks(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to query networks for VXLAN coordination")
		return
	}
	vc.syncNetworks(ctx, networks)
	vc.syncPorts(ctx, networks)
	if listErr != nil || !complete {
		log.Warn().Msg("VXLAN: the networks were not all read; removing no device this pass")
		return
	}
	want := map[string]bool{}
	for _, n := range networks {
		want[vc.vxlan.GetVXLANName(n.id)] = true
	}
	for _, name := range devices {
		if want[name] {
			continue
		}
		if err := vc.vxlan.DeleteVXLANDevice(name); err != nil {
			log.Warn().Err(err).Str("device", name).Msg("VXLAN: remove a deleted network's device")
		} else {
			log.Info().Str("device", name).Msg("VXLAN: removed a deleted network's device")
		}
	}
}

// readVXLANNetworks reads every VXLAN network with its VNI.
func (vc *VXLANCoordinator) readVXLANNetworks(ctx context.Context) ([]vxlanNetwork, bool, error) {
	rows, err := vc.pool.Query(ctx, `
		SELECT n.id::text, v.vni
		FROM networks n
		LEFT JOIN network_vni_allocations v ON n.id = v.network_id
		WHERE n.network_type = 'vxlan'
	`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	complete := true
	var out []vxlanNetwork
	for rows.Next() {
		var n vxlanNetwork
		if err := rows.Scan(&n.id, &n.vni); err != nil {
			log.Warn().Err(err).Str("operation", "syncNetworks").Msg("failed to scan row")
			complete = false
			continue
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		complete = false
	}
	return out, complete, nil
}

// syncNetworks ensures every VXLAN network's device on this node, with a VNI, attached to its bridge.
func (vc *VXLANCoordinator) syncNetworks(ctx context.Context, networks []vxlanNetwork) {
	localIP := vc.nodeRegistry.GetTunnelIP()
	for _, n := range networks {
		vni := n.vni
		if vni == nil {
			allocated, err := vc.allocateVNI(ctx, n.id)
			if err != nil {
				log.Error().Err(err).Str("network_id", n.id).Msg("failed to allocate VNI")
				continue
			}
			vni = &allocated
		}
		if err := vc.vxlan.CreateVXLAN(n.id, *vni, localIP); err != nil {
			log.Error().Err(err).Str("network_id", n.id).Msg("failed to create VXLAN")
			continue
		}

		// Attach to the network's bridge, in the namespace Lightstack runs in — where the bridge and the guest
		// TAPs are, and where every other VXLANManager method already looks.
		//
		// This used to move the device into light-ns-<project> and attach it to a bridge there, which
		// put it out of reach of its own manager: CreateVXLAN, DeleteVXLANDevice, AddFDBEntry and
		// RemoveFDBEntry all resolve it with netlink.LinkByName in the root namespace. So FDB entries
		// could not be installed at all (no cross-node forwarding, which is the entire point), the
		// device's deletion reported success while leaking it, and CreateVXLAN's existence check
		// missed it and rebuilt it every pass. The guests were never on the namespaced bridge either
		// (SPEC-024 / cloud-storage#546 F10).
		bridgeName := networking.BridgeNameForNetwork(n.id)
		if err := vc.vxlan.AttachToBridge(n.id, bridgeName); err != nil {
			log.Error().Err(err).Str("network_id", n.id).Msg("failed to attach VXLAN to bridge")
		}
	}
}

// readFDBRows reads every port's forwarding row.
func (vc *VXLANCoordinator) readFDBRows(ctx context.Context) ([]fdbRow, bool, error) {
	rows, err := vc.pool.Query(ctx, `SELECT network_id::text, mac_address, vtep_ip FROM vxlan_fdb_entries`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	complete := true
	var out []fdbRow
	for rows.Next() {
		var r fdbRow
		if err := rows.Scan(&r.network, &r.mac, &r.vtep); err != nil {
			log.Warn().Err(err).Str("operation", "syncPorts").Msg("failed to scan row")
			complete = false
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		complete = false
	}
	return out, complete, nil
}

// syncPorts makes each VXLAN network's device on this node hold exactly the entries of its ports on
// other active nodes: each row's entry added where it is missing or points elsewhere, and, after a
// complete read of the rows and the nodes, every entry Lightstack wrote that no such row holds removed
// — its port deleted, or its node no longer active. This node's own ports need none.
func (vc *VXLANCoordinator) syncPorts(ctx context.Context, networks []vxlanNetwork) {
	nodes, err := vc.nodeRegistry.ListActiveNodes(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to list active nodes")
		return
	}
	rows, complete, err := vc.readRows(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to query FDB entries")
		return
	}
	active := map[string]bool{}
	for _, n := range nodes {
		active[n.TunnelIP] = true
	}
	localIP := vc.nodeRegistry.GetTunnelIP()
	want := map[string]map[string]string{} // network → MAC → remote tunnel address
	for _, r := range rows {
		if r.vtep == localIP || !active[r.vtep] {
			continue
		}
		if want[r.network] == nil {
			want[r.network] = map[string]string{}
		}
		want[r.network][r.mac] = r.vtep
	}
	for _, n := range networks {
		held, err := vc.vxlan.ListFDBEntries(n.id)
		if err != nil {
			log.Warn().Err(err).Str("network_id", n.id).Msg("VXLAN: list the forwarding entries")
			continue
		}
		for mac, remote := range want[n.id] {
			if held[mac] == remote {
				continue
			}
			if err := vc.vxlan.AddFDBEntry(n.id, mac, remote); err != nil {
				log.Error().Err(err).Str("mac", mac).Str("network_id", n.id).Msg("failed to add FDB entry")
			}
		}
		if !complete {
			continue
		}
		for mac := range held {
			if _, ok := want[n.id][mac]; ok {
				continue
			}
			if err := vc.vxlan.RemoveFDBEntry(n.id, mac); err != nil {
				log.Warn().Err(err).Str("mac", mac).Str("network_id", n.id).Msg("VXLAN: remove a forwarding entry no row holds")
			}
		}
	}
	if !complete {
		log.Warn().Msg("VXLAN: the forwarding rows were not all read; removing no entry this pass")
	}
}

// allocateVNI allocates a VNI for a network using a single atomic SQL query
func (vc *VXLANCoordinator) allocateVNI(ctx context.Context, networkID string) (int, error) {
	// Find and reserve the lowest available VNI in the configured range atomically.
	// This avoids the O(N) sequential-insert loop and races under concurrent allocation.
	// The recursive CTE counts up the range and both backends stop it at the first
	// free VNI, so it never materialises the range.
	var vni int
	err := vc.pool.QueryRow(ctx, `
		WITH RECURSIVE candidate(vni) AS (
			SELECT $1::int WHERE $1::int <= $2::int
			UNION ALL
			SELECT vni + 1 FROM candidate WHERE vni < $2::int
		)
		INSERT INTO network_vni_allocations (network_id, vni)
		SELECT $3, vni FROM candidate
		 WHERE vni NOT IN (SELECT vni FROM network_vni_allocations)
		 LIMIT 1
		RETURNING vni
	`, vc.vniRangeStart, vc.vniRangeEnd, networkID).Scan(&vni)

	if err != nil {
		return 0, fmt.Errorf("no available VNI in range %d-%d: %w", vc.vniRangeStart, vc.vniRangeEnd, err)
	}

	return vni, nil
}

// RecordFDB writes a port's forwarding row, naming this node's tunnel address, in the transaction that
// stores the port, so a port never exists without its row (SPEC-033 FR-045, R40). The kernel's entries
// on the nodes are reconciliation's.
func (vc *VXLANCoordinator) RecordFDB(ctx context.Context, tx database.Tx, networkID, portID, macAddress string) error {
	now := time.Now()
	_, err := tx.Exec(ctx, `
		INSERT INTO vxlan_fdb_entries (network_id, mac_address, vtep_ip, port_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (network_id, mac_address)
		DO UPDATE SET
			vtep_ip = EXCLUDED.vtep_ip,
			port_id = EXCLUDED.port_id,
			updated_at = EXCLUDED.updated_at
	`, networkID, macAddress, vc.nodeRegistry.GetTunnelIP(), portID, now, now)
	if err != nil {
		return fmt.Errorf("record the forwarding row of port %s: %w", portID, err)
	}
	return nil
}
