package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/rs/zerolog/log"
)

// NetworkReconciler rebuilds real-mode network state from the database on
// startup and periodically, so VM networking survives an O3K control-plane
// restart/upgrade (SPEC-009 WS-2). On a pod restart the kernel objects
// (bridges, netns, taps, iptables) persist, but dnsmasq is an O3K child process
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

// Stop ends the reconcile loop.
func (r *NetworkReconciler) Stop() { close(r.stopChan) }

func (r *NetworkReconciler) reconcile(ctx context.Context) {
	r.reconcileNetworks(ctx)
	r.reconcileSubnets(ctx)
}

// reconcileNetworks ensures each network's bridge exists. Best-effort: bridges
// are kernel objects that normally persist across a pod restart, so an
// "already exists" outcome is expected and logged at debug.
func (r *NetworkReconciler) reconcileNetworks(ctx context.Context) {
	rows, err := database.DB.Query(ctx, `SELECT id FROM networks`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query networks")
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("reconcile: networks rows error")
	}

	for _, networkID := range ids {
		bridgeName := "br-" + networkID[:8]
		if err := r.svc.brManager.CreateBridge(bridgeName, false, ""); err != nil {
			log.Debug().Err(err).Str("network_id", networkID).Str("bridge", bridgeName).Msg("reconcile: bridge ensure (likely already present)")
		}
	}
}

// reconcileSubnets re-applies DHCP for every network that has a DHCP-enabled
// subnet but is not currently serving — the part that does not survive a pod
// restart. It delegates to applyDHCPForNetwork(restart=false), which is
// idempotent (IsRunning guard) while still self-healing a dnsmasq that was
// killed out-of-band (SC-010) and re-asserting the bridge IP + metadata DNAT
// every pass (SPEC-009 WS-4 / T029).
func (r *NetworkReconciler) reconcileSubnets(ctx context.Context) {
	rows, err := database.DB.Query(ctx,
		`SELECT DISTINCT network_id FROM subnets WHERE enable_dhcp = true`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query subnets")
		return
	}
	var networkIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			networkIDs = append(networkIDs, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("reconcile: subnets rows error")
	}
	for _, networkID := range networkIDs {
		r.svc.applyDHCPForNetwork(ctx, networkID, false)
	}
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
func (svc *Service) applyDHCPForNetwork(ctx context.Context, networkID string, restart bool) {
	if svc.mode == "stub" {
		return
	}
	var cidr, gatewayIP string
	var dns []string
	if err := database.DB.QueryRow(ctx, `
		SELECT cidr, gateway_ip, dns_nameservers FROM subnets
		WHERE network_id = $1 AND enable_dhcp = true LIMIT 1`, networkID).Scan(&cidr, &gatewayIP, &dns); err != nil {
		return // no DHCP-enabled subnet for this network — nothing to do
	}
	_, ipNet, perr := net.ParseCIDR(cidr)
	if perr != nil {
		log.Warn().Err(perr).Str("network_id", networkID).Str("cidr", cidr).Msg("dhcp apply: bad subnet CIDR; skipping")
		return
	}
	bridgeName := "br-" + networkID[:8]
	// Bridge is in the default namespace; give it the gateway IP (so dnsmasq has
	// an address in the subnet to serve) and the 169.254.169.254 DNAT.
	ones, _ := ipNet.Mask.Size()
	if err := svc.brManager.EnsureBridgeIP(bridgeName, fmt.Sprintf("%s/%d", gatewayIP, ones)); err != nil {
		log.Debug().Err(err).Str("bridge", bridgeName).Msg("dhcp apply: bridge IP ensure")
	}
	if err := svc.mdRedirect.Ensure(bridgeName, gatewayIP); err != nil {
		log.Debug().Err(err).Str("bridge", bridgeName).Msg("dhcp apply: metadata redirect ensure")
	}

	if !restart && svc.dhcpManager.IsRunning(networkID) {
		return // already serving — idempotent
	}

	dhcpStart, dhcpEnd := dhcpRange(ipNet)
	cfg := networking.DHCPConfig{
		NetworkID:        networkID,
		BridgeName:       bridgeName,
		SubnetCIDR:       cidr,
		GatewayIP:        gatewayIP,
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
		dhcpErr = svc.dhcpManager.RestartDHCP(cfg, "")
	} else {
		dhcpErr = svc.dhcpManager.StartDHCP(cfg, "")
	}
	if dhcpErr != nil {
		log.Warn().Err(dhcpErr).Str("network_id", networkID).Msg("dhcp apply: failed to (re)start DHCP")
	} else {
		log.Info().Str("network_id", networkID).Int("reservations", len(cfg.HostReservations)).Bool("restart", restart).Msg("dhcp apply: server (re)started from DB state")
	}
}

// dhcpReservations builds MAC->IP dnsmasq reservations from the network's ports
// so dnsmasq hands each VM the exact fixed IP neutron allocated to its port
// (SPEC-009 WS-2 / FR-006). The port's fixed_ip in the DB is authoritative.
func (svc *Service) dhcpReservations(ctx context.Context, networkID string) []networking.DHCPHostReservation {
	rows, err := database.DB.Query(ctx,
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
