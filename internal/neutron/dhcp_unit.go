package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// A network's DHCP (SPEC-033 FR-037; research R29). Its records are the network's DHCP port, a
// network:dhcp port of the network's project that holds an address on each of the network's IPv4 DHCP
// subnets, and every other port's address on them. Its unit, on the network node, is the namespace
// qdhcp-<network> with the port's device, dnsmasq with a reservation per address, and the metadata
// listener in the namespace.

// applyDHCP converges a network's DHCP on this, the network node, with its metadata listener: to its
// records, those named in without read as gone, or to absent when absent is set or nothing is left to
// serve. The listener opens once the namespace holds the metadata address and closes before the
// namespace goes.
func (svc *Service) applyDHCP(ctx context.Context, networkID string, absent bool, without []string) error {
	var state *networking.DHCPState
	if !absent {
		s, found, err := readDHCPState(ctx, svc.pool, networkID, without)
		if err != nil {
			return err
		}
		if found {
			state = &s
		}
	}
	if state == nil {
		if svc.listeners != nil {
			if err := svc.listeners.Close(networkID); err != nil {
				return fmt.Errorf("network %s: close its metadata listener: %w", networkID, err)
			}
		}
		return svc.dhcp.ConvergeDHCP(ctx, networkID, nil)
	}
	// The network's bridge, which the DHCP leg joins: on this, the network node, it may not be made yet.
	if err := svc.brManager.CreateBridge(state.Bridge); err != nil {
		return fmt.Errorf("network %s: its bridge: %w", networkID, err)
	}
	if err := svc.dhcp.ConvergeDHCP(ctx, networkID, state); err != nil {
		return err
	}
	if svc.listeners != nil {
		if err := svc.listeners.Open(ctx, networkID); err != nil {
			return fmt.Errorf("network %s: open its metadata listener: %w", networkID, err)
		}
	}
	return nil
}

// convergeDHCPUnit converges a network's DHCP on the network node through the node call: as its
// records say, those named in without read as gone (a deletion's first step, R34).
func (svc *Service) convergeDHCPUnit(ctx context.Context, networkID string, without ...string) error {
	return svc.caller.converge(ctx, svc.networkNodeName(),
		unitRequest{Unit: unitDHCP, IDs: []string{networkID}, AbsentIDs: without})
}

// removeDHCPUnit converges a network's DHCP to absent on the network node, through the node call.
func (svc *Service) removeDHCPUnit(ctx context.Context, networkID string) error {
	return svc.caller.converge(ctx, svc.networkNodeName(),
		unitRequest{Unit: unitDHCP, IDs: []string{networkID}, Absent: true})
}

// reapplyDHCP converges a network's DHCP from its records again after a change that did not stand. What
// it cannot apply, the reconciliation pass converges.
func (svc *Service) reapplyDHCP(ctx context.Context, networkID string) {
	if err := svc.convergeDHCPUnit(context.WithoutCancel(ctx), networkID); err != nil {
		log.Error().Err(err).Str("network_id", networkID).Msg("converge a network's DHCP from its records again; the reconciliation pass converges it")
	}
}

// dhcpPort is a network's DHCP port as stored.
type dhcpPort struct {
	id    string
	fixed common.FixedIPs
}

// dhcpPortOf reads a network's DHCP port: its network:dhcp port whose device is the network, in the
// network's project, as syncDHCPPort writes it — never a port another project gave that owner — the
// oldest should there be two (SPEC-033 FR-062).
func dhcpPortOf(ctx context.Context, q subnetQuerier, networkID string) (dhcpPort, bool, error) {
	var p dhcpPort
	err := q.QueryRow(ctx, `
		SELECT id::text, fixed_ips FROM ports
		 WHERE network_id::text = $1 AND device_owner = $2 AND device_id::text = $1
		   AND project_id = (SELECT project_id FROM networks WHERE id::text = $1)
		 ORDER BY created_at, id LIMIT 1`, networkID, ownerDHCP).Scan(&p.id, &p.fixed)
	if errors.Is(err, database.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("network %s: read its DHCP port: %w", networkID, err)
	}
	return p, true, nil
}

// dhcpSubnet is one of a network's IPv4 DHCP subnets as stored.
type dhcpSubnet struct {
	id, cidr, gateway string
	dns               []string
}

// dhcpSubnetsOf reads a network's IPv4 DHCP subnets in id order. DHCP serves IPv4 only, as it did.
func dhcpSubnetsOf(ctx context.Context, q subnetQuerier, networkID string) ([]dhcpSubnet, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, cidr, COALESCE(gateway_ip, ''), dns_nameservers FROM subnets
		 WHERE network_id::text = $1 AND enable_dhcp = true AND ip_version = 4
		 ORDER BY id`, networkID)
	if err != nil {
		return nil, fmt.Errorf("network %s: read its DHCP subnets: %w", networkID, err)
	}
	defer rows.Close()
	var out []dhcpSubnet
	for rows.Next() {
		var s dhcpSubnet
		if err := rows.Scan(&s.id, &s.cidr, &s.gateway, &s.dns); err != nil {
			return nil, fmt.Errorf("network %s: read its DHCP subnets: %w", networkID, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("network %s: read its DHCP subnets: %w", networkID, err)
	}
	return out, nil
}

// readDHCPState reads a network's DHCP as the network node is to hold it (R29): its DHCP port, each
// DHCP subnet the port has an address on, and a reservation for every other port's address on those.
// Records named in without — a subnet, a port, the DHCP port itself — are read as gone. found is false
// when nothing is left to serve.
func readDHCPState(ctx context.Context, q subnetQuerier, networkID string, without []string) (networking.DHCPState, bool, error) {
	s := networking.DHCPState{NetworkID: networkID, Bridge: networking.BridgeNameForNetwork(networkID)}
	gone := func(id string) bool { return slices.Contains(without, id) }
	port, found, err := dhcpPortOf(ctx, q, networkID)
	if err != nil || !found || gone(port.id) {
		return s, false, err
	}
	s.PortID = port.id
	subnets, err := dhcpSubnetsOf(ctx, q, networkID)
	if err != nil {
		return s, false, err
	}
	served := map[string]bool{}
	for _, sub := range subnets {
		address := addressOn(port.fixed, sub.id)
		if gone(sub.id) || address == "" {
			continue
		}
		served[sub.id] = true
		s.Subnets = append(s.Subnets, networking.DHCPSubnet{ID: sub.id, CIDR: sub.cidr, Address: address, Gateway: sub.gateway, DNS: sub.dns})
	}
	if len(s.Subnets) == 0 {
		return s, false, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, mac_address, fixed_ips FROM ports
		 WHERE network_id::text = $1 AND id::text <> $2 AND mac_address <> ''
		 ORDER BY id`, networkID, port.id)
	if err != nil {
		return s, false, fmt.Errorf("network %s: read its ports: %w", networkID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, mac string
		var fixed common.FixedIPs
		if err := rows.Scan(&id, &mac, &fixed); err != nil {
			return s, false, fmt.Errorf("network %s: read its ports: %w", networkID, err)
		}
		if gone(id) {
			continue
		}
		for _, f := range fixed {
			if served[f.SubnetID] && f.IPAddress != "" {
				s.Hosts = append(s.Hosts, networking.DHCPHost{MAC: mac, IP: f.IPAddress})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return s, false, fmt.Errorf("network %s: read its ports: %w", networkID, err)
	}
	return s, true, nil
}

// addressOn is a port's address on a subnet, or "".
func addressOn(fixed common.FixedIPs, subnetID string) string {
	for _, f := range fixed {
		if f.SubnetID == subnetID {
			return f.IPAddress
		}
	}
	return ""
}

// lockNetwork takes a network's row lock, under which its DHCP port is created, changed and removed
// (R29). A transaction takes it after any subnet's lock it takes, as a floating-IP create does: a port
// or gateway insert holds a subnet's lock and then a key-share lock on its network, which a network
// lock taken before the subnet would wait on in a cycle. A subnet create takes it before inserting its
// subnet, and locks no other.
func lockNetwork(ctx context.Context, tx database.Tx, networkID string) error {
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM networks WHERE id::text = $1 FOR UPDATE", networkID).Scan(&id); err != nil {
		return fmt.Errorf("network %s: lock it: %w", networkID, err)
	}
	return nil
}

// syncDHCPPort makes a network's DHCP port match its DHCP subnets (R29), as upstream's DHCP agent has
// its port made: created with the first, of the network's project, its device the network; an address
// on each, the lowest free of its pools; the address on a subnet that is gone dropped; removed with the
// last. A missing address is allocated only on the subnets in allocateOn, whose locks the caller holds:
// a subnet create's own subnet, or, in the reconciliation pass, every DHCP subnet. So a request never
// waits for another subnet's lock while it holds the network's, and a port deleted through the API is
// made again by the pass, as upstream's agent has a new one made when it resyncs. A subnet with no
// free address gets none, and no DHCP, as upstream's port update fails there. The caller holds the
// network's lock.
func syncDHCPPort(ctx context.Context, tx database.Tx, networkID string, allocateOn []string) error {
	subnets, err := dhcpSubnetsOf(ctx, tx, networkID)
	if err != nil {
		return err
	}
	port, found, err := dhcpPortOf(ctx, tx, networkID)
	if err != nil {
		return err
	}
	if len(subnets) == 0 {
		if found {
			if _, err := tx.Exec(ctx, "DELETE FROM ports WHERE id::text = $1", port.id); err != nil {
				return fmt.Errorf("network %s: remove its DHCP port: %w", networkID, err)
			}
		}
		return nil
	}
	var fixed common.FixedIPs
	for _, sub := range subnets {
		if address := addressOn(port.fixed, sub.id); address != "" {
			fixed = append(fixed, common.FixedIP{SubnetID: sub.id, IPAddress: address})
			continue
		}
		if !slices.Contains(allocateOn, sub.id) {
			continue
		}
		address, err := allocateAddress(ctx, tx, sub.id, pickLowest)
		if errors.Is(err, ErrNoAvailableIP) {
			log.Warn().Str("network_id", networkID).Str("subnet_id", sub.id).Msg("no free address for the network's DHCP port; the subnet has no DHCP")
			continue
		}
		if err != nil {
			return err
		}
		fixed = append(fixed, common.FixedIP{SubnetID: sub.id, IPAddress: address})
	}
	if (!found && len(fixed) == 0) || (found && slices.Equal(fixed, port.fixed)) {
		return nil
	}
	raw, err := json.Marshal(fixed)
	if err != nil {
		return fmt.Errorf("network %s: its DHCP port's addresses: %w", networkID, err)
	}
	now := time.Now()
	if found {
		if _, err := tx.Exec(ctx, "UPDATE ports SET fixed_ips = $1, updated_at = $2 WHERE id::text = $3", raw, now, port.id); err != nil {
			return fmt.Errorf("network %s: change its DHCP port's addresses: %w", networkID, err)
		}
		return nil
	}
	var project *string
	if err := tx.QueryRow(ctx, "SELECT project_id::text FROM networks WHERE id::text = $1", networkID).Scan(&project); err != nil {
		return fmt.Errorf("network %s: read its project: %w", networkID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, '', $2, $3, $2, $4, $5, true, 'ACTIVE', $6, $7, $7)`,
		uuid.NewString(), networkID, project, ownerDHCP, generateMAC(), raw, now); err != nil {
		return fmt.Errorf("network %s: create its DHCP port: %w", networkID, err)
	}
	return nil
}

// dhcpPortInLine reports whether a network's DHCP port matches its DHCP subnets: there when they are,
// with an address on each and on no other subnet, and gone when there are none. The reconciliation pass
// locks nothing for a network whose port is.
func dhcpPortInLine(ctx context.Context, q subnetQuerier, networkID string) (bool, error) {
	subnets, err := dhcpSubnetsOf(ctx, q, networkID)
	if err != nil {
		return false, err
	}
	port, found, err := dhcpPortOf(ctx, q, networkID)
	if err != nil || len(subnets) == 0 || !found {
		return len(subnets) == 0 && !found, err
	}
	served := map[string]bool{}
	for _, sub := range subnets {
		if addressOn(port.fixed, sub.id) == "" {
			return false, nil
		}
		served[sub.id] = true
	}
	for _, f := range port.fixed {
		if !served[f.SubnetID] {
			return false, nil
		}
	}
	return true, nil
}

// realignDHCPPort brings a network's DHCP port in line with its DHCP subnets in a transaction of its
// own, allocating on any of them: the reconciliation pass's step. It locks the DHCP subnets in creation
// order, as a floating-IP create and a gateway change take theirs, and the network after them.
func realignDHCPPort(ctx context.Context, pool database.DB, networkID string) error {
	return database.WithTx(ctx, pool, func(tx database.Tx) error {
		ids, err := queryStrings(ctx, tx, `
			SELECT id::text FROM subnets WHERE network_id::text = $1 AND enable_dhcp = true AND ip_version = 4
			 ORDER BY created_at, id`, networkID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := LockSubnet(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := lockNetwork(ctx, tx, networkID); err != nil {
			return err
		}
		return syncDHCPPort(ctx, tx, networkID, ids)
	})
}

// servesDHCP reports whether a port's addresses are on a subnet whose DHCP serves them: an IPv4 subnet
// with DHCP. Only such a port's create or delete changes what its network's dnsmasq serves. A failed
// read answers yes, so the unit is converged and decides.
func servesDHCP(ctx context.Context, q subnetQuerier, fixed common.FixedIPs) bool {
	var ids []string
	for _, f := range fixed {
		ids = append(ids, f.SubnetID)
	}
	if len(ids) == 0 {
		return false
	}
	var serves bool
	if err := q.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM subnets WHERE id::text = ANY($1) AND enable_dhcp = true AND ip_version = 4)", ids,
	).Scan(&serves); err != nil {
		log.Warn().Err(err).Strs("subnets", ids).Msg("read whether the subnets serve DHCP; converging the network's DHCP")
		return true
	}
	return serves
}
