package neutron

import (
	"context"
	"errors"
	"fmt"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// PurgeRealModeNetworkState removes the real-mode host network state this node
// created — the inverse of the reconciler (SPEC-010 teardown / cloud-storage#472,
// o3k#53). It enumerates from the database (read-only; the DB is operator-owned and
// is never reset by teardown) and calls the existing Remove* helpers, in reverse of
// how the reconciler asserts them:
//
//  1. floating-IP DNAT (+ the /32 on the uplink) and per-VM ingress SG filters
//  2. egress SNAT for internal subnets behind an external-gateway router
//     2b. per-security-group iptables chains (O3K-SG-*), best-effort
//  3. per network: stop dnsmasq, remove the 169.254.169.254 DNAT, delete the bridge
//  4. router namespaces, then tenant/project namespaces
//
// Best-effort per resource (an already-absent rule is fine and not counted); returns
// an error if any removal actually failed, so the blocking teardown hook can
// fail-closed (FR-001/016). Stub mode is a no-op. Networked backends (RBD/S3) are
// never touched (operator-owned, C4). The global net.ipv4.ip_forward sysctl is left
// as-is (host-wide; other workloads depend on it).
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

	uplink, uerr := networking.DefaultUplinkInterface()
	if uerr != nil {
		log.Warn().Err(uerr).Msg("teardown network: no uplink detected — skipping north-south NAT removal")
	}

	if uplink != "" {
		// 1. Floating-IP NAT + per-VM ingress SG filter (inverse of reconcileNAT's FIP loop).
		if rows, err := database.DB.Query(ctx, `
			SELECT floating_ip_address, fixed_ip_address
			FROM floating_ips
			WHERE port_id IS NOT NULL AND fixed_ip_address IS NOT NULL`); err == nil {
			type fip struct{ floating, fixed string }
			var fips []fip
			for rows.Next() {
				var f fip
				if rows.Scan(&f.floating, &f.fixed) == nil && f.floating != "" && f.fixed != "" {
					fips = append(fips, f)
				}
			}
			if e := rows.Err(); e != nil {
				fail(e, "iterate floating IPs")
			}
			rows.Close()
			for _, f := range fips {
				fail(svc.routerManager.WithdrawFloatingIP(f.floating, f.fixed, uplink), "withdraw floating IP")
				fail(svc.routerManager.RemoveIngressFilter(f.fixed, uplink), "remove ingress filter")
			}
		} else {
			fail(err, "query floating IPs")
		}

		// 2. Egress SNAT per internal subnet behind an external-gateway router.
		if rows, err := database.DB.Query(ctx, `
			SELECT DISTINCT s.cidr
			FROM routers rt
			JOIN router_interfaces ri ON ri.router_id = rt.id
			JOIN subnets s ON s.id = ri.subnet_id
			WHERE rt.external_gateway_info IS NOT NULL
			  AND rt.external_gateway_info::text <> 'null'`); err == nil {
			var cidrs []string
			for rows.Next() {
				var c string
				if rows.Scan(&c) == nil && c != "" {
					cidrs = append(cidrs, c)
				}
			}
			if e := rows.Err(); e != nil {
				fail(e, "iterate egress subnets")
			}
			rows.Close()
			for _, c := range cidrs {
				fail(svc.routerManager.RemoveEgressSNAT(c, uplink), "remove egress SNAT")
			}
		} else {
			fail(err, "query egress subnets")
		}
	}

	// 2b. Per-security-group iptables chains (O3K-SG-*, east-west SG in iptables mode).
	// Best-effort: a chain that was never materialized (stub/ebpf) or is still
	// referenced by a port jump is logged, NOT counted — it must not abort the
	// teardown (issue #53 topic 3 / option b). A DB query failure IS a real error.
	if svc.sgManager != nil {
		if rows, err := database.DB.Query(ctx, `SELECT id FROM security_groups`); err == nil {
			var sgs []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil && id != "" {
					sgs = append(sgs, id)
				}
			}
			if e := rows.Err(); e != nil {
				fail(e, "iterate security groups")
			}
			rows.Close()
			for _, id := range sgs {
				if e := svc.sgManager.DeleteSecurityGroupChain(id); e != nil {
					log.Debug().Err(e).Str("sg", id).Msg("teardown network: SG chain absent or still referenced (best-effort)")
				}
			}
		} else {
			fail(err, "query security groups")
		}
	}

	// 3. Per network: stop dnsmasq, remove the metadata DNAT, delete the host bridge.
	if rows, err := database.DB.Query(ctx, `SELECT id FROM networks`); err == nil {
		var netIDs []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && id != "" {
				netIDs = append(netIDs, id)
			}
		}
		if e := rows.Err(); e != nil {
			fail(e, "iterate networks")
		}
		rows.Close()
		for _, id := range netIDs {
			fail(svc.dhcpManager.StopDHCP(id), "stop dnsmasq")
			if len(id) >= 8 {
				bridge := "br-" + id[:8]
				var gw string
				if e := database.DB.QueryRow(ctx,
					`SELECT gateway_ip FROM subnets WHERE network_id = $1 AND enable_dhcp = true LIMIT 1`, id).Scan(&gw); e != nil && !errors.Is(e, pgx.ErrNoRows) {
					fail(e, "query gateway_ip")
				} else if gw != "" {
					fail(svc.mdRedirect.Remove(bridge, gw), "remove metadata DNAT")
				}
				fail(svc.brManager.DeleteBridge(bridge, false, ""), "delete bridge")
			}
		}
	} else {
		fail(err, "query networks")
	}

	// 4. Router namespaces, then tenant/project namespaces.
	if rows, err := database.DB.Query(ctx, `SELECT id FROM routers`); err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && id != "" {
				ids = append(ids, id)
			}
		}
		if e := rows.Err(); e != nil {
			fail(e, "iterate routers")
		}
		rows.Close()
		for _, id := range ids {
			fail(svc.routerManager.DeleteRouterNamespace(id), "delete router namespace")
		}
	} else {
		fail(err, "query routers")
	}
	if rows, err := database.DB.Query(ctx, `SELECT DISTINCT project_id::text FROM networks WHERE project_id IS NOT NULL`); err == nil {
		var pids []string
		for rows.Next() {
			var pid string
			if rows.Scan(&pid) == nil && pid != "" {
				pids = append(pids, pid)
			}
		}
		if e := rows.Err(); e != nil {
			fail(e, "iterate project namespaces")
		}
		rows.Close()
		for _, pid := range pids {
			fail(svc.nsManager.DeleteNamespace(pid), "delete project namespace")
		}
	} else {
		fail(err, "query project namespaces")
	}

	if failures > 0 {
		return fmt.Errorf("network purge: %d removal(s) failed", failures)
	}
	return nil
}
