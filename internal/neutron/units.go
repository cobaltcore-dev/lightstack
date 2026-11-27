package neutron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// A unit is what one node converges as a whole (SPEC-033 research R27–R32): a router, a network's
// DHCP, or the ports bound to a node. The node that holds a unit reads its records, renders them and
// converges its state to them, or to absent before a deletion (R34). A request asks for a unit through
// nodeCaller, which reaches the node that holds it; that node converges it under the unit's lock,
// within the call's deadline.

// unitKind names a kind of unit, as the node call's body does (contracts/node-apply.md).
type unitKind string

const (
	unitRouter unitKind = "router" // a router's namespace and everything in it, on the network node
	unitDHCP   unitKind = "dhcp"   // a network's DHCP namespace, dnsmasq and metadata listener, ditto
	unitPorts  unitKind = "ports"  // the chains of the ports bound to a node, on that node
)

// unitRequest names a unit and its ids, never their state: the node that converges it reads the
// records itself, so a request carries nothing a stale caller could get wrong (R27).
//   - A router or a network's DHCP has exactly one id. Absent converges it as if its records were
//     gone; AbsentIDs names records of it to read as gone — a port forwarding, an interface's port, a
//     subnet of an interface's port that keeps others — so a deletion can take their state away before
//     their records go (R34, T315).
//   - Ports: the node converges every port bound to it from the records. IDs names those the request
//     changed, which must be bound there; AbsentIDs names ports being deleted, read as gone wherever
//     they are bound and from the remote-group expansions (a deletion's first step). A request naming
//     neither converges the node as its records say: the old node of a binding that moved.
type unitRequest struct {
	Unit      unitKind `json:"unit"`
	IDs       []string `json:"ids"`
	Absent    bool     `json:"absent,omitempty"`
	AbsentIDs []string `json:"absent_ids,omitempty"`
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// errBadUnit: the request names no unit this node converges. The call answers 400.
var errBadUnit = errors.New("not a unit")

// errUnitNotHeld: this node does not hold the unit — it is not the network node, for a router or DHCP,
// or a port listed for presence is not bound to it. The call answers 409.
var errUnitNotHeld = errors.New("this node does not hold the unit")

// errUnitBusy: the unit's lock was not acquired within the call's deadline. The call answers 500.
var errUnitBusy = errors.New("the unit is busy")

// validate refuses a request that names no unit: an unknown kind, a router or DHCP without exactly one
// id or absent both whole and in part, an id that is not one, or an id given twice.
func (r unitRequest) validate() error {
	switch r.Unit {
	case unitRouter, unitDHCP:
		if len(r.IDs) != 1 {
			return fmt.Errorf("%w: a %s unit has exactly one id", errBadUnit, r.Unit)
		}
		if r.Absent && len(r.AbsentIDs) > 0 {
			return fmt.Errorf("%w: a %s is absent whole or in part, not both", errBadUnit, r.Unit)
		}
	case unitPorts:
		if r.Absent {
			return fmt.Errorf("%w: the ports unit names the absent ports", errBadUnit)
		}
	default:
		return fmt.Errorf("%w: %q", errBadUnit, r.Unit)
	}
	all := append(slices.Clone(r.IDs), r.AbsentIDs...)
	for i, id := range all {
		if uuid.Validate(id) != nil {
			return fmt.Errorf("%w: %q is not an id", errBadUnit, id)
		}
		if slices.Contains(all[:i], id) {
			return fmt.Errorf("%w: %s is named twice", errBadUnit, id)
		}
	}
	return nil
}

// lockKey names the unit's lock: one per router and one per network's DHCP on the network node, and
// one for the node's ports, which are committed together (data-model.md, "Units, their locks and who
// applies them").
func (r unitRequest) lockKey() string {
	if r.Unit == unitPorts {
		return string(unitPorts)
	}
	return string(r.Unit) + ":" + r.IDs[0]
}

// unitLocks are this node's unit locks, each a one-slot channel so that acquiring it has a deadline:
// the node call's (R27). A request holds at most one at a time, so they need no order among
// themselves. A lock lives while a request holds or waits for it and goes with the last of them, so
// the table holds the units being converged, not every one this node ever converged (branch review
// T219).
type unitLocks struct {
	mu    sync.Mutex
	slots map[string]*unitSlot
}

// unitSlot is a unit's lock, the number of requests holding or waiting for it, and the converges of the
// unit to its records begun under it and the latest of them that succeeded, numbered from one.
type unitSlot struct {
	ch               chan struct{}
	refs             int
	begun, succeeded uint64
}

func (l *unitLocks) acquire(ctx context.Context, key string) (release func(), err error) {
	slot, _, err := l.take(ctx, key)
	if err != nil {
		return nil, err
	}
	return func() { l.give(key, slot) }, nil
}

// converge applies a unit under its lock, within ctx's deadline. A request that only asks for the unit as
// its records hold it is coalescible: it is answered by any converge to the records that began after it
// joined — that converge read the records after the request committed them — and succeeded, so requests
// waiting together share one converge rather than each queueing its own (SPEC-033 T253). A request that
// reads some records as gone always applies itself, and never answers another.
func (l *unitLocks) converge(ctx context.Context, key string, coalescible bool, apply func() error) error {
	slot, joined, err := l.take(ctx, key)
	if err != nil {
		return err
	}
	defer l.give(key, slot)
	if !coalescible {
		return apply()
	}
	n, answered := l.begin(slot, joined)
	if answered {
		return nil
	}
	if err := apply(); err != nil {
		return err
	}
	l.succeed(slot, n)
	return nil
}

// take waits for key's lock until ctx is done, and returns it with the number of converges begun under it
// when the request joined.
func (l *unitLocks) take(ctx context.Context, key string) (*unitSlot, uint64, error) {
	slot, joined := l.join(key)
	select {
	case slot.ch <- struct{}{}:
		return slot, joined, nil
	case <-ctx.Done():
		l.leave(key, slot)
		return nil, 0, fmt.Errorf("%w: %s: %w", errUnitBusy, key, ctx.Err())
	}
}

// give releases key's lock.
func (l *unitLocks) give(key string, slot *unitSlot) {
	<-slot.ch
	l.leave(key, slot)
}

// begin numbers a converge to the records, unless one that began after joined has succeeded already.
func (l *unitLocks) begin(slot *unitSlot, joined uint64) (n uint64, answered bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if slot.succeeded > joined {
		return 0, true
	}
	slot.begun++
	return slot.begun, false
}

// succeed records converge n as succeeded.
func (l *unitLocks) succeed(slot *unitSlot, n uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot.succeeded = max(slot.succeeded, n)
}

// join counts a request in on key's lock, making the lock when no request has it, and returns the number
// of converges begun under it so far.
func (l *unitLocks) join(key string) (*unitSlot, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slots == nil {
		l.slots = map[string]*unitSlot{}
	}
	slot, ok := l.slots[key]
	if !ok {
		slot = &unitSlot{ch: make(chan struct{}, 1)}
		l.slots[key] = slot
	}
	slot.refs++
	return slot, slot.begun
}

// leave counts a request out of key's lock, removing the lock with the last.
func (l *unitLocks) leave(key string, slot *unitSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if slot.refs--; slot.refs == 0 {
		delete(l.slots, key)
	}
}

// nodeCaller asks the node that holds a unit to converge it (R27): a function call when that node is
// this one, the authenticated one-second call otherwise. Any failure is a failed step for the request
// that asked (FR-008, FR-040). httpNodeCaller is the real one.
type nodeCaller interface {
	converge(ctx context.Context, node string, req unitRequest) error
}

// convergeUnit converges a unit this node holds: it refuses a unit held elsewhere, takes the unit's
// lock and converges the unit, all within ctx's deadline, which the node call sets to 900 ms from
// receipt (R27). Requests that ask only for the unit as its records hold it share a converge
// (unitLocks.converge): every caller commits its records before it asks.
func (svc *Service) convergeUnit(ctx context.Context, req unitRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	if err := svc.requireHeld(ctx, req); err != nil {
		return err
	}
	coalescible := !req.Absent && len(req.AbsentIDs) == 0
	return svc.units.converge(ctx, req.lockKey(), coalescible, func() error { return svc.applyUnit(ctx, req) })
}

// requireHeld refuses a unit this node does not hold: a router or DHCP anywhere but on the network
// node, or a port listed for presence that is not bound here. Ports listed as absent are accepted
// whatever their binding, so the old node of a moved or cleared binding can remove their chains.
func (svc *Service) requireHeld(ctx context.Context, req unitRequest) error {
	if req.Unit != unitPorts {
		if !svc.isNetworkNode() {
			return fmt.Errorf("%w: %s %s lives on the network node, %s", errUnitNotHeld, req.Unit, req.IDs[0], svc.networkNodeName())
		}
		return nil
	}
	if len(req.IDs) == 0 {
		return nil
	}
	var bound int
	if err := svc.pool.QueryRow(ctx,
		"SELECT count(*) FROM ports WHERE id::text = ANY($1) AND binding_host_id = $2", req.IDs, svc.nodes.Name,
	).Scan(&bound); err != nil {
		return fmt.Errorf("read the ports' bindings: %w", err)
	}
	if bound != len(req.IDs) {
		return fmt.Errorf("%w: %d of the %d ports are not bound to %s", errUnitNotHeld, len(req.IDs)-bound, len(req.IDs), svc.nodes.Name)
	}
	return nil
}

// applyUnit converges the unit to its records, or to absent.
func (svc *Service) applyUnit(ctx context.Context, req unitRequest) error {
	switch req.Unit {
	case unitRouter:
		return svc.applyRouter(ctx, req.IDs[0], req.Absent, req.AbsentIDs)
	case unitDHCP:
		return svc.applyDHCP(ctx, req.IDs[0], req.Absent, req.AbsentIDs)
	default:
		return svc.applyPorts(ctx, req.AbsentIDs)
	}
}

// routerDataplane converges routers on the network node (R28, R41): *networking.RouterManager, or a
// fake in tests. A nil state converges the router to absent. Its listing and its sweep serve the
// reconciliation of every node.
type routerDataplane interface {
	ConvergeRouter(ctx context.Context, routerID string, state *networking.RouterState, uplink string) error
	// HeldRouters lists the routers whose namespaces the node holds.
	HeldRouters(ctx context.Context) ([]string, error)
	// RemoveStrayProxies removes the uplink's proxy entries no router's leg routes, among the addresses
	// ours recognises as Lightstack's.
	RemoveStrayProxies(ctx context.Context, uplink string, ours func(string) bool) ([]string, error)
}

// applyRouter converges a router on this, the network node: to its records, those named in without
// read as gone, or to absent when absent is set or the router has no record.
func (svc *Service) applyRouter(ctx context.Context, routerID string, absent bool, without []string) error {
	uplink, err := svc.uplink()
	if err != nil {
		return fmt.Errorf("router %s: no uplink: %w", routerID, err)
	}
	var state *networking.RouterState
	if !absent {
		s, found, err := readRouterState(ctx, svc.pool, routerID, without)
		if err != nil {
			return err
		}
		if found {
			state = &s
		}
	}
	if state != nil {
		// The bridges its interfaces join: on this, the network node, they may not be made yet.
		for _, i := range state.Interfaces {
			if err := svc.brManager.CreateBridge(i.Bridge); err != nil {
				return fmt.Errorf("router %s: bridge %s: %w", routerID, i.Bridge, err)
			}
		}
	}
	return svc.routers.ConvergeRouter(ctx, routerID, state, uplink)
}

// convergeRouterUnit converges a router on the network node through the node call: as its records
// say, those named in without read as gone (a deletion's first step, R34).
func (svc *Service) convergeRouterUnit(ctx context.Context, routerID string, without ...string) error {
	return svc.caller.converge(ctx, svc.networkNodeName(),
		unitRequest{Unit: unitRouter, IDs: []string{routerID}, AbsentIDs: without})
}

// removeRouterUnit converges a router to absent on the network node, through the node call.
func (svc *Service) removeRouterUnit(ctx context.Context, routerID string) error {
	return svc.caller.converge(ctx, svc.networkNodeName(),
		unitRequest{Unit: unitRouter, IDs: []string{routerID}, Absent: true})
}

// convergeRouterUnits converges each distinct router of routerIDs, skipping "", and stops at the
// first failure.
func (svc *Service) convergeRouterUnits(ctx context.Context, routerIDs ...string) error {
	done := map[string]bool{"": true}
	for _, id := range routerIDs {
		if done[id] {
			continue
		}
		done[id] = true
		if err := svc.convergeRouterUnit(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// readRouterState reads a router's records as the network node is to hold them (R28): its gateway port
// with its subnets, its interfaces with every fixed IP of their ports (T315), and, with a gateway, its
// floating IPs on a port and its port forwardings. Records named in without — a gateway port, an
// interface's port, a subnet of one, a floating IP, a port forwarding — are read as gone. found is false
// when the router has no record.
func readRouterState(ctx context.Context, q subnetQuerier, routerID string, without []string) (networking.RouterState, bool, error) {
	s := networking.RouterState{ID: routerID}
	var info sql.NullString
	err := q.QueryRow(ctx, "SELECT external_gateway_info FROM routers WHERE id::text = $1", routerID).Scan(&info)
	if errors.Is(err, database.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("router %s: read it: %w", routerID, err)
	}
	gone := func(id string) bool { return slices.Contains(without, id) }

	gateways, err := gatewayPortsOf(ctx, q, []string{routerID})
	if err != nil {
		return s, false, err
	}
	interfaces, err := routerInterfacePorts(ctx, q, routerID)
	if err != nil {
		return s, false, err
	}
	var subnetIDs []string
	for _, p := range append([]gatewayPort{gateways[routerID]}, interfaces...) {
		for _, f := range p.fixedIPs {
			subnetIDs = append(subnetIDs, f.SubnetID)
		}
	}
	subnets, err := subnetsOf(ctx, q, subnetIDs)
	if err != nil {
		return s, false, err
	}
	if port, ok := gateways[routerID]; ok && !gone(port.id) {
		g := &networking.RouterGateway{PortID: port.id, SNAT: gatewaySNATOn(parseStoredGateway(routerID, info))}
		for _, f := range port.fixedIPs {
			if sub, ok := subnets[f.SubnetID]; ok {
				g.Addresses = append(g.Addresses, fmt.Sprintf("%s/%d", f.IPAddress, sub.prefix))
				if g.NextHop == "" && sub.version == 4 && sub.gateway != "" {
					g.NextHop = sub.gateway
				}
			}
		}
		s.Gateway = g
	}
	for _, p := range interfaces {
		if gone(p.id) {
			continue
		}
		leg := networking.RouterInterface{PortID: p.id, Bridge: networking.BridgeNameForNetwork(p.networkID)}
		for _, f := range p.fixedIPs {
			if sub, ok := subnets[f.SubnetID]; ok && !gone(f.SubnetID) {
				leg.Addresses = append(leg.Addresses, fmt.Sprintf("%s/%d", f.IPAddress, sub.prefix))
			}
		}
		if len(leg.Addresses) > 0 {
			s.Interfaces = append(s.Interfaces, leg)
		}
	}
	if s.Gateway == nil {
		return s, true, nil // a floating IP needs the gateway (FR-039)
	}
	if s.FloatingIPs, err = routerFloatingIPs(ctx, q, routerID, gone); err != nil {
		return s, false, err
	}
	if s.PortForwardings, err = routerPortForwardings(ctx, q, routerID, gone); err != nil {
		return s, false, err
	}
	return s, true, nil
}

// routerInterfacePorts reads the ports of a router's interfaces, in id order, each once: a port has a
// router_interfaces row per subnet it joins to the router (T315).
func routerInterfacePorts(ctx context.Context, q subnetQuerier, routerID string) ([]gatewayPort, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, p.network_id::text, p.fixed_ips
		  FROM ports p
		 WHERE p.id IN (SELECT ri.port_id FROM router_interfaces ri WHERE ri.router_id::text = $1)
		 ORDER BY p.id`, routerID)
	if err != nil {
		return nil, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
	}
	defer rows.Close()
	var out []gatewayPort
	for rows.Next() {
		var p gatewayPort
		if err := rows.Scan(&p.id, &p.networkID, &p.fixedIPs); err != nil {
			return nil, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("router %s: read its interfaces: %w", routerID, err)
	}
	return out, nil
}

// routerSubnet is what a router needs of a subnet: its prefix length, its version and its gateway.
type routerSubnet struct {
	prefix, version int
	gateway         string
}

func subnetsOf(ctx context.Context, q subnetQuerier, ids []string) (map[string]routerSubnet, error) {
	out := map[string]routerSubnet{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		"SELECT id::text, cidr, COALESCE(gateway_ip, ''), ip_version FROM subnets WHERE id::text = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("read subnets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, cidr, gateway string
		var version int
		if err := rows.Scan(&id, &cidr, &gateway, &version); err != nil {
			return nil, fmt.Errorf("read subnets: %w", err)
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue // not a prefix: nothing to address with it
		}
		out[id] = routerSubnet{prefix: prefix.Bits(), version: version, gateway: gateway}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read subnets: %w", err)
	}
	return out, nil
}

// routerFloatingIPs reads the floating IPs through a router that are on a port.
func routerFloatingIPs(ctx context.Context, q subnetQuerier, routerID string, gone func(string) bool) ([]networking.RouterFloatingIP, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, floating_ip_address, fixed_ip_address FROM floating_ips
		 WHERE router_id::text = $1 AND port_id IS NOT NULL AND fixed_ip_address IS NOT NULL
		 ORDER BY floating_ip_address`, routerID)
	if err != nil {
		return nil, fmt.Errorf("router %s: read its floating IPs: %w", routerID, err)
	}
	defer rows.Close()
	var out []networking.RouterFloatingIP
	for rows.Next() {
		var id string
		var f networking.RouterFloatingIP
		if err := rows.Scan(&id, &f.Address, &f.FixedIP); err != nil {
			return nil, fmt.Errorf("router %s: read its floating IPs: %w", routerID, err)
		}
		if !gone(id) {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("router %s: read its floating IPs: %w", routerID, err)
	}
	return out, nil
}

// routerPortForwardings reads the port forwardings on the floating IPs through a router.
func routerPortForwardings(ctx context.Context, q subnetQuerier, routerID string, gone func(string) bool) ([]networking.RouterPortForwarding, error) {
	rows, err := q.Query(ctx, `
		SELECT pf.id::text, f.id::text, f.floating_ip_address, pf.protocol, pf.external_port, pf.internal_ip_address, pf.internal_port
		  FROM port_forwardings pf JOIN floating_ips f ON f.id = pf.floatingip_id
		 WHERE f.router_id::text = $1
		 ORDER BY pf.id`, routerID)
	if err != nil {
		return nil, fmt.Errorf("router %s: read its port forwardings: %w", routerID, err)
	}
	defer rows.Close()
	var out []networking.RouterPortForwarding
	for rows.Next() {
		var floatingID string
		var pf networking.RouterPortForwarding
		if err := rows.Scan(&pf.ID, &floatingID, &pf.FloatingIP, &pf.Protocol, &pf.ExternalPort, &pf.InternalIP, &pf.InternalPort); err != nil {
			return nil, fmt.Errorf("router %s: read its port forwardings: %w", routerID, err)
		}
		if !gone(pf.ID) && !gone(floatingID) {
			out = append(out, pf)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("router %s: read its port forwardings: %w", routerID, err)
	}
	return out, nil
}

// dhcpDataplane converges networks' DHCP on the network node (R29, R41): *networking.DHCPManager, or a
// fake in tests. A nil state converges the network's DHCP to absent.
type dhcpDataplane interface {
	ConvergeDHCP(ctx context.Context, networkID string, state *networking.DHCPState) error
	// HeldNetworks lists the networks whose DHCP namespaces the node holds.
	HeldNetworks(ctx context.Context) ([]string, error)
}

// MetadataListeners serve the metadata service in networks' DHCP namespaces on the network node (R29):
// the metadata service's, handed to Neutron by main.go. Open serves a network's namespace, Close stops
// serving it; both are idempotent. Serving lists the networks served.
type MetadataListeners interface {
	Open(ctx context.Context, networkID string) error
	Close(networkID string) error
	Serving() []string
}

// SetMetadataListeners gives the DHCP unit the metadata listeners it opens and closes with each
// network's DHCP. Without them it serves none, as in stub mode.
func (svc *Service) SetMetadataListeners(l MetadataListeners) { svc.listeners = l }

// portFirewall converges the firewall of the ports bound to this node (R32, R41):
// *networking.FirewallManager, or a fake in tests.
type portFirewall interface {
	ConvergePorts(ctx context.Context, ports []networking.FirewallPort) error
	// RemoveAll removes the node's whole port firewall, its fixed chains included: teardown's (R48).
	RemoveAll(ctx context.Context) error
}
