package neutron

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// NetworkReconciler keeps this node's real-mode network state matching the records, in both directions,
// at start and on every interval (SPEC-033 FR-010 as FR-042 restates it, FR-036, FR-044; research R24,
// R26, R28, R29, R32). It runs on every node and converges only what its node holds (R26):
//
//   - every node: the networks' bridges; once, the retired state of earlier layouts (FR-044); the uplink's
//     proxy entries no router's leg routes; the chains of the ports bound to it;
//   - the network node: every router and every network's DHCP with a record — which restarts a
//     dnsmasq that died with an earlier process and re-opens its metadata listener — and every one it
//     holds without a record removed;
//   - any other node: every router and DHCP namespace it holds removed, records or not, since only the
//     network node holds them (it was the network node once, say).
//
// It lists the node before it reads the records, and removes by "no record" only after a complete read
// (R24): every path commits a record before it makes the state, so what the listing found has a record
// the read can see, and an incomplete read proves nothing. What it did read it still converges. A unit
// that already matches its records is only read (R7). Best-effort per unit: one that fails is logged and
// the next pass tries again.
type NetworkReconciler struct {
	svc          *Service
	pollInterval time.Duration
	stopChan     chan struct{}
	// readIDs reads a column of ids, and whether every row was read. A field so a test can scope a pass
	// to its own records, or make a read partial.
	readIDs func(ctx context.Context, query string, args ...any) (ids []string, complete bool, err error)
	// retiredRemoved is set once the retired state is gone: nothing writes it again (FR-044).
	retiredRemoved bool
}

// NewNetworkReconciler binds a reconciler to the Neutron service's managers.
// A zero/negative interval defaults to 30s.
func NewNetworkReconciler(svc *Service, pollInterval time.Duration) *NetworkReconciler {
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	return &NetworkReconciler{
		svc:          svc,
		pollInterval: pollInterval,
		stopChan:     make(chan struct{}),
		readIDs: func(ctx context.Context, query string, args ...any) ([]string, bool, error) {
			return readIDs(ctx, svc.pool, query, args...)
		},
	}
}

// Start runs an initial reconcile, then re-reconciles on the poll interval until
// Stop() or context cancellation.
func (r *NetworkReconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	r.reconcile(ctx)
	for {
		select {
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// PollInterval returns the interval the reconciler actually runs at, after the clamp below.
//
// Callers used to log the *configured* value, so a deployment that left it unset printed
// "interval: 0s" and read as though periodic reconciliation were disabled — it is not (#546).
// Exposing the effective value keeps the clamp in one place rather than duplicating it at the
// log site, where the two could drift.
func (r *NetworkReconciler) PollInterval() time.Duration { return r.pollInterval }

// Stop ends the reconcile loop.
func (r *NetworkReconciler) Stop() { close(r.stopChan) }

// reconcile is one pass. On the network node the stray proxy entries go before the routers are
// converged: a router whose address a racing request gave it, and whose convergence found the stray
// entry and kept it, gets it back in the same pass. On another node the routers it holds go first, with
// the entries their legs route.
func (r *NetworkReconciler) reconcile(ctx context.Context) {
	r.reconcileNetworks(ctx)
	r.removeRetired(ctx)
	if r.svc.isNetworkNode() {
		r.reconcileDHCP(ctx)
		r.removeStrayProxies(ctx)
		r.reconcileRouters(ctx)
	} else {
		r.removeNetworkNodeUnits(ctx)
		r.removeStrayProxies(ctx)
	}
	r.reconcilePorts(ctx)
	r.reportGrandfatheredGateways(ctx)
}

// readIDs reads a single column of ids. complete is false when a row could not be read: such a list is
// safe to converge from and unsafe to remove by.
func readIDs(ctx context.Context, q subnetQuerier, query string, args ...any) (ids []string, complete bool, err error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	complete = true
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Warn().Err(err).Msg("reconcile: an unreadable row")
			complete = false
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("reconcile: a read ended early")
		complete = false
	}
	return ids, complete, nil
}

// reconcileNetworks ensures each network's bridge exists. Best-effort: bridges
// are kernel objects that normally persist across a pod restart, so an
// "already exists" outcome is expected and logged at debug.
func (r *NetworkReconciler) reconcileNetworks(ctx context.Context) {
	// A short list is safe to ENSURE from — only the prune needs a complete one.
	ids, _, err := r.readIDs(ctx, `SELECT id::text FROM networks`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query networks")
		return
	}
	for _, networkID := range ids {
		bridgeName := networking.BridgeNameForNetwork(networkID)
		if err := r.svc.brManager.CreateBridge(bridgeName); err != nil {
			log.Debug().Err(err).Str("network_id", networkID).Str("bridge", bridgeName).Msg("reconcile: bridge ensure (likely already present)")
		}
	}
	r.pruneDeletedNetworks(ctx)
}

// pruneDeletedNetworks removes the bridges of networks that no longer exist: a deleted network's bridge
// persists in the kernel, colliding with a later network that draws the same bridge name. A network's
// DHCP leaves the network node with its delete (DeleteNetwork); the DHCP pass removes any it left.
//
// The bridges are LISTED FIRST and the networks read after them. Every path commits a network's row
// before creating its bridge, so whatever the listing found belongs to a row this read can see. The
// pass used to read once, before ensuring and listing, and a network created in between lost its
// bridge right after its 201.
//
// The prune reads "not in the list" as "deleted", so it may not run against a list known to be short:
// a network that was merely unread would lose its bridge, taking its guests off the network. Skipping
// only defers the hygiene to the next pass.
func (r *NetworkReconciler) pruneDeletedNetworks(ctx context.Context) {
	bridges, listErr := r.svc.brManager.ListManagedBridges()
	if listErr != nil {
		log.Debug().Err(listErr).Msg("reconcile: list bridges for prune")
	}

	ids, complete, err := r.readIDs(ctx, `SELECT id::text FROM networks`)
	if err != nil || !complete {
		log.Warn().Err(err).Msg("reconcile: network enumeration incomplete; skipping the bridge prune this pass")
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[networking.BridgeNameForNetwork(id)] = true
	}

	for _, b := range bridges {
		if want[b] {
			continue
		}
		if err := r.svc.brManager.DeleteBridge(b); err != nil {
			log.Debug().Err(err).Str("bridge", b).Msg("reconcile: prune stale bridge")
		} else {
			log.Info().Str("bridge", b).Msg("reconcile: pruned stale bridge (network deleted)")
		}
	}
}

// removeRetired removes what earlier layouts left on this node and this one never writes (FR-044), once
// per process: a failed removal is made again the next pass, a done one never, since nothing writes that
// state again. It hands the node no records, so only what a mark or a name makes Lightstack's goes: a
// tenant chooses a subnet's CIDR, and an operator's own masquerade or DNAT has the earlier layout's
// shape (SPEC-033 T190). The records are the operator's teardown's (lightstack cleanup).
func (r *NetworkReconciler) removeRetired(ctx context.Context) {
	if r.retiredRemoved {
		return
	}
	uplink, err := r.svc.uplink()
	if err != nil {
		log.Debug().Err(err).Msg("reconcile: no uplink; the retired state only it makes recognisable stays")
		uplink = ""
	}
	if err := r.svc.retired.Remove(ctx, uplink, networking.RetiredRecords{}); err != nil {
		log.Warn().Err(err).Msg("reconcile: remove the retired state of an earlier layout; the next pass tries again")
		return
	}
	r.retiredRemoved = true
}

// converge converges a unit this node holds within the node call's deadline, logging a failure.
func (r *NetworkReconciler) converge(ctx context.Context, req unitRequest) {
	uctx, cancel := context.WithTimeout(ctx, nodeApplyDeadline)
	defer cancel()
	if err := r.svc.convergeUnit(uctx, req); err != nil {
		log.Warn().Err(err).Str("unit", string(req.Unit)).Strs("ids", req.IDs).Msg("reconcile: converge a unit")
	}
}

// heldWithout is what the node holds and the records do not name, once.
func heldWithout(held, records []string) []string {
	var out []string
	for _, id := range held {
		if !slices.Contains(records, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// reconcileDHCP converges, on the network node, the DHCP of every network with a DHCP subnet or a DHCP
// port (R29), each brought in line with its subnets first, and after a complete read every network's
// DHCP the node holds, or serves metadata for, without one: converged as its records say, which is to
// absent.
func (r *NetworkReconciler) reconcileDHCP(ctx context.Context) {
	held, err := r.svc.dhcp.HeldNetworks(ctx)
	listed := err == nil
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: list the networks' DHCP namespaces")
	}
	if r.svc.listeners != nil {
		held = append(held, r.svc.listeners.Serving()...)
	}
	networks, complete, err := r.readIDs(ctx, `
		SELECT network_id::text FROM subnets WHERE enable_dhcp = true AND ip_version = 4
		UNION
		SELECT network_id::text FROM ports WHERE device_owner = '`+ownerDHCP+`' AND network_id IS NOT NULL`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: read the networks with DHCP")
		return
	}
	for _, networkID := range networks {
		r.reconcileNetworkDHCP(ctx, networkID)
	}
	if !listed || !complete {
		log.Warn().Msg("reconcile: the networks with DHCP were not all read; removing no network's DHCP this pass")
		return
	}
	for _, networkID := range heldWithout(held, networks) {
		r.converge(ctx, unitRequest{Unit: unitDHCP, IDs: []string{networkID}})
	}
}

// reconcileNetworkDHCP brings a network's DHCP port in line with its DHCP subnets, locking nothing when
// it is, and converges the network's DHCP on this, the network node.
func (r *NetworkReconciler) reconcileNetworkDHCP(ctx context.Context, networkID string) {
	inLine, err := dhcpPortInLine(ctx, r.svc.pool, networkID)
	if err == nil && !inLine {
		err = realignDHCPPort(ctx, r.svc.pool, networkID)
	}
	if err != nil {
		if !errors.Is(err, database.ErrNoRows) { // no row: deleted since it was read
			log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: bring the network's DHCP port in line with its subnets")
		}
		return
	}
	r.converge(ctx, unitRequest{Unit: unitDHCP, IDs: []string{networkID}})
}

// reconcileRouters converges, on the network node, every router with a record (R28), and after a
// complete read every router the node holds without one: converged as its records say, which is to
// absent — its namespace, legs, host routes and proxy entries.
func (r *NetworkReconciler) reconcileRouters(ctx context.Context) {
	held, err := r.svc.routers.HeldRouters(ctx)
	listed := err == nil
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: list the routers' namespaces")
	}
	routers, complete, err := r.readIDs(ctx, `SELECT id::text FROM routers`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: read the routers")
		return
	}
	for _, routerID := range routers {
		r.converge(ctx, unitRequest{Unit: unitRouter, IDs: []string{routerID}})
	}
	if !listed || !complete {
		log.Warn().Msg("reconcile: the routers were not all read; removing no router this pass")
		return
	}
	for _, routerID := range heldWithout(held, routers) {
		r.converge(ctx, unitRequest{Unit: unitRouter, IDs: []string{routerID}})
	}
}

// removeNetworkNodeUnits removes, from a node that is not the network node, every router and every
// network's DHCP it holds, with their metadata listeners: only the network node holds them (FR-033).
// No record is read, so none can be missing. No request converges them here, so no unit lock is needed.
// Only while the node network_node names is an active node: a name that matches none would otherwise
// take every router and DHCP away from every node (SPEC-033 T276). Without a registry — VXLAN off — every
// node is its own network node and never gets here.
func (r *NetworkReconciler) removeNetworkNodeUnits(ctx context.Context) {
	if r.svc.nodes.Registry != nil {
		if _, err := r.svc.nodeAddress(ctx, r.svc.networkNodeName()); err != nil {
			log.Error().Err(err).Str("network_node", r.svc.networkNodeName()).
				Msg("reconcile: the network node is no active node; this node removes no router or DHCP until it is")
			return
		}
	}
	uplink, err := r.svc.uplink()
	if err != nil {
		uplink = "" // the routes, legs and namespace still go; the entries on an unknown uplink cannot
	}
	routers, err := r.svc.routers.HeldRouters(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: list the routers' namespaces")
	}
	for _, routerID := range routers {
		uctx, cancel := context.WithTimeout(ctx, nodeApplyDeadline)
		if err := r.svc.routers.ConvergeRouter(uctx, routerID, nil, uplink); err != nil {
			log.Warn().Err(err).Str("router_id", routerID).Msg("reconcile: remove a router from a node that is not the network node")
		}
		cancel()
	}
	networks, err := r.svc.dhcp.HeldNetworks(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: list the networks' DHCP namespaces")
	}
	if r.svc.listeners != nil {
		networks = append(networks, r.svc.listeners.Serving()...)
	}
	for _, networkID := range heldWithout(networks, nil) {
		if r.svc.listeners != nil {
			if err := r.svc.listeners.Close(networkID); err != nil {
				log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: close a metadata listener")
			}
		}
		uctx, cancel := context.WithTimeout(ctx, nodeApplyDeadline)
		if err := r.svc.dhcp.ConvergeDHCP(uctx, networkID, nil); err != nil {
			log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: remove a network's DHCP from a node that is not the network node")
		}
		cancel()
	}
}

// removeStrayProxies removes the uplink's proxy entries that no router's leg routes, when a record makes
// their address Lightstack's (contracts/router-namespace-dataplane.md, Reconciliation): an entry a
// router's namespace left when it went — out of band, or on a node that is no longer the network node,
// where no router is left to route any. An address no record knows is not Lightstack's to remove.
func (r *NetworkReconciler) removeStrayProxies(ctx context.Context) {
	uplink, err := r.svc.uplink()
	if err != nil || uplink == "" {
		return
	}
	ours, err := lightstackAddresses(ctx, r.svc.pool)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: read the addresses Lightstack hands out; removing no proxy entry this pass")
		return
	}
	removed, err := r.svc.routers.RemoveStrayProxies(ctx, uplink, ours)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: remove stray proxy entries")
	}
	if len(removed) > 0 {
		log.Info().Strs("addresses", removed).Msg("reconcile: removed proxy entries no router routes")
	}
}

// lightstackAddresses recognises the addresses Lightstack hands out on its external networks: every
// floating IP's, every router gateway's, and every one in the pools of an external network's IPv4
// subnets (FR-031, FR-038). It reads them all, or fails.
func lightstackAddresses(ctx context.Context, q subnetQuerier) (func(string) bool, error) {
	held := map[netip.Addr]bool{}
	floating, err := queryStrings(ctx, q, `SELECT floating_ip_address FROM floating_ips`)
	if err != nil {
		return nil, err
	}
	for _, a := range floating {
		if ip, err := netip.ParseAddr(a); err == nil {
			held[ip.Unmap()] = true
		}
	}
	gateways, err := readFixedIPs(ctx, q, `SELECT fixed_ips FROM ports WHERE `+gatewayPortWhere(""))
	if err != nil {
		return nil, err
	}
	for _, f := range gateways {
		if ip, err := netip.ParseAddr(f.IPAddress); err == nil {
			held[ip.Unmap()] = true
		}
	}
	pools, err := externalPools(ctx, q)
	if err != nil {
		return nil, err
	}
	return func(a string) bool {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return false
		}
		ip = ip.Unmap()
		return held[ip] || slices.ContainsFunc(pools, func(p ipRange) bool { return p.contains(ip) })
	}, nil
}

// readFixedIPs reads every fixed IP of the ports a query selects by their fixed_ips column.
func readFixedIPs(ctx context.Context, q subnetQuerier, query string) (common.FixedIPs, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out common.FixedIPs
	for rows.Next() {
		var fixed common.FixedIPs
		if err := rows.Scan(&fixed); err != nil {
			return nil, err
		}
		out = append(out, fixed...)
	}
	return out, rows.Err()
}

// externalPools reads the allocation pools of every external network's IPv4 subnets.
func externalPools(ctx context.Context, q subnetQuerier) ([]ipRange, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id::text, s.cidr, s.gateway_ip, s.pools_stored FROM subnets s JOIN networks n ON n.id = s.network_id
		 WHERE n.external = true AND s.ip_version = 4`)
	if err != nil {
		return nil, err
	}
	type subnet struct {
		id, cidr string
		gateway  *string
		stored   *bool
	}
	var subnets []subnet
	for rows.Next() {
		var s subnet
		if err := rows.Scan(&s.id, &s.cidr, &s.gateway, &s.stored); err != nil {
			rows.Close()
			return nil, err
		}
		subnets = append(subnets, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(subnets))
	for _, s := range subnets {
		ids = append(ids, s.id)
	}
	stored, err := storedPools(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	var pools []ipRange
	for _, s := range subnets {
		prefix, err := parsePrefix(s.cidr)
		if err != nil {
			continue
		}
		pools = append(pools, poolsFor(prefix, parseGateway(s.gateway), s.stored != nil && *s.stored, stored[s.id])...)
	}
	return pools, nil
}

// reconcilePorts converges the chains of every port bound to this node, which removes the chains of
// any port that is not (R32): a port whose binding moved while this node was down, say. Its read of
// the bound ports fails whole or not at all, so it never removes after a partial one.
func (r *NetworkReconciler) reconcilePorts(ctx context.Context) {
	r.converge(ctx, unitRequest{Unit: unitPorts})
}

// reportGrandfatheredGateways logs any router whose gateway network is not marked external.
//
// Migration 066 added networks.external defaulting to false, so every gateway configured before it
// looks invalid to the new validation. Those keep working — this reconcile pass deliberately does NOT
// learn the check, because withdrawing SNAT here would silently remove working guest egress on
// upgrade (FR-035a). Logging them instead makes the condition discoverable so an operator can mark
// the network and converge deliberately rather than finding out later (FR-035b).
func (r *NetworkReconciler) reportGrandfatheredGateways(ctx context.Context) {
	rows, err := r.svc.pool.Query(ctx, `
		SELECT rt.id, n.id, n.name
		FROM routers rt
		JOIN networks n ON n.id::text = rt.external_gateway_info->>'network_id'
		WHERE rt.external_gateway_info IS NOT NULL
		  AND rt.external_gateway_info::text <> 'null'
		  AND n.external = false`)
	if err != nil {
		log.Debug().Err(err).Msg("reconcile: failed to check grandfathered gateways")
		return
	}
	defer rows.Close()

	// Scan and iteration failures are logged, not swallowed. Warning the operator IS this function's
	// output (FR-035b), so a row dropped in silence is not a degraded result — it is the whole result
	// going missing, and its silence reads as "nothing to converge". Same rule ListSubnets follows for
	// the same reason (FR-024): keep serving the rest, but say what was skipped.
	for rows.Next() {
		var routerID, networkID, networkName string
		if err := rows.Scan(&routerID, &networkID, &networkName); err != nil {
			log.Warn().Err(err).Msg("reconcile: skipping unreadable grandfathered-gateway row")
			continue
		}
		log.Warn().
			Str("router_id", routerID).
			Str("network_id", networkID).
			Str("network_name", networkName).
			Msg("router gateway network is not marked router:external; kept working (grandfathered), " +
				"but a NEW gateway on it would be refused — mark it external to converge")
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("reconcile: grandfathered-gateway scan incomplete — some routers may " +
			"need converging without having been reported")
	}
}
