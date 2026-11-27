package neutron

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// interfaceSubnet is the subnet a router interface is added on, as the caller may see it.
type interfaceSubnet struct {
	id, networkID, project, cidr, gateway string
}

// checkInterfaceSubnet makes upstream's checks of an interface added by subnet (_add_interface_by_subnet,
// neutron master), in its order: the subnet has a gateway; a non-administrator's project owns it — a
// network shared with every project gives no other project its subnets, as upstream's LP-1757482 has
// it, and Lightstack shares no network with one project alone; the router has no port on it and none
// of its interfaces' subnets overlaps it (_check_for_dup_router_subnets); and, under the subnet's lock,
// nothing — a port, a floating IP — holds the gateway address, which the interface's port takes as read
// under that lock (SPEC-033 FR-055, T171, T268, T269). It returns that address.
func checkInterfaceSubnet(ctx context.Context, tx database.Tx, a interfaceAdd, s interfaceSubnet) (netip.Addr, error) {
	noGateway := badRequest("router", "Subnet for router interface must have a gateway IP")
	if s.gateway == "" {
		return netip.Addr{}, noGateway
	}
	if !a.admin && s.project != a.caller {
		return netip.Addr{}, badRequest("router", fmt.Sprintf(
			"Cannot add interface to router because subnet %s is not owned by project making the request", s.id))
	}
	if err := checkRouterSubnets(ctx, tx, a.router, s); err != nil {
		return netip.Addr{}, err
	}
	if _, err := LockSubnet(ctx, tx, s.id); err != nil {
		return netip.Addr{}, err
	}
	locked, err := readSubnet(ctx, tx, s.id)
	if err != nil {
		return netip.Addr{}, err
	}
	if !locked.gateway.IsValid() {
		return netip.Addr{}, noGateway
	}
	return locked.gateway, addressRequest{routerPort: true}.refusal(ctx, tx, locked, locked.gateway, locked.gateway.String())
}

// checkRouterSubnets is upstream's _check_for_dup_router_subnets for an interface: no port of the
// router, its gateway's included, has an address on the subnet, and no subnet of its interfaces
// overlaps it. The router's ports are its records — the gateway port the router code writes and the
// router_interfaces rows — never a port that merely names the router.
func checkRouterSubnets(ctx context.Context, tx database.Tx, routerID string, s interfaceSubnet) error {
	alreadyHas := badRequest("router", fmt.Sprintf("Router already has a port on subnet %s", s.id))
	gateways, err := gatewayPortsOf(ctx, tx, []string{routerID})
	if err != nil {
		return err
	}
	for _, f := range gateways[routerID].fixedIPs {
		if f.SubnetID == s.id {
			return alreadyHas
		}
	}
	prefix, err := netip.ParsePrefix(s.cidr)
	if err != nil {
		return fmt.Errorf("subnet %s: cidr %q: %w", s.id, s.cidr, err)
	}
	rows, err := tx.Query(ctx, `
		SELECT sn.id::text, sn.cidr FROM router_interfaces ri JOIN subnets sn ON sn.id = ri.subnet_id
		 WHERE ri.router_id::text = $1 ORDER BY ri.created_at, ri.id`, routerID)
	if err != nil {
		return fmt.Errorf("router %s: read its interfaces' subnets: %w", routerID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, cidr string
		if err := rows.Scan(&id, &cidr); err != nil {
			return fmt.Errorf("router %s: read an interface's subnet: %w", routerID, err)
		}
		if id == s.id {
			return alreadyHas
		}
		if other, err := netip.ParsePrefix(cidr); err == nil && other.Overlaps(prefix) {
			return badRequest("router", fmt.Sprintf(
				"Cidr %s of subnet %s overlaps with cidr %s of subnet %s", s.cidr, s.id, cidr, id))
		}
	}
	return rows.Err()
}
