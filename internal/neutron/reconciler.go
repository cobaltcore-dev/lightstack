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

// reconcileSubnets re-spawns the DHCP server for each DHCP-enabled subnet that
// is not currently running — this is the part that does not survive a pod
// restart, and the IsRunning guard makes the periodic pass idempotent while
// still self-healing a dnsmasq that was killed out-of-band (SC-010).
func (r *NetworkReconciler) reconcileSubnets(ctx context.Context) {
	rows, err := database.DB.Query(ctx, `
		SELECT network_id, project_id, cidr, gateway_ip, dns_nameservers
		FROM subnets WHERE enable_dhcp = true`)
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to query subnets")
		return
	}

	type subnetRow struct {
		networkID, projectID, cidr, gatewayIP string
		dns                                   []string
	}
	var subnets []subnetRow
	for rows.Next() {
		var s subnetRow
		if err := rows.Scan(&s.networkID, &s.projectID, &s.cidr, &s.gatewayIP, &s.dns); err == nil {
			subnets = append(subnets, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("reconcile: subnets rows error")
	}

	for _, s := range subnets {
		if r.svc.dhcpManager.IsRunning(s.networkID) {
			continue // already serving — idempotent
		}
		_, ipNet, perr := net.ParseCIDR(s.cidr)
		if perr != nil {
			log.Warn().Err(perr).Str("network_id", s.networkID).Str("cidr", s.cidr).Msg("reconcile: bad subnet CIDR; skipping")
			continue
		}
		bridgeName := "br-" + s.networkID[:8]
		// Bridge is in the default namespace; give it the gateway IP and run
		// dnsmasq there too so it has an address in the subnet to serve.
		ones, _ := ipNet.Mask.Size()
		if err := r.svc.brManager.EnsureBridgeIP(bridgeName, fmt.Sprintf("%s/%d", s.gatewayIP, ones)); err != nil {
			log.Debug().Err(err).Str("bridge", bridgeName).Msg("reconcile: bridge IP ensure")
		}
		cfg := networking.DHCPConfig{
			NetworkID:      s.networkID,
			BridgeName:     bridgeName,
			SubnetCIDR:     s.cidr,
			GatewayIP:      s.gatewayIP,
			DNSServers:     s.dns,
			DHCPRangeStart: incrementIP(ipNet.IP, 10).String(),
			DHCPRangeEnd:   incrementIP(ipNet.IP, 250).String(),
			LeaseTime:      "24h",
		}
		cfg.HostReservations = r.portReservations(ctx, s.networkID)
		if err := r.svc.dhcpManager.StartDHCP(cfg, ""); err != nil {
			log.Warn().Err(err).Str("network_id", s.networkID).Msg("reconcile: failed to (re)start DHCP")
		} else {
			log.Info().Str("network_id", s.networkID).Int("reservations", len(cfg.HostReservations)).Msg("reconcile: DHCP (re)started from DB state")
		}
	}
}

// portReservations builds MAC->IP dnsmasq reservations from the network's ports
// so a (re)started dnsmasq hands each VM the same IP its port already holds
// (SPEC-009 WS-2 / FR-006). The port's fixed_ip in the DB is authoritative.
func (r *NetworkReconciler) portReservations(ctx context.Context, networkID string) []networking.DHCPHostReservation {
	rows, err := database.DB.Query(ctx,
		`SELECT mac_address, fixed_ips FROM ports WHERE network_id = $1 AND mac_address <> ''`, networkID)
	if err != nil {
		log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: failed to query ports for DHCP reservations")
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
		log.Warn().Err(err).Str("network_id", networkID).Msg("reconcile: ports rows error")
	}
	return res
}
