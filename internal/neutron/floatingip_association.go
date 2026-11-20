package neutron

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// How a floating IP gets its address and its router, as upstream's do (SPEC-033 round 2: FR-027–FR-029,
// FR-031, FR-054; research R36).

// floatingIPAllocationLock is the transaction-scoped advisory lock every floating-IP create takes
// first, so creates on any external network never race for an address (FR-027). On SQLite the lock is
// a registered no-op: its writers are serialised already.
const floatingIPAllocationLock = "lightstack:floating-ip-allocation"

func takeFloatingIPAllocationLock(ctx context.Context, tx database.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", floatingIPAllocationLock); err != nil {
		return fmt.Errorf("take the floating-IP allocation lock: %w", err)
	}
	return nil
}

func badFloatingIPRequest(message string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, "BadRequest", "Bad floatingip request: "+message+".")
}

// floatingPort is the port a floating IP reaches and the fixed IP on it.
type floatingPort struct {
	id, networkID, subnetID, fixedIP string
}

// portFixedIP reads a port of the caller's project, of any project for an administrator (SPEC-033
// FR-058), and the fixed IP a floating IP reaches on it, as upstream's _internal_fip_assoc_data
// chooses it: the requested one, which must be one of the port's (400 otherwise, FR-054), or the
// port's only IPv4 address. forUpdate locks the port.
func portFixedIP(ctx context.Context, q subnetQuerier, portID, projectID, requested string, admin, forUpdate bool) (floatingPort, error) {
	query := "SELECT network_id::text, fixed_ips FROM ports WHERE id::text = $1 AND " + ownerOrAdmin("project_id", "$2", "$3")
	if forUpdate {
		query += " FOR UPDATE"
	}
	p := floatingPort{id: portID}
	var fixed common.FixedIPs
	err := q.QueryRow(ctx, query, portID, projectID, admin).Scan(&p.networkID, &fixed)
	if errors.Is(err, database.ErrNoRows) {
		return p, common.NewNotFoundError("port")
	}
	if err != nil {
		return p, fmt.Errorf("read port %s: %w", portID, err)
	}
	if requested != "" {
		a, err := netip.ParseAddr(strings.TrimSpace(requested))
		if err != nil || !a.Unmap().Is4() {
			return p, badFloatingIPRequest(fmt.Sprintf(
				"Cannot process floating IP association with %s, since that is not an IPv4 address", requested))
		}
		for _, entry := range fixed {
			if e, err := netip.ParseAddr(entry.IPAddress); err == nil && e.Unmap() == a.Unmap() {
				p.fixedIP, p.subnetID = a.Unmap().String(), entry.SubnetID
				return p, nil
			}
		}
		return p, badFloatingIPRequest(fmt.Sprintf("Port %s does not have fixed ip %s", portID, requested))
	}
	var ipv4 common.FixedIPs
	for _, entry := range fixed {
		if a, err := netip.ParseAddr(entry.IPAddress); err == nil && a.Unmap().Is4() {
			ipv4 = append(ipv4, entry)
		}
	}
	switch len(ipv4) {
	case 0:
		return p, badFloatingIPRequest(fmt.Sprintf("Cannot add floating IP to port %s that has no fixed IPv4 addresses", portID))
	case 1:
		p.fixedIP, p.subnetID = ipv4[0].IPAddress, ipv4[0].SubnetID
		return p, nil
	}
	return p, badFloatingIPRequest(fmt.Sprintf(
		"Port %s has multiple fixed IPv4 addresses.  Must provide a specific IPv4 address when assigning a floating IP", portID))
}

// routerForFloatingIP is upstream's get_router_for_floatingip: the routers with an interface port on
// the fixed IP's subnet and a gateway port on the floating network; of them, the one whose interface
// address is the subnet's gateway, else the first. None is 404 ExternalGatewayForFloatingIPNotFound
// (FR-028). The chosen router is locked, so its gateway and its interface on the subnet stay until the
// floating IP's transaction commits: removing either checks for floating IPs under the same lock
// (FR-039). Interfaces are read from router_interfaces and the gateway as gatewayPortsOf reads it:
// what the router code writes, never a member's port claiming either (branch review SRC-2).
func routerForFloatingIP(ctx context.Context, tx database.Tx, port floatingPort, floatingNetwork string) (string, error) {
	var gateway *string
	if err := tx.QueryRow(ctx, "SELECT gateway_ip FROM subnets WHERE id::text = $1", port.subnetID).Scan(&gateway); err != nil {
		return "", fmt.Errorf("read subnet %s: %w", port.subnetID, err)
	}
	if gateway == nil || *gateway == "" {
		return "", badFloatingIPRequest(fmt.Sprintf(
			"Cannot add floating IP to port on subnet %s which has no gateway_ip", port.subnetID))
	}
	rows, err := tx.Query(ctx, `
		SELECT ri.router_id::text, i.fixed_ips
		  FROM router_interfaces ri
		  JOIN ports i ON i.id = ri.port_id
		  JOIN ports g ON g.device_id = ri.router_id AND g.device_owner = 'network:router_gateway' AND g.project_id IS NULL
		 WHERE i.network_id::text = $1 AND g.network_id::text = $2
		 ORDER BY i.created_at, i.id`, port.networkID, floatingNetwork)
	if err != nil {
		return "", fmt.Errorf("find the routers of subnet %s: %w", port.subnetID, err)
	}
	first, chosen := "", ""
	for rows.Next() {
		var router string
		var fixed common.FixedIPs
		if err := rows.Scan(&router, &fixed); err != nil {
			rows.Close()
			return "", fmt.Errorf("read a router interface: %w", err)
		}
		for _, entry := range fixed {
			if entry.SubnetID != port.subnetID {
				continue
			}
			if first == "" {
				first = router
			}
			if chosen == "" && entry.IPAddress == *gateway {
				chosen = router
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("find the routers of subnet %s: %w", port.subnetID, err)
	}
	if chosen == "" {
		chosen = first
	}
	notFound := common.NewNeutronError(http.StatusNotFound, "ExternalGatewayForFloatingIPNotFound",
		fmt.Sprintf("External network %s is not reachable from subnet %s.  Therefore, cannot associate Port %s with a Floating IP.",
			floatingNetwork, port.subnetID, port.id))
	if chosen == "" {
		return "", notFound
	}
	var locked string
	err = tx.QueryRow(ctx, "SELECT id::text FROM routers WHERE id::text = $1 FOR UPDATE", chosen).Scan(&locked)
	if errors.Is(err, database.ErrNoRows) {
		return "", notFound // deleted since it was found
	}
	if err != nil {
		return "", fmt.Errorf("lock router %s: %w", chosen, err)
	}
	return locked, nil
}

// requireOneFloatingIPPerFixedIP refuses a second floating IP of the network on one fixed IP (upstream's
// FloatingIPPortAlreadyAssociated, FR-029). self is the floating IP being associated, "" on create. The
// caller holds the port's lock, so two associations of one fixed IP serialise on it.
func requireOneFloatingIPPerFixedIP(ctx context.Context, tx database.Tx, network string, port floatingPort, self, selfAddress string) error {
	var otherID, otherAddress string
	err := tx.QueryRow(ctx, `
		SELECT id::text, floating_ip_address FROM floating_ips
		 WHERE floating_network_id::text = $1 AND port_id::text = $2 AND fixed_ip_address = $3 AND id::text <> $4
		 LIMIT 1`, network, port.id, port.fixedIP, self).Scan(&otherID, &otherAddress)
	if errors.Is(err, database.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the floating IPs of port %s: %w", port.id, err)
	}
	if self == "" {
		self, selfAddress = otherID, otherAddress
	}
	return common.NewNeutronError(http.StatusConflict, "FloatingIPPortAlreadyAssociated",
		fmt.Sprintf("Cannot associate floating IP %s (%s) with port %s using fixed IP %s, as that fixed IP already has a floating IP on external network %s.",
			selfAddress, self, port.id, port.fixedIP, network))
}

// floatingAddresses is every floating address of every network: floating_ips.floating_ip_address is
// unique across networks.
func floatingAddresses(ctx context.Context, tx database.Tx) (map[netip.Addr]bool, error) {
	addresses, err := queryStrings(ctx, tx, "SELECT floating_ip_address FROM floating_ips")
	if err != nil {
		return nil, fmt.Errorf("read the floating addresses: %w", err)
	}
	out := make(map[netip.Addr]bool, len(addresses))
	for _, a := range addresses {
		if addr, err := netip.ParseAddr(a); err == nil {
			out[addr.Unmap()] = true
		}
	}
	return out, nil
}

// allocateFloatingAddress chooses a floating IP's address in the caller's transaction, which holds the
// allocation lock: the requested floating_ip_address, checked as upstream's IPAM checks it; else the
// lowest free address of the pools of the network's IPv4 subnets, in creation order, or of the
// requested subnet_id's (FR-031, FR-054). Each subnet is locked before its addresses are read (FR-032).
func allocateFloatingAddress(ctx context.Context, tx database.Tx, networkID, subnetID, requested string) (string, error) {
	all, err := externalSubnets(ctx, tx, networkID)
	if err != nil {
		return "", err
	}
	var subnets []externalSubnet
	for _, s := range all {
		if s.prefix.Addr().Is4() {
			subnets = append(subnets, s)
		}
	}
	if len(subnets) == 0 {
		return "", badFloatingIPRequest(fmt.Sprintf("Network %s does not contain any IPv4 subnet", networkID))
	}
	if subnetID != "" {
		var only []externalSubnet
		for _, s := range subnets {
			if s.id == subnetID {
				only = append(only, s)
			}
		}
		if len(only) == 0 {
			return "", invalidInput(fmt.Sprintf(
				"Failed to create port on network %s, because fixed_ips included invalid subnet %s", networkID, subnetID))
		}
		subnets = only
	}
	floating, err := floatingAddresses(ctx, tx)
	if err != nil {
		return "", err
	}
	if requested != "" {
		return requestedFloatingAddress(ctx, tx, subnets, requested, floating)
	}
	for _, s := range subnets {
		if _, err := LockSubnet(ctx, tx, s.id); err != nil {
			return "", err
		}
		ip, err := allocateAddressExcluding(ctx, tx, s.id, pickLowest, floating)
		if errors.Is(err, ErrNoAvailableIP) {
			continue
		}
		return ip, err
	}
	return "", common.NewNeutronError(http.StatusBadRequest, "ExternalIpAddressExhausted",
		fmt.Sprintf("Unable to find any IP address on external network %s.", networkID))
}

// requestedFloatingAddress checks a requested floating_ip_address: on one of the subnets
// (InvalidIpForNetwork), a host address of it (InvalidIpForSubnet), and held by nothing
// (IpAddressAlreadyAllocated).
func requestedFloatingAddress(ctx context.Context, tx database.Tx, subnets []externalSubnet, requested string, floating map[netip.Addr]bool) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(requested))
	var subnet *externalSubnet
	if err == nil {
		a = a.Unmap()
		for i := range subnets {
			if subnets[i].prefix.Contains(a) {
				subnet = &subnets[i]
				break
			}
		}
	}
	if subnet == nil {
		return "", common.NewNeutronError(http.StatusBadRequest, "InvalidIpForNetwork",
			fmt.Sprintf("IP address %s is not a valid IP for any of the subnets on the specified network.", requested))
	}
	if !checkSubnetIP(subnet.prefix, a, false) {
		return "", common.NewNeutronError(http.StatusBadRequest, "InvalidIpForSubnet", (&InvalidIPForSubnetError{IP: requested}).Error())
	}
	if _, err := LockSubnet(ctx, tx, subnet.id); err != nil {
		return "", err
	}
	s, err := readSubnet(ctx, tx, subnet.id)
	if err != nil {
		return "", err
	}
	used, err := usedAddresses(ctx, tx, s)
	if err != nil {
		return "", err
	}
	if used[a] || floating[a] {
		return "", common.NewNeutronError(http.StatusConflict, "IpAddressAlreadyAllocated",
			fmt.Sprintf("IP address %s already allocated in subnet %s", a, subnet.id))
	}
	return a.String(), nil
}
