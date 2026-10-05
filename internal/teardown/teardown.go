// Package teardown implements Lightstack's release-scoped cleanup: the purge the Helm
// pre-delete hook runs on `dataRetentionPolicy: delete` (SPEC-010 / issue #53,
// cloud-storage#472).
//
// It removes everything Lightstack generated on the node and leaves the database to its
// operator (the DB is read to enumerate what to purge, but never reset). Order:
//   - compute: destroy + undefine the release's libvirt domains
//   - network: remove all host network state (egress SNAT, floating-IP DNAT + the
//     /32 on the uplink, security-group filters, metadata DNAT, dnsmasq, bridges,
//     namespaces) — delegated to neutron.PurgeRealModeNetworkState
//   - storage: remove every node-local tree under /var/lib/lightstack (images, volumes,
//     instances [VM disks], cloud-init ISOs, dhcp)
//
// Best-effort and idempotent per artifact (a missing artifact is success, FR-008)
// but fail-closed overall: it returns an error if any required class could not be
// purged, so the blocking hook fails and uninstall halts (FR-001/016). An
// unreachable database is ignored, not fatal (C9): host state is then purged
// best-effort (storage trees + a netns prefix scan). It runs in the existing single
// binary as `lightstack cleanup` (FR-010), is Kubernetes-API-free (research R1), and is
// stub-safe (Constitution IV). Networked backends (RBD/S3) and the global
// net.ipv4.ip_forward sysctl are deliberately left alone (operator-owned / host-wide).
package teardown

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Options controls a teardown run.
type Options struct {
	DryRun bool // enumerate and log only; delete nothing
}

// Inventory is the release-scoped compute set. Network purge self-enumerates (via
// the neutron service); storage purge is wholesale by directory.
type Inventory struct {
	DomainIDs []string // libvirt domain UUIDs (instances.libvirt_domain_id)
	enumErr   error    // domain-enumeration error (DB reachable but the query failed) → fail the compute class
}

// deps are the side-effecting operations execute() drives. They are function fields,
// not concrete managers, for two reasons: purgeNetwork has two real implementations
// (neutron.PurgeRealModeNetworkState when the DB is reachable; the netns-prefix
// fallback when it is not, C9), and tests replace all three with fakes so execute()
// runs without libvirt/netlink/DB (the deliberate test seam, Constitution IV).
type deps struct {
	deleteDomain func(ctx context.Context, id string) error
	purgeNetwork func() error // remove all host network state (DB-driven, or netns-prefix fallback)
	purgeStorage func() error // RemoveAll the node-local /var/lib/lightstack trees (never RBD/S3 — C4/FR-015)
}

// Run wires the real managers from cfg, enumerates the release's state, and purges
// it. This is the entrypoint the `lightstack cleanup` subcommand invokes.
func Run(ctx context.Context, cfg *common.Config, opts Options) error {
	// Host hypervisor. Stub mode → no-op; real mode → connects to host libvirtd.
	vmMgr, err := hypervisor.NewVMManager(cfg.Nova.LibvirtURI, cmp.Or(cfg.Nova.LibvirtMode, "stub"))
	if err != nil {
		return fmt.Errorf("teardown: connect hypervisor: %w", err)
	}
	defer vmMgr.Close()

	netMode := cmp.Or(cfg.Neutron.NetworkingMode, "stub")
	d := deps{
		deleteDomain: vmMgr.DeleteVM,
		purgeStorage: purgeLocalStorage,
	}

	// The DB is operator-owned: read it to enumerate, never reset it. If it is
	// unreachable, ignore it (C9) and fall back to host-only cleanup.
	var inv Inventory
	dbReachable := false
	if pool, connErr := database.Connect(ctx, cfg.Database.URL, database.DefaultPoolConfig()); connErr != nil {
		log.Warn().Err(connErr).Msg("teardown: database unreachable — ignoring it (operator-owned); host cleanup is best-effort (C9)")
		nsMgr := networking.NewNetworkNamespaceManager(netMode)
		routerMgr := networking.NewRouterManager(netMode)
		d.purgeNetwork = func() error { return purgeNamespacesByPrefix(nsMgr, routerMgr) }
		// DomainIDs stay empty: without the DB we can't enumerate domains, so compute
		// (libvirt domains) is NOT purged on this path — they are left for the operator
		// (C9, accepted limitation). Storage trees + a netns-prefix scan still run.
	} else {
		dbReachable = true
		defer pool.Close()
		inv.DomainIDs, inv.enumErr = enumerateDomainIDs(ctx, pool)
		neutronSvc := neutron.NewService(pool, netMode, nil)
		d.purgeNetwork = func() error { return neutronSvc.PurgeRealModeNetworkState(ctx) }
	}

	return execute(ctx, d, inv, opts, dbReachable)
}

// execute purges compute → network → storage. Best-effort + idempotent per
// artifact; fail-closed overall (research R3/R9). No database reset (operator-owned).
// dbReachable reflects whether the DB could be enumerated: on the false (C9) path
// only storage + prefixed namespaces are purged, so the closing message is qualified.
func execute(ctx context.Context, d deps, inv Inventory, opts Options, dbReachable bool) error {
	compute := runDeletes("compute", inv.DomainIDs, opts.DryRun, func(id string) error { return d.deleteDomain(ctx, id) })
	if inv.enumErr != nil {
		// DB was reachable but the domain-enumeration query failed: compute could not
		// be completed, so fail the class (fail-closed) rather than report success.
		compute.failed++
		log.Error().Err(inv.enumErr).Msg("teardown: compute enumeration failed")
	}

	network := classResult{name: "network"}
	if opts.DryRun {
		log.Info().Msg("teardown: would purge host network state (dry-run)")
	} else if err := d.purgeNetwork(); err != nil {
		network.failed++
		log.Error().Err(err).Msg("teardown: network purge failed")
	} else {
		network.purged++
	}

	stor := classResult{name: "storage"}
	if opts.DryRun {
		log.Info().Msg("teardown: would purge node-local storage trees (dry-run)")
	} else if err := d.purgeStorage(); err != nil {
		stor.failed++
		log.Error().Err(err).Msg("teardown: node-local storage purge failed")
	} else {
		stor.purged++
	}

	var failed []string
	for _, r := range []classResult{compute, network, stor} {
		log.Info().Str("class", r.name).Int("purged", r.purged).Int("absent", r.absent).Int("failed", r.failed).Msg("teardown: class summary")
		if r.failed > 0 {
			failed = append(failed, r.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("teardown incomplete: failed to purge %s", strings.Join(failed, ", "))
	}
	if dbReachable {
		log.Info().Msg("teardown: complete — no lightstack-generated host state remaining (database left to its operator)")
	} else {
		log.Warn().Msg("teardown: complete (best-effort) — DB unreachable, so storage + prefixed namespaces were purged; host bridges/iptables/domains could not be enumerated and may remain (C9)")
	}
	return nil
}

// classResult tallies one purge class for the fail-closed roll-up + audit log.
type classResult struct {
	name                   string
	purged, absent, failed int
}

// runDeletes applies del to each id. A not-found / already-absent result is success
// (idempotency, FR-008); any other error is a class failure.
func runDeletes(name string, ids []string, dryRun bool, del func(id string) error) classResult {
	r := classResult{name: name}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if dryRun {
			log.Info().Str("id", id).Msg("teardown: would purge (dry-run)")
			r.purged++
			continue
		}
		switch err := del(id); {
		case err == nil:
			r.purged++
		case isAbsent(err):
			r.absent++
		default:
			r.failed++
			log.Error().Err(err).Str("id", id).Msg("teardown: purge failed")
		}
	}
	return r
}

// isAbsent reports whether err indicates the artifact was already gone, which
// teardown treats as success (idempotency, FR-008).
func isAbsent(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "no such") ||
		strings.Contains(s, "does not exist") ||
		strings.Contains(s, "already")
}

// purgeLocalStorage removes every node-local storage tree under /var/lib/lightstack — VM
// disk overlays (instances/), config-drive ISOs (cloud-init/), local Glance images
// (images/), local Cinder volumes (volumes/), dnsmasq state (dhcp/), and the console's
// token projection and guest sockets (console-tokens/, console-sockets/; SPEC-021
// FR-017b — the proxy goes in the same uninstall, and the next start recreates both
// with the right owner). It removes
// ONLY local hostPath data — never networked RBD/S3 backends (operator-owned,
// C4/FR-015). Covers DB-tracked and orphaned files (FR-006) and needs no database
// (C9); the trees are single-release per node (FR-003b). Removing an absent dir is nil.
func purgeLocalStorage() error {
	for _, dir := range []string{
		paths.ImagesDir(), paths.VolumesDir(), paths.InstancesDir(), paths.CloudInitDir(), paths.DHCPDir(),
		paths.ConsoleTokenDir(), paths.ConsoleSocketDir(),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return nil
}

// enumerateDomainIDs returns the libvirt domain UUIDs the release created. A query
// or iteration error is returned (not swallowed) so the caller fails the compute
// class: the DB is reachable here, so a failed enumeration means compute could not
// be completed (fail-closed, FR-001/016).
func enumerateDomainIDs(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT libvirt_domain_id FROM instances WHERE libvirt_domain_id IS NOT NULL AND libvirt_domain_id <> ''`)
	if err != nil {
		return nil, fmt.Errorf("enumerate domains: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var s string
		if e := rows.Scan(&s); e != nil {
			return nil, fmt.Errorf("scan domain id: %w", e)
		}
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids, rows.Err()
}

// purgeNamespacesByPrefix is the DB-unreachable network fallback (C9): it deletes
// Lightstack network namespaces by their well-known prefixes. Host bridges and iptables
// can't be precisely enumerated without the DB and are left for the operator (logged).
func purgeNamespacesByPrefix(nsMgr *networking.NetworkNamespaceManager, routerMgr *networking.RouterManager) error {
	out, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		// Can't enumerate host namespaces → can't run the fallback; fail the network
		// class (fail-closed) rather than silently report a clean purge.
		return fmt.Errorf("namespace fallback: ip netns list: %w", err)
	}
	failures := 0
	for _, n := range parseNetnsList(string(out)) {
		switch {
		case strings.HasPrefix(n, "light-ns-"):
			if err := nsMgr.DeleteNamespace(strings.TrimPrefix(n, "light-ns-")); err != nil {
				failures++
				log.Warn().Err(err).Str("ns", n).Msg("teardown: delete namespace (fallback)")
			}
		case strings.HasPrefix(n, "qrouter-"):
			if err := routerMgr.DeleteRouterNamespace(strings.TrimPrefix(n, "qrouter-")); err != nil {
				failures++
				log.Warn().Err(err).Str("ns", n).Msg("teardown: delete router namespace (fallback)")
			}
		}
	}
	log.Warn().Msg("teardown: DB-unreachable fallback purged namespaces by prefix; host bridges + iptables not enumerable without the DB")
	if failures > 0 {
		return fmt.Errorf("namespace fallback: %d delete(s) failed", failures)
	}
	return nil
}

// parseNetnsList extracts namespace names from `ip netns list` output (each line is
// "<name>" or "<name> (id: N)").
func parseNetnsList(out string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names
}
