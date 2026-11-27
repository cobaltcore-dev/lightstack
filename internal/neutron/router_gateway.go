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

// address is the entry's requested address, when it names one that parses.
func (r requestedFixedIP) address() (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(r.ipAddress))
	return a.Unmap(), err == nil
}

// names reports whether the entry names subnet s: by its id, or by an address inside it.
func (r requestedFixedIP) names(s networkSubnet) bool {
	if r.subnetID != "" {
		return r.subnetID == s.id
	}
	a, ok := r.address()
	return ok && s.prefix.Contains(a)
}

// duplicateAddress refuses requested fixed IPs that name one address twice, as upstream's fixed_ips
// validator, under the attribute that carries them (SPEC-033 FR-032, T254).
func duplicateAddress(attribute string, requested []requestedFixedIP) *common.OpenStackError {
	seen := map[netip.Addr]bool{}
	for _, r := range requested {
		if a, ok := r.address(); ok {
			if seen[a] {
				return invalidAttribute(attribute, fmt.Sprintf("Duplicate IP address '%s'", r.ipAddress))
			}
			seen[a] = true
		}
	}
	return nil
}

// stored is what routers.external_gateway_info keeps.
func (g gatewayRequest) stored() map[string]any {
	return map[string]any{"network_id": g.networkID, "enable_snat": g.enableSNAT}
}

// parseGatewayRequest reads a gateway gatewayNetworkID has checked. external_fixed_ips is a list of
// {subnet_id, ip_address} objects, each naming at least one. Requesting them, and setting enable_snat,
// is an administrator's, whatever the value, as upstream's policy matches on the key being given
// (FR-038; SPEC-033 T183).
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
	return req, duplicateAddress("external_gateway_info", req.fixedIPs)
}

// subnetGatewayRouters reads the routers whose gateway port has an address on the subnet: the subnet's
// gateway is their default route (SPEC-033 T189). A gateway port is what the router code writes
// (gatewayPortWhere).
func subnetGatewayRouters(ctx context.Context, q subnetQuerier, networkID, subnetID string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT device_id::text, fixed_ips FROM ports
		 WHERE network_id::text = $1 AND `+gatewayPortWhere("")+`
		 ORDER BY device_id`, networkID)
	if err != nil {
		return nil, fmt.Errorf("read the gateway ports of network %s: %w", networkID, err)
	}
	defer rows.Close()
	var routers []string
	for rows.Next() {
		var router string
		var fixed common.FixedIPs
		if err := rows.Scan(&router, &fixed); err != nil {
			return nil, fmt.Errorf("read a gateway port of network %s: %w", networkID, err)
		}
		if slices.ContainsFunc(fixed, func(f common.FixedIP) bool { return f.SubnetID == subnetID }) {
			routers = append(routers, router)
		}
	}
	return routers, rows.Err()
}

// gatewayPort is a router's gateway port as stored.
type gatewayPort struct {
	id, networkID string
	fixedIPs      common.FixedIPs
}

// gatewayPortWhere is what makes a port a router's gateway port, for the columns of alias ("" or "g."):
// one only the router code writes — the owner and no project; the caller names the router as device. Any
// member may give a port of their own that owner and a router as device, and one taken for a gateway would
// have the network node answer for its address on the uplink. Every query meaning a router's gateway port
// asks this (SPEC-033 T170, T317).
func gatewayPortWhere(alias string) string {
	return alias + "device_owner = '" + ownerRouterGateway + "' AND " + alias + "project_id IS NULL"
}

// gatewayPortsOf reads the gateway ports of the given routers, keyed by router (gatewayPortWhere).
func gatewayPortsOf(ctx context.Context, q subnetQuerier, routerIDs []string) (map[string]gatewayPort, error) {
	out := map[string]gatewayPort{}
	if len(routerIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT device_id::text, id::text, network_id::text, fixed_ips FROM ports
		 WHERE `+gatewayPortWhere("")+` AND device_id::text = ANY($1)`, routerIDs)
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
		// No list, or an empty one, asks for no change, as upstream's _check_for_external_ip_change
		// (SPEC-033 T255).
		if len(req.fixedIPs) == 0 {
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
	if err := lockExternalNetwork(ctx, tx, req.networkID, "a router gateway"); err != nil {
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

// requireNoGatewayPorts refuses to make a network non-external while a router's gateway port is on it, as
// upstream's _process_l3_update (409 ExternalNetworkInUse). The network's lock serialises it with a gateway
// port's creation, which takes that lock and checks the flag again (lockExternalNetwork; SPEC-033 FR-070,
// T257).
func requireNoGatewayPorts(ctx context.Context, tx database.Tx, networkID string) error {
	var locked string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM networks WHERE id::text = $1 FOR UPDATE", networkID).Scan(&locked); err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return common.NewNotFoundError("network")
		}
		return fmt.Errorf("lock network %s: %w", networkID, err)
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ports WHERE network_id::text = $1 AND `+gatewayPortWhere("")+`)`,
		networkID).Scan(&used); err != nil {
		return fmt.Errorf("read the gateway ports of network %s: %w", networkID, err)
	}
	if used {
		return common.NewNeutronError(http.StatusConflict, "ExternalNetworkInUse", fmt.Sprintf(
			"External network %s cannot be updated to be made non-external, since it has existing gateway ports.", networkID))
	}
	return nil
}

// lockExternalNetwork locks the external network a gateway port or a floating IP is made on, after its
// subnets — the order every port insert takes them in, the network's foreign key last — and refuses it when
// it is no longer external: made non-external since the request checked it (requireNoGatewayPorts).
func lockExternalNetwork(ctx context.Context, tx database.Tx, networkID, use string) error {
	var external bool
	if err := tx.QueryRow(ctx, "SELECT external FROM networks WHERE id::text = $1 FOR UPDATE", networkID).Scan(&external); err != nil {
		return fmt.Errorf("lock network %s: %w", networkID, err)
	}
	if !external {
		return errNotExternal(networkID, use)
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

// requireInterfaceUnused refuses to remove a router's interface on a subnet a floating IP of the router
// reaches: its fixed IP, or one of its port forwardings' internal address, inside the subnet and held by
// a port of the subnet's network. That is upstream's RouterInterfaceInUseByFloatingIP, whose words name
// floating IPs for both (FR-039; SPEC-033 T184).
func requireInterfaceUnused(ctx context.Context, tx database.Tx, routerID string, subnet interfaceSubnet) error {
	prefix, err := parsePrefix(subnet.cidr)
	if err != nil {
		return err
	}
	reached, err := queryStrings(ctx, tx, `
		SELECT f.fixed_ip_address FROM floating_ips f JOIN ports p ON p.id = f.port_id
		 WHERE f.router_id::text = $1 AND p.network_id::text = $2 AND f.fixed_ip_address IS NOT NULL
		UNION ALL
		SELECT pf.internal_ip_address FROM port_forwardings pf
		  JOIN floating_ips f ON f.id = pf.floatingip_id JOIN ports p ON p.id = pf.internal_port_id
		 WHERE f.router_id::text = $1 AND p.network_id::text = $2`, routerID, subnet.networkID)
	if err != nil {
		return fmt.Errorf("read what the floating IPs of router %s reach: %w", routerID, err)
	}
	for _, ip := range reached {
		if a, err := netip.ParseAddr(ip); err == nil && prefix.Contains(a.Unmap()) {
			return common.NewNeutronError(http.StatusConflict, "RouterInterfaceInUseByFloatingIP",
				fmt.Sprintf("Router interface for subnet %s on router %s cannot be deleted, as it is required by one or more floating IPs.",
					subnet.id, routerID))
		}
	}
	return nil
}

// networkSubnet is one subnet of an external network as gateway addressing needs it.
type networkSubnet struct {
	id      string
	prefix  netip.Prefix
	gateway netip.Addr
}

func networkSubnets(ctx context.Context, tx database.Tx, networkID string) ([]networkSubnet, error) {
	rows, err := tx.Query(ctx,
		"SELECT id::text, cidr, gateway_ip FROM subnets WHERE network_id::text = $1 ORDER BY created_at, id", networkID)
	if err != nil {
		return nil, fmt.Errorf("read the subnets of network %s: %w", networkID, err)
	}
	defer rows.Close()
	var out []networkSubnet
	for rows.Next() {
		var s networkSubnet
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
	subnets, err := networkSubnets(ctx, tx, networkID)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		if !slices.ContainsFunc(subnets, func(s networkSubnet) bool { return s.prefix.Addr().Is4() }) {
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
	return requestedAddresses(ctx, tx, addressRequest{networkID: networkID, subnets: subnets, held: held, routerPort: true}, requested)
}

// addressRequest is what a port's requested fixed IPs are resolved against: its network's subnets, the
// addresses it holds already — re-requesting one is no conflict, and an entry naming only a subnet keeps
// the one it holds there — and whether it is a router's port, which may take an IPv6 subnet's anycast
// address.
type addressRequest struct {
	networkID  string
	subnets    []networkSubnet
	held       common.FixedIPs
	routerPort bool
}

// requestedAddresses resolves a port's requested fixed IPs on its network, each as resolve does, in the
// order requested, each address once: an entry naming only a subnet never takes an address another entry
// names or an earlier one took (SPEC-033 FR-032, T254). The subnets they name are locked first, in
// subnets' order — creation order, as every other allocation takes them — so two requests naming them in
// opposite orders cannot deadlock (T206).
func requestedAddresses(ctx context.Context, tx database.Tx, a addressRequest, requested []requestedFixedIP) (common.FixedIPs, error) {
	chosen := map[netip.Addr]bool{}
	for _, r := range requested {
		if addr, ok := r.address(); ok {
			chosen[addr] = true
		}
	}
	for _, s := range a.subnets {
		if slices.ContainsFunc(requested, func(r requestedFixedIP) bool { return r.names(s) }) {
			if _, err := LockSubnet(ctx, tx, s.id); err != nil {
				return nil, err
			}
		}
	}
	out := common.FixedIPs{}
	for _, r := range requested {
		entry, err := a.resolve(ctx, tx, r, chosen)
		if err != nil {
			return nil, err
		}
		if addr, err := netip.ParseAddr(entry.IPAddress); err == nil {
			chosen[addr] = true
		}
		out = append(out, entry)
	}
	return out, nil
}

// resolve checks one requested entry of a port's fixed IPs, as upstream's IPAM does: an address on one of
// the network's subnets (InvalidIpForNetwork otherwise), checked by refusal; it need not lie in a pool
// (FR-032). An entry naming only a subnet keeps the address the port holds there, as upstream's
// _get_changed_ips_for_port, unless chosen has it; otherwise it takes a free address of the subnet's pools
// that chosen does not have.
func (a addressRequest) resolve(ctx context.Context, tx database.Tx, r requestedFixedIP, chosen map[netip.Addr]bool) (common.FixedIP, error) {
	subnet, addr, refusal := a.subnetOf(r)
	if refusal != nil {
		return common.FixedIP{}, refusal
	}
	if _, err := LockSubnet(ctx, tx, subnet.id); err != nil {
		return common.FixedIP{}, err
	}
	if addr.IsValid() {
		s, err := readSubnet(ctx, tx, subnet.id)
		if err != nil {
			return common.FixedIP{}, err
		}
		if err := a.refusal(ctx, tx, s, addr, r.ipAddress); err != nil {
			return common.FixedIP{}, err
		}
		return common.FixedIP{SubnetID: subnet.id, IPAddress: addr.String()}, nil
	}
	for _, f := range a.held {
		if held, err := netip.ParseAddr(f.IPAddress); err == nil && f.SubnetID == subnet.id && !chosen[held] {
			return f, nil
		}
	}
	ip, err := allocateAddressExcluding(ctx, tx, subnet.id, pickRandom, chosen)
	if errors.Is(err, ErrNoAvailableIP) {
		return common.FixedIP{}, common.NewNeutronError(http.StatusConflict, "IpAddressGenerationFailure",
			fmt.Sprintf("No more IP addresses available for subnet %s.", subnet.id))
	}
	return common.FixedIP{SubnetID: subnet.id, IPAddress: ip}, err
}

// subnetOf finds the subnet an entry names — by its address, or by its id alone — and the address, zero
// for an entry naming only a subnet.
func (a addressRequest) subnetOf(r requestedFixedIP) (networkSubnet, netip.Addr, *common.OpenStackError) {
	if r.ipAddress == "" {
		for _, s := range a.subnets {
			if s.id == r.subnetID {
				return s, netip.Addr{}, nil
			}
		}
		return networkSubnet{}, netip.Addr{}, invalidInput(fmt.Sprintf(
			"Failed to create port on network %s, because fixed_ips included invalid subnet %s", a.networkID, r.subnetID))
	}
	if addr, ok := r.address(); ok {
		for _, s := range a.subnets {
			if s.prefix.Contains(addr) && (r.subnetID == "" || s.id == r.subnetID) {
				return s, addr, nil
			}
		}
	}
	return networkSubnet{}, netip.Addr{}, common.NewNeutronError(http.StatusBadRequest, "InvalidIpForNetwork",
		fmt.Sprintf("IP address %s is not a valid IP for any of the subnets on the specified network.", r.ipAddress))
}
