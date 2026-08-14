package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// NetworkReconciler rebuilds real-mode network state from the database on
// startup and periodically, so VM networking survives a Lightstack control-plane
// restart/upgrade (SPEC-009 WS-2). On a pod restart the kernel objects
// (bridges, netns, taps, iptables) persist, but dnsmasq is a Lightstack child process
// that dies with the pod — so the operative job is re-spawning DHCP, with a
// best-effort bridge ensure. Reconciliation is idempotent and best-effort per
// resource: one bad tenant never blocks the others or control-plane readiness.
// Modeled on the VXLAN coordinator.
type NetworkReconciler struct {
	svc          *Service
	pollInterval time.Duration
	stopChan     chan struct{}
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

func (r *NetworkReconciler) reconcile(ctx context.Context) {
	r.reconcileNetworks(ctx)
	r.reconcileSubnets(ctx)
	r.reconcileNAT(ctx)
}

// reconcileNAT re-asserts egress SNAT for every router with an external gateway
// (per attached internal subnet) and re-asserts every associated floating IP's
// NAT — so VM internet access and floating-IP reachability survive a Lightstack
// control-plane restart (SPEC-009 WS-3 / T035). Idempotent.
func (r *NetworkReconciler) reconcileNAT(ctx context.Context) {
	r.reportGrandfatheredGateways(ctx)

	// Egress: internal subnets behind a router whose gateway masquerades them. enable_snat is honoured
	// here as everywhere (egressSNATCIDRs); it used not to be, so a range withdrawn when SNAT was
	// turned off came straight back on this pass.
	cidrs, err := r.svc.egressSNATCIDRs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: egress subnets read incomplete; ensuring what was read this pass")
	}

	// Floating IPs currently associated to a port.
	type fipRow struct{ floating, fixed, port string }
	var fips []fipRow
	if rows, err := r.svc.pool.Query(ctx, `
		SELECT floating_ip_address, fixed_ip_address, port_id
		FROM floating_ips
		WHERE port_id IS NOT NULL AND fixed_ip_address IS NOT NULL`); err == nil {
		for rows.Next() {
			var f fipRow
			if err := rows.Scan(&f.floating, &f.fixed, &f.port); err != nil {
				log.Warn().Err(err).Str("operation", "reconcileNAT").Msg("failed to scan floating IP row")
			} else if f.floating != "" && f.fixed != "" {
				fips = append(fips, f)
			}
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("reconcile: floating IP rows error; NAT set may be incomplete this pass")
		}
		rows.Close()
	} else {
		log.Error().Err(err).Msg("reconcile: failed to query floating IPs")
	}

	if len(cidrs) == 0 && len(fips) == 0 {
		return
	}
	uplink, err := networking.DefaultUplinkInterface()
	if err != nil {
		log.Debug().Err(err).Msg("reconcile: no uplink for north-south NAT")
		return
	}
	for _, cidr := range cidrs {
		if err := r.svc.routerManager.EnsureEgressSNAT(cidr, uplink); err != nil {
			log.Debug().Err(err).Str("cidr", cidr).Msg("reconcile: egress SNAT")
		}
	}
	for _, f := range fips {
		if err := r.svc.routerManager.EnsureFloatingIP(f.floating, f.fixed, uplink); err != nil {
			log.Debug().Err(err).Str("floating_ip", f.floating).Msg("reconcile: floating IP")
		}
		if err := r.svc.ensureIngressFilter(ctx, f.fixed, uplink, f.port); err != nil {
			log.Debug().Err(err).Str("fixed_ip", f.fixed).Msg("reconcile: ingress filter")
		}
	}
}

// reconcileNetworks ensures each network's bridge exists. Best-effort: bridges
// are kernel objects that normally persist across a pod restart, so an
// "already exists" outcome is expected and logged at debug.
func (r *NetworkReconciler) reconcileNetworks(ctx context.Context) {
	// A short list is safe to ENSURE from — only the prune needs a complete one.
	ids, _, err := r.readNetworkIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query networks")
		return
	}
	for _, networkID := range ids {
		bridgeName := "br-" + networkID[:8]
		if err := r.svc.brManager.CreateBridge(bridgeName); err != nil {
			log.Debug().Err(err).Str("network_id", networkID).Str("bridge", bridgeName).Msg("reconcile: bridge ensure (likely already present)")
		}
	}
	r.pruneDeletedNetworks(ctx)
}

// readNetworkIDs reads every network id. complete is false when a row could not be read: such a list
// is safe to ENSURE from and unsafe to PRUNE from. This used to skip only on rows.Err(), which left
// the more likely case — one unreadable row, logged and stepped over — feeding a short list straight
// into the prune.
func (r *NetworkReconciler) readNetworkIDs(ctx context.Context) (ids []string, complete bool, err error) {
	rows, err := r.svc.pool.Query(ctx, `SELECT id FROM networks`)
	if err != nil {
		return nil, false, err
	}
	complete = true
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Warn().Err(err).Str("operation", "reconcileNetworks").Msg("failed to scan row")
			complete = false
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("reconcile: networks rows error")
		complete = false
	}
	return ids, complete, nil
}

// pruneDeletedNetworks removes real-mode state for networks that no longer exist: a deleted network's
// bridge persists in the kernel and keeps claiming its subnet gateway IP, colliding with a later
// network on the same CIDR (the recurring single-node stale-bridge problem), and its dnsmasq keeps
// running.
//
// The host state is LISTED FIRST and the networks read after it. Every path commits a network's row
// before creating host state for it, so whatever the listing found belongs to a row this read can
// see. The pass used to read once, before ensuring and listing, and a network created in between lost
// its bridge and its DHCP right after its 201.
//
// Both steps read "not in the list" as "deleted", so neither may run against a list known to be
// short: a network that was merely unread would lose its bridge and have its dnsmasq stopped — taking
// DHCP away from running guests. Skipping only defers the hygiene to the next pass, which is the same
// trade reconcileSubnets makes for the metadata redirect.
func (r *NetworkReconciler) pruneDeletedNetworks(ctx context.Context) {
	bridges, listErr := r.svc.brManager.ListManagedBridges()
	if listErr != nil {
		log.Debug().Err(listErr).Msg("reconcile: list bridges for prune")
	}
	served := r.svc.dhcpManager.ServedNetworks()

	ids, complete, err := r.readNetworkIDs(ctx)
	if err != nil || !complete {
		log.Warn().Err(err).Msg("reconcile: network enumeration incomplete; skipping bridge and DHCP prune this pass")
		return
	}
	active := make(map[string]bool, len(ids))
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		active[id] = true
		want["br-"+id[:8]] = true
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
	for _, networkID := range served {
		if !active[networkID] {
			_ = r.svc.dhcpManager.StopDHCP(networkID)
		}
	}
}

// reconcileSubnets re-applies DHCP for every network that has a DHCP-enabled
// subnet but is not currently serving — the part that does not survive a pod
// restart. It delegates to applyDHCPForNetwork(restart=false), which is
// idempotent (IsRunning guard) while still self-healing a dnsmasq that was
// killed out-of-band (SC-010) and re-asserting the bridge IP + metadata DNAT
// every pass (SPEC-009 WS-4 / T029). It then prunes the metadata DNAT rules that
// pass did NOT assert, reclaiming what the delete paths leaked.
func (r *NetworkReconciler) reconcileSubnets(ctx context.Context) {
	rows, err := r.svc.pool.Query(ctx,
		`SELECT DISTINCT network_id FROM subnets WHERE enable_dhcp = true`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query subnets")
		return
	}
	var networkIDs []string
	scanFailed := false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			// Logged so the bad row is discoverable (#549), and recorded so the prune below cannot
			// treat a short list as authoritative (#546). Both halves are needed: the log alone
			// leaves the prune destructive, the flag alone leaves the cause invisible.
			log.Warn().Err(err).Str("operation", "reconcileSubnets").Msg("failed to scan row")
			scanFailed = true
			continue
		}
		networkIDs = append(networkIDs, id)
	}
	rows.Close()
	// A short list is safe to ASSERT from and unsafe to PRUNE from, so the two halves of this pass
	// are treated differently rather than the whole pass being abandoned (#549 returned here).
	// Re-applying DHCP for the networks that were read is idempotent and only ever adds; the ones
	// missed are picked up next pass. Pruning against the same short list would delete the
	// metadata redirect out from under a network that was merely unread.
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("reconcile: subnets rows error")
		scanFailed = true
	}

	// bridge -> the address applyDHCPForNetwork pointed the redirect at. Collected from the calls
	// themselves rather than re-derived from a second query, so the two can never disagree about
	// which address a gatewayless subnet got and prune a rule that was just asserted.
	desired := make(map[string]string, len(networkIDs))
	for _, networkID := range networkIDs {
		bridge, addr, err := r.svc.applyDHCPForNetwork(ctx, networkID, false)
		if err != nil {
			log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: failed to read DHCP state")
			scanFailed = true
			continue
		}
		if bridge != "" && addr != "" {
			desired[bridge] = addr
		}
	}

	// A partial read is not a prune input: pruning against a set that is missing a network — one
	// the enumeration skipped, or one whose subnet could not be read — deletes the metadata redirect
	// out from under it, which breaks cloud-init for its guests until the next clean pass. Skipping
	// a pass only defers hygiene.
	if scanFailed {
		log.Warn().Msg("reconcile: subnet state incomplete; skipping metadata DNAT prune this pass")
		return
	}
	// Prune lists the rules before it calls this, so a network whose subnet was created after the
	// enumeration above — its handler having added the redirect since — is in the picture too.
	if removed, err := r.svc.mdRedirect.Prune(func() (map[string]string, error) {
		return r.svc.desiredRedirectsNow(ctx, desired)
	}); err != nil {
		log.Debug().Err(err).Msg("reconcile: metadata DNAT prune")
	} else if removed > 0 {
		log.Info().Int("rules", removed).Msg("reconcile: pruned stale metadata DNAT rules (network or gateway gone)")
	}
}

// desiredRedirectsNow returns the pass's redirects plus that of every network with a DHCP-enabled
// subnet the pass did not reach: one whose subnet was created after the pass enumerated them. It reads
// when called, which Prune does after listing the rules, so the network of every listed rule is in
// the result. The pass's own entries are kept as asserted; an added one is derived as
// applyDHCPForNetwork derives it, from the network's first DHCP-enabled subnet.
func (svc *Service) desiredRedirectsNow(ctx context.Context, pass map[string]string) (map[string]string, error) {
	rows, err := svc.pool.Query(ctx, `
		SELECT DISTINCT ON (network_id) network_id::text, cidr, gateway_ip
		FROM subnets
		WHERE enable_dhcp = true
		ORDER BY network_id, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	want := maps.Clone(pass)
	if want == nil {
		want = map[string]string{}
	}
	for rows.Next() {
		var networkID, cidr string
		var gatewayIP *string
		if err := rows.Scan(&networkID, &cidr, &gatewayIP); err != nil {
			return nil, err // a short picture is not a prune input
		}
		bridge := "br-" + networkID[:8]
		if _, asserted := want[bridge]; asserted {
			continue
		}
		if addr := bridgeAddrFor(gatewayIP, cidr); addr != "" {
			want[bridge] = addr
		}
	}
	return want, rows.Err()
}

// applyDHCPForNetwork (re)asserts the bridge gateway IP, the metadata DNAT, and
// the dnsmasq server (with MAC->IP reservations from the DB) for a network's
// DHCP-enabled subnet. With restart=true it reloads dnsmasq so a newly allocated
// port's reservation takes effect *before* its VM boots — dnsmasq must hand out
// the exact fixed IP neutron assigned the port, or OpenStack's view and the
// guest disagree (breaks metadata identity). This is the synchronous path called
// from port allocation and subnet create (Constitution II). With restart=false
// (the reconciler) it only starts dnsmasq if it is not already running, but
// always re-asserts the bridge IP and metadata redirect. Best-effort.
//
// Returns the bridge and the address it pointed the metadata redirect at, or two empty strings when
// it asserted nothing (stub mode, no DHCP-enabled subnet, unusable CIDR). The reconciler collects
// these into the desired set it prunes against, so what gets kept is exactly what got asserted.
// The error reports a subnet that could not be READ, which is not the same as there being none: the
// reconciler must not prune against a set missing a network it merely failed to read.
func (svc *Service) applyDHCPForNetwork(ctx context.Context, networkID string, restart bool) (string, string, error) {
	if svc.mode == "stub" {
		return "", "", nil
	}
	var cidr string
	// gateway_ip is nullable and a subnet may legitimately have none. Scanning it into a plain string
	// made this Scan fail on such a subnet, and the early return then left it with NO DHCP at all —
	// silently (SPEC-024 / cloud-storage#546 FR-023a).
	var gatewayIP *string
	var dns []string
	err := svc.pool.QueryRow(ctx, `
		SELECT cidr, gateway_ip, dns_nameservers FROM subnets
		WHERE network_id = $1 AND enable_dhcp = true
		ORDER BY created_at, id LIMIT 1`, networkID).Scan(&cidr, &gatewayIP, &dns)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil // no DHCP-enabled subnet for this network — nothing to do
	}
	if err != nil {
		return "", "", fmt.Errorf("read DHCP subnet of network %s: %w", networkID, err)
	}
	_, ipNet, perr := net.ParseCIDR(cidr)
	if perr != nil {
		log.Warn().Err(perr).Str("network_id", networkID).Str("cidr", cidr).Msg("dhcp apply: bad subnet CIDR; skipping")
		return "", "", nil
	}
	bridgeName := "br-" + networkID[:8]

	// The bridge needs an address in the subnet whether or not the subnet advertises a gateway:
	// dnsmasq binds to it, and the metadata DNAT hangs off it. With a gateway that address IS the
	// gateway; without one we still take the first host address, but only as the bridge's own
	// on-link address — it is never advertised as a default route (FR-023a).
	bridgeAddr := bridgeAddrFor(gatewayIP, cidr)

	ones, _ := ipNet.Mask.Size()
	if err := svc.brManager.EnsureBridgeIP(bridgeName, fmt.Sprintf("%s/%d", bridgeAddr, ones)); err != nil {
		log.Debug().Err(err).Str("bridge", bridgeName).Msg("dhcp apply: bridge IP ensure")
	}
	if err := svc.mdRedirect.Ensure(bridgeName, bridgeAddr); err != nil {
		log.Debug().Err(err).Str("bridge", bridgeName).Msg("dhcp apply: metadata redirect ensure")
	}

	if !restart && svc.dhcpManager.IsRunning(networkID) {
		return bridgeName, bridgeAddr, nil // already serving — idempotent, but the redirect above is ours
	}

	dhcpStart, dhcpEnd := dhcpRange(ipNet)
	cfg := networking.DHCPConfig{
		NetworkID:  networkID,
		BridgeName: bridgeName,
		SubnetCIDR: cidr,
		// Empty when the subnet has no gateway: no default route is advertised. The metadata route
		// is carried separately so it survives that case (FR-023a).
		GatewayIP:        derefOr(gatewayIP, ""),
		MetadataNextHop:  bridgeAddr,
		DNSServers:       dns,
		DHCPRangeStart:   dhcpStart,
		DHCPRangeEnd:     dhcpEnd,
		LeaseTime:        "24h",
		HostReservations: svc.dhcpReservations(ctx, networkID),
	}
	// restart=true reloads to pick up a newly allocated port's reservation: kill
	// and respawn dnsmasq WITHOUT dropping leases, serialized so concurrent boots
	// on this network can't interleave the kill/start (SPEC-009 WS-2).
	var dhcpErr error
	if restart {
		dhcpErr = svc.dhcpManager.RestartDHCP(cfg)
	} else {
		dhcpErr = svc.dhcpManager.EnsureDHCP(cfg)
	}
	if dhcpErr != nil {
		log.Warn().Err(dhcpErr).Str("network_id", networkID).Msg("dhcp apply: failed to (re)start DHCP")
	} else {
		log.Info().Str("network_id", networkID).Int("reservations", len(cfg.HostReservations)).Bool("restart", restart).Msg("dhcp apply: server (re)started from DB state")
	}
	// The redirect was asserted before DHCP and stands whether or not dnsmasq came up.
	return bridgeName, bridgeAddr, nil
}

// dhcpReservations builds MAC->IP dnsmasq reservations from the network's ports
// so dnsmasq hands each VM the exact fixed IP neutron allocated to its port
// (SPEC-009 WS-2 / FR-006). The port's fixed_ip in the DB is authoritative.
func (svc *Service) dhcpReservations(ctx context.Context, networkID string) []networking.DHCPHostReservation {
	rows, err := svc.pool.Query(ctx,
		`SELECT mac_address, fixed_ips FROM ports WHERE network_id = $1 AND mac_address <> ''`, networkID)
	if err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("dhcp reservations: query ports failed")
		return nil
	}
	defer rows.Close()

	var res []networking.DHCPHostReservation
	for rows.Next() {
		var mac string
		var fixedIPsRaw []byte
		if err := rows.Scan(&mac, &fixedIPsRaw); err != nil {
			log.Warn().Err(err).Str("operation", "dhcpReservations").Msg("failed to scan row")
			continue
		}
		var fixedIPs []struct {
			IPAddress string `json:"ip_address"`
		}
		if err := json.Unmarshal(fixedIPsRaw, &fixedIPs); err != nil {
			continue
		}
		for _, f := range fixedIPs {
			if f.IPAddress != "" {
				res = append(res, networking.DHCPHostReservation{MAC: mac, IP: f.IPAddress})
				break // pin the port's primary fixed IP
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("dhcp reservations: ports rows error")
	}
	return res
}

// dhcpRange returns a DHCP pool [start,end] that always stays inside the subnet.
// It preserves the conventional .10–.250 window for a /24 and clamps to fit
// smaller prefixes: the start falls back past the network and gateway addresses
// when .10 doesn't fit, and the end is capped at the last usable address
// (broadcast-1). Without this a non-/24 subnet produced a range outside the
// subnet (e.g. .250 in a /26), which dnsmasq rejects (SPEC-009 WS-2).
func dhcpRange(ipNet *net.IPNet) (string, string) {
	last := decrementIP(broadcast(ipNet), 1) // last usable address (broadcast - 1)
	start := incrementIP(ipNet.IP, 10)
	if !ipNet.Contains(start) {
		start = incrementIP(ipNet.IP, 2) // tiny subnet: skip network (.0) and gateway (.1)
	}
	end := incrementIP(ipNet.IP, 250)
	if !ipNet.Contains(end) {
		end = last
	}
	return start.String(), end.String()
}

// bridgeAddrFor returns the address the network's bridge carries for a subnet: the gateway when the
// subnet has one, otherwise the first host address of its CIDR.
//
// The bridge needs an address in the subnet regardless — dnsmasq binds to it and the metadata DNAT
// hangs off it — but for a gatewayless subnet that address is the bridge's own and is never
// advertised as a default route (SPEC-024 FR-023a). Shared by the apply and teardown paths so both
// agree on which address to add and remove.
func bridgeAddrFor(gatewayIP *string, cidr string) string {
	if gatewayIP != nil && *gatewayIP != "" {
		return *gatewayIP
	}
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	return incrementIP(ipNet.IP, 1).String()
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
