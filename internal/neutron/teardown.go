package neutron

import (
	"context"
	"fmt"
	"slices"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// PurgeRealModeNetworkState removes the real-mode host network state this node holds — the inverse of
// the reconciler (SPEC-010 teardown / cloud-storage#472, Lightstack#53; SPEC-033 research R48). It
// enumerates from the database (read-only; the DB is operator-owned and is never reset by teardown) and
// from the node, so state whose record is gone goes too:
//
//  1. the node's whole port firewall: every port's chains, the fixed chains, the routers' forward chain
//  2. the retired state of earlier layouts, as reconciliation removes it (FR-044)
//  3. every network's DHCP the records name or the node holds, converged to absent: dnsmasq, its device
//     and namespace
//  4. every router the records name or the node holds, converged to absent: the host's proxy entries and
//     routes, its legs, its namespace
//  5. the networks' bridges, then the project namespaces of older deployments
//
// Metadata listeners are the running server's and go with it. Best-effort per resource (an
// already-absent rule is fine and not counted); returns an error if any removal actually failed, so the
// blocking teardown hook can fail-closed (FR-001/016). Stub mode is a no-op. Networked backends (RBD/S3)
// are never touched (operator-owned, C4). The global net.ipv4.ip_forward sysctl is left as-is
// (host-wide; other workloads depend on it).
func (svc *Service) PurgeRealModeNetworkState(ctx context.Context) error {
	if svc.mode == "stub" {
		return nil
	}

	failures := 0
	fail := func(err error, msg string) {
		if err != nil {
			failures++
			log.Warn().Err(err).Msg("teardown network: " + msg)
		}
	}

	uplink, uerr := svc.uplink()
	if uerr != nil {
		log.Warn().Err(uerr).Msg("teardown network: no uplink detected — skipping the routers' proxy entries")
		uplink = ""
	}

	fail(svc.firewall.RemoveAll(ctx), "remove the port firewall")

	var records networking.RetiredRecords
	var err error
	if records.FloatingIPs, err = queryStrings(ctx, svc.pool, `SELECT floating_ip_address FROM floating_ips`); err != nil {
		fail(err, "enumerate floating IPs")
	}
	if records.SubnetCIDRs, err = queryStrings(ctx, svc.pool, `SELECT cidr FROM subnets`); err != nil {
		fail(err, "enumerate subnets")
	}
	fail(svc.retired.Remove(ctx, uplink, records), "remove the retired state of an earlier layout")

	netIDs, err := queryStrings(ctx, svc.pool, `SELECT id::text FROM networks`)
	fail(err, "enumerate networks")
	held, err := svc.dhcp.HeldNetworks(ctx)
	fail(err, "list the networks' DHCP namespaces")
	for _, id := range heldWithout(slices.Concat(netIDs, held), nil) {
		fail(svc.dhcp.ConvergeDHCP(ctx, id, nil), "remove a network's DHCP")
	}

	routerIDs, err := queryStrings(ctx, svc.pool, `SELECT id::text FROM routers`)
	fail(err, "enumerate routers")
	heldRouters, err := svc.routers.HeldRouters(ctx)
	fail(err, "list the routers' namespaces")
	for _, id := range heldWithout(slices.Concat(routerIDs, heldRouters), nil) {
		fail(svc.routers.ConvergeRouter(ctx, id, nil, uplink), "remove a router")
	}

	bridges, err := svc.brManager.ListManagedBridges()
	fail(err, "list the bridges")
	for _, id := range netIDs {
		bridges = append(bridges, networking.BridgeNameForNetwork(id))
	}
	for _, b := range heldWithout(bridges, nil) {
		fail(svc.brManager.DeleteBridge(b), "delete a bridge")
	}

	if pids, err := queryStrings(ctx, svc.pool, `SELECT DISTINCT project_id::text FROM networks WHERE project_id IS NOT NULL`); err != nil {
		fail(err, "enumerate project namespaces")
	} else {
		for _, pid := range pids {
			fail(svc.nsManager.DeleteNamespace(ctx, pid), "delete project namespace")
		}
	}

	if failures > 0 {
		return fmt.Errorf("network purge: %d removal(s) failed", failures)
	}
	return nil
}

// queryStrings runs a single-column query and returns its non-empty string values.
// A query, scan, or iteration error is returned so the caller can fail-close
// (FR-001/016). Teardown uses it read-only (operator-owned, never reset); it also
// serves any single-column read whose empty values are not wanted.
func queryStrings(ctx context.Context, pool subnetQuerier, query string, args ...any) ([]string, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			log.Warn().Err(err).Str("operation", "queryStrings").Msg("failed to scan row")
			return nil, err
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}
