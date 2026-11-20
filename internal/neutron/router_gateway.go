package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
)

// A router's gateway is a port of the external network, as upstream's is (SPEC-033 round 2: FR-038,
// FR-039; research R31): device_owner network:router_gateway, no project, holding an address of the
// network — the one an administrator requests in external_gateway_info.external_fixed_ips, or one
// from the pools of its IPv4 subnets. The router reports the port's addresses as its
// external_fixed_ips, and its subnets leave through the port's address (the router namespace, R28).
// routers.external_gateway_info stores the network and enable_snat; the addresses live on the port.

const (
	ownerRouterGateway   = "network:router_gateway"
	ownerRouterInterface = "network:router_interface"
	ownerDHCP            = "network:dhcp"
)

// gatewayRequest is external_gateway_info as a create or an update gives it.
type gatewayRequest struct {
	networkID  string
	enableSNAT bool
	fixedIPs   []requestedFixedIP // nil when external_fixed_ips was not given
}

type requestedFixedIP struct{ subnetID, ipAddress string }

// stored is what routers.external_gateway_info keeps.
func (g gatewayRequest) stored() map[string]any {
	return map[string]any{"network_id": g.networkID, "enable_snat": g.enableSNAT}
}

// parseGatewayRequest reads a gateway gatewayNetworkID has checked. external_fixed_ips is a list of
// {subnet_id, ip_address} objects, each naming at least one. Requesting them, and setting enable_snat,
// is an administrator's, whatever the value, as upstream's policy matches on the key being given
// (FR-038; branch review SRC-11).
func parseGatewayRequest(gateway map[string]any, admin bool, rule string) (gatewayRequest, *common.OpenStackError) {
	req := gatewayRequest{enableSNAT: gatewaySNATOn(gateway)}
	req.networkID, _ = gateway["network_id"].(string)
	if !admin {
		for _, key := range []string{"external_fixed_ips", "enable_snat"} {
			if value, given := gateway[key]; given && value != nil {
				return req, common.NewNeutronError(http.StatusForbidden, "PolicyNotAuthorized",
					fmt.Sprintf("(rule:%s:external_gateway_info:%s) is disallowed by policy", rule, key))
			}
		}
	}
	raw, given := gateway["external_fixed_ips"]
	if !given || raw == nil {
		return req, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return req, invalidInput("external_fixed_ips must be a list")
	}
	req.fixedIPs = []requestedFixedIP{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return req, invalidInput("each external_fixed_ips entry must be an object")
		}
		subnet, _ := entry["subnet_id"].(string)
		ip, _ := entry["ip_address"].(string)
		if subnet == "" && ip == "" {
			return req, invalidInput("an external_fixed_ips entry needs a subnet_id or an ip_address")
		}
		req.fixedIPs = append(req.fixedIPs, requestedFixedIP{subnetID: subnet, ipAddress: ip})
	}
	return req, nil
}

// gatewayPort is a router's gateway port as stored.
type gatewayPort struct {
	id, networkID string
	fixedIPs      common.FixedIPs
}

// gatewayPortsOf reads the gateway ports of the given routers, keyed by router. A gateway port is one
// only the router code writes: the owner, the router as device, and no project. Any member may give a
// port of their own that owner and device, and one taken for the gateway would have the network node
// answer for its address on the uplink (branch review SRC-2).
func gatewayPortsOf(ctx context.Context, q subnetQuerier, routerIDs []string) (map[string]gatewayPort, error) {
	out := map[string]gatewayPort{}
	if len(routerIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT device_id::text, id::text, network_id::text, fixed_ips FROM ports
		 WHERE device_owner = 'network:router_gateway' AND project_id IS NULL AND device_id::text = ANY($1)`, routerIDs)
	if err != nil {
		return nil, fmt.Errorf("reading the routers' gateway ports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var router string
		var p gatewayPort
		if err := rows.Scan(&router, &p.id, &p.networkID, &p.fixedIPs); err != nil {
			return nil, fmt.Errorf("reading a gateway port: %w", err)
		}
		out[router] = p
	}
	return out, rows.Err()
}

// renderGatewayInfo is a router's external_gateway_info as upstream reports it: the stored network and
// enable_snat, and the gateway port's addresses. nil when the router has no gateway.
func renderGatewayInfo(stored map[string]any, port *gatewayPort) map[string]any {
	if stored == nil {
		return nil
	}
	fixed := []map[string]string{}
	if port != nil {
		for _, ip := range port.fixedIPs {
			fixed = append(fixed, map[string]string{"subnet_id": ip.SubnetID, "ip_address": ip.IPAddress})
		}
	}
	return map[string]any{
		"network_id":         stored["network_id"],
		"enable_snat":        gatewaySNATOn(stored),
		"external_fixed_ips": fixed,
	}
}

// setRouterGateway brings a router's gateway port in line with req — nil clears it — in the caller's
// transaction, which holds the router's row lock. A port on another network goes and a new one is made;
// on the same network, requested addresses that differ move the port, as upstream's
// _update_current_gw_port; otherwise the port stays. Removing a gateway port a floating IP of the router
// needs answers 409 (FR-039).
func setRouterGateway(ctx context.Context, tx database.Tx, routerID string, req *gatewayRequest) error {
	current, err := gatewayPortsOf(ctx, tx, []string{routerID})
	if err != nil {
		return err
	}
	port, has := current[routerID]
	if has && (req == nil || port.networkID != req.networkID) {
		if err := requireGatewayUnused(ctx, tx, routerID, port.networkID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM ports WHERE id = $1", port.id); err != nil {
			return fmt.Errorf("delete the gateway port of router %s: %w", routerID, err)
		}
		has = false
	}
	if req == nil {
		return nil
	}
	if has {
		if req.fixedIPs == nil {
			return nil
		}
		fixed, err := gatewayAddresses(ctx, tx, req.networkID, req.fixedIPs, port.fixedIPs)
		if err != nil {
			return err
		}
		if slices.Equal(fixed, port.fixedIPs) {
			return nil
		}
		raw, _ := json.Marshal(fixed)
		_, err = tx.Exec(ctx, "UPDATE ports SET fixed_ips = $1, updated_at = $2 WHERE id = $3", raw, time.Now(), port.id)
		return err
	}
	fixed, err := gatewayAddresses(ctx, tx, req.networkID, req.fixedIPs, nil)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(fixed)
	now := time.Now()
	_, err = tx.Exec(ctx, `
		INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		VALUES ($1, '', $2, NULL, $3, $4, $5, true, 'ACTIVE', $6, $7, $8)`,
		uuid.NewString(), req.networkID, routerID, ownerRouterGateway, generateMAC(), raw, now, now)
	if err != nil {
		return fmt.Errorf("create the gateway port of router %s: %w", routerID, err)
	}
	return nil
}

// requireGatewayUnused refuses to remove a router's gateway while a floating IP goes through the router
// (upstream's RouterExternalGatewayInUseByFloatingIp, FR-039).
func requireGatewayUnused(ctx context.Context, tx database.Tx, routerID, networkID string) error {
	var used bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM floating_ips WHERE router_id::text = $1)", routerID).Scan(&used); err != nil {
		return fmt.Errorf("read the floating IPs of router %s: %w", routerID, err)
	}
	if used {
		return common.NewNeutronError(http.StatusConflict, "RouterExternalGatewayInUseByFloatingIp",
			fmt.Sprintf("Gateway cannot be updated for router %s, since a gateway to external network %s is required by one or more floating IPs.",
				routerID, networkID))
	}
	return nil
}

// requireInterfaceUnused refuses to remove a router's interface on a subnet whose fixed IPs a floating
// IP of the router reaches (upstream's RouterInterfaceInUseByFloatingIP, FR-039).
func requireInterfaceUnused(ctx context.Context, tx database.Tx, routerID, subnetID, cidr string) error {
	prefix, err := parsePrefix(cidr)
	if err != nil {
		return err
	}
	fixed, err := queryStrings(ctx, tx,
		"SELECT fixed_ip_address FROM floating_ips WHERE router_id::text = $1 AND fixed_ip_address IS NOT NULL", routerID)
	if err != nil {
		return fmt.Errorf("read the floating IPs of router %s: %w", routerID, err)
	}
	for _, ip := range fixed {
		if a, err := netip.ParseAddr(ip); err == nil && prefix.Contains(a.Unmap()) {
			return common.NewNeutronError(http.StatusConflict, "RouterInterfaceInUseByFloatingIP",
				fmt.Sprintf("Router interface for subnet %s on router %s cannot be deleted, as it is required by one or more floating IPs.",
					subnetID, routerID))
		}
	}
	return nil
}

// externalSubnet is one subnet of an external network as gateway addressing needs it.
type externalSubnet struct {
	id      string
	prefix  netip.Prefix
	gateway netip.Addr
}

func externalSubnets(ctx context.Context, tx database.Tx, networkID string) ([]externalSubnet, error) {
	rows, err := tx.Query(ctx,
		"SELECT id::text, cidr, gateway_ip FROM subnets WHERE network_id::text = $1 ORDER BY created_at, id", networkID)
	if err != nil {
		return nil, fmt.Errorf("read the subnets of network %s: %w", networkID, err)
	}
	defer rows.Close()
	var out []externalSubnet
	for rows.Next() {
		var s externalSubnet
		var cidr string
		var gateway *string
		if err := rows.Scan(&s.id, &cidr, &gateway); err != nil {
			return nil, fmt.Errorf("read a subnet of network %s: %w", networkID, err)
		}
		if s.prefix, err = parsePrefix(cidr); err != nil {
			continue
		}
		s.gateway = parseGateway(gateway)
		out = append(out, s)
	}
	return out, rows.Err()
}

// gatewayAddresses resolves a gateway's addresses on its network: each requested one checked as
// upstream checks it, or, when none is requested, one address from the pools of the network's first
// IPv4 subnet that has a free one, at random. A network with no IPv4 subnet gives the port none, as
// upstream's _create_router_gw_port allows; subnets that are all exhausted answer 409
// IpAddressGenerationFailure, as upstream's IPAM does. held is what the port holds now: re-requesting
// it is not a conflict. Every subnet is locked before its addresses are read (FR-032).
func gatewayAddresses(ctx context.Context, tx database.Tx, networkID string, requested []requestedFixedIP, held common.FixedIPs) (common.FixedIPs, error) {
	subnets, err := externalSubnets(ctx, tx, networkID)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		if !slices.ContainsFunc(subnets, func(s externalSubnet) bool { return s.prefix.Addr().Is4() }) {
			return common.FixedIPs{}, nil
		}
		for _, s := range subnets {
			if !s.prefix.Addr().Is4() {
				continue
			}
			if _, err := LockSubnet(ctx, tx, s.id); err != nil {
				return nil, err
			}
			ip, err := allocateAddress(ctx, tx, s.id, pickRandom)
			if errors.Is(err, ErrNoAvailableIP) {
				continue
			}
			if err != nil {
				return nil, err
			}
			return common.FixedIPs{{SubnetID: s.id, IPAddress: ip}}, nil
		}
		return nil, common.NewNeutronError(http.StatusConflict, "IpAddressGenerationFailure",
			fmt.Sprintf("No more IP addresses available on network %s.", networkID))
	}
	var out common.FixedIPs
	for _, r := range requested {
		entry, err := requestedGatewayAddress(ctx, tx, networkID, subnets, r, held)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// requestedGatewayAddress checks one requested entry: an address on one of the network's subnets
// (InvalidIpForNetwork otherwise), a host address of it (InvalidIpForSubnet), not its gateway, and held
// by nothing else (IpAddressAlreadyAllocated). An entry naming only a subnet takes a free address of its
// pools.
func requestedGatewayAddress(ctx context.Context, tx database.Tx, networkID string, subnets []externalSubnet, r requestedFixedIP, held common.FixedIPs) (common.FixedIP, error) {
	var subnet *externalSubnet
	var addr netip.Addr
	if r.ipAddress != "" {
		a, err := netip.ParseAddr(strings.TrimSpace(r.ipAddress))
		if err == nil {
			addr = a.Unmap()
			for i := range subnets {
				if subnets[i].prefix.Contains(addr) && (r.subnetID == "" || subnets[i].id == r.subnetID) {
					subnet = &subnets[i]
					break
				}
			}
		}
		if subnet == nil {
			return common.FixedIP{}, common.NewNeutronError(http.StatusBadRequest, "InvalidIpForNetwork",
				fmt.Sprintf("IP address %s is not a valid IP for any of the subnets on the specified network.", r.ipAddress))
		}
	} else {
		for i := range subnets {
			if subnets[i].id == r.subnetID {
				subnet = &subnets[i]
			}
		}
		if subnet == nil {
			return common.FixedIP{}, invalidInput(fmt.Sprintf(
				"Failed to create port on network %s, because fixed_ips included invalid subnet %s", networkID, r.subnetID))
		}
	}
	if _, err := LockSubnet(ctx, tx, subnet.id); err != nil {
		return common.FixedIP{}, err
	}
	if !addr.IsValid() {
		ip, err := allocateAddress(ctx, tx, subnet.id, pickRandom)
		if errors.Is(err, ErrNoAvailableIP) {
			return common.FixedIP{}, common.NewNeutronError(http.StatusConflict, "IpAddressGenerationFailure",
				fmt.Sprintf("No more IP addresses available for subnet %s.", subnet.id))
		}
		return common.FixedIP{SubnetID: subnet.id, IPAddress: ip}, err
	}
	if !checkSubnetIP(subnet.prefix, addr, true) {
		return common.FixedIP{}, common.NewNeutronError(http.StatusBadRequest, "InvalidIpForSubnet",
			(&InvalidIPForSubnetError{IP: r.ipAddress}).Error())
	}
	if subnet.gateway.IsValid() && addr == subnet.gateway {
		return common.FixedIP{}, invalidInput(fmt.Sprintf("IP address %s is the gateway of subnet %s", addr, subnet.id))
	}
	if !held.Holds(addr.String()) {
		s, err := readSubnet(ctx, tx, subnet.id)
		if err != nil {
			return common.FixedIP{}, err
		}
		used, err := usedAddresses(ctx, tx, s)
		if err != nil {
			return common.FixedIP{}, err
		}
		if used[addr] {
			return common.FixedIP{}, common.NewNeutronError(http.StatusConflict, "IpAddressAlreadyAllocated",
				fmt.Sprintf("IP address %s already allocated in subnet %s", addr, subnet.id))
		}
	}
	return common.FixedIP{SubnetID: subnet.id, IPAddress: addr.String()}, nil
}
