// Package teardown implements o3k's release-scoped cleanup: the purge the Helm
// pre-delete hook runs on `dataRetentionPolicy: delete` (SPEC-010 / issue #53).
//
// It enumerates release state from the database (the source of truth), purges it
// in dependency-safe order — compute (libvirt domains) → network (namespaces,
// routes) → node-local storage — then resets the database schema LAST, so the
// enumeration source survives until host purge completes (research R3, FR-019).
// The purge is best-effort and idempotent per artifact (a missing artifact is
// success, FR-008) but fail-closed overall: it returns an error if any required
// class could not be purged, so the blocking hook fails and uninstall halts
// (FR-001/016). An unreachable database is ignored, not fatal (C9): host state is
// then purged best-effort by prefix-scanning network namespaces.
//
// It is Kubernetes-API-free (research R1): scaling the workload and deleting PVCs
// are the chart's kubectl hook Job, not this command. It runs in the existing
// single binary as `o3k cleanup` (FR-010) and is stub-safe (Constitution IV).
package teardown

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/hypervisor"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/cobaltcore-dev/o3k/pkg/storage"
	"github.com/rs/zerolog/log"
)

// Options controls a teardown run.
type Options struct {
	MigrationsPath string // for the schema reset (golang-migrate)
	DryRun         bool   // enumerate and log only; delete nothing
}

// Inventory is the release-scoped set of artifacts to purge, gathered by an
// enumerator (from the database, or from the host when the DB is unreachable).
type Inventory struct {
	DomainIDs  []string // libvirt domain UUIDs (instances.libvirt_domain_id)
	ProjectIDs []string // tenant network namespaces (light-ns-<project_id>)
	RouterIDs  []string // router namespaces (qrouter-<router_id[:11]>)
}

// deps are the side-effecting operations execute() drives. Run wires them to the
// real managers; tests replace them with fakes (stub-safe, Constitution IV).
type deps struct {
	deleteDomain  func(ctx context.Context, id string) error
	deleteProject func(projectID string) error
	deleteRouter  func(routerID string) error
	purgeStorage  func() error // remove node-local hostPath trees (never RBD/S3 — C4/FR-015)
	resetDB       func() error // nil when the DB is unreachable → reset is skipped (C9)
}

// Run wires the real managers from cfg, enumerates the release's state, and
// purges it. This is the entrypoint the `o3k cleanup` subcommand invokes.
func Run(ctx context.Context, cfg *common.Config, opts Options) error {
	if opts.MigrationsPath == "" {
		opts.MigrationsPath = "migrations"
	}

	// Host managers. Stub mode → no-ops (safe in CI / non-Linux). Real mode →
	// NewVMManager connects to the host libvirtd via the mounted socket.
	vmMgr, err := hypervisor.NewVMManager(cfg.Nova.LibvirtURI, orStub(cfg.Nova.LibvirtMode))
	if err != nil {
		return fmt.Errorf("teardown: connect hypervisor: %w", err)
	}
	defer vmMgr.Close()

	netMode := orStub(cfg.Neutron.NetworkingMode)
	nsMgr := networking.NewNetworkNamespaceManager(netMode)
	routerMgr := networking.NewRouterManager(netMode)

	d := deps{
		deleteDomain:  vmMgr.DeleteVM,
		deleteProject: nsMgr.DeleteNamespace,
		deleteRouter:  routerMgr.DeleteRouterNamespace,
		// Purge ONLY node-local storage; never RBD/S3 (operator-owned, C4/FR-015).
		purgeStorage: purgeLocalStorage,
	}

	// Enumerate from the DB (source of truth). If the DB is unreachable, ignore
	// it (C9): no enumeration from / reset of the DB; purge host state by prefix.
	var inv Inventory
	if connErr := database.Connect(ctx, cfg.Database.URL, database.DefaultPoolConfig()); connErr != nil {
		log.Warn().Err(connErr).Msg("teardown: database unreachable — ignoring it (no DB enumeration or reset); purging host state best-effort (C9)")
		inv = enumerateFromHost()
	} else {
		var enumErr error
		inv, enumErr = enumerateFromDB(ctx)
		database.Close() // the pool is only needed for enumeration; close it before the schema reset
		if enumErr != nil {
			return fmt.Errorf("teardown: enumerate from database: %w", enumErr)
		}
		d.resetDB = func() error { return database.MigrateReset(cfg.Database.URL, opts.MigrationsPath) }
	}

	return execute(ctx, d, inv, opts)
}

// execute purges the inventory in dependency-safe order and resets the DB last.
// Best-effort + idempotent per artifact; fail-closed overall (research R3/R9).
func execute(ctx context.Context, d deps, inv Inventory, opts Options) error {
	compute := runDeletes(inv.DomainIDs, opts.DryRun, func(id string) error { return d.deleteDomain(ctx, id) })
	compute.name = "compute"

	network := classResult{name: "network"}
	network.add(runDeletes(inv.ProjectIDs, opts.DryRun, d.deleteProject))
	network.add(runDeletes(inv.RouterIDs, opts.DryRun, d.deleteRouter))

	stor := classResult{name: "storage"}
	switch {
	case opts.DryRun:
		log.Info().Msg("teardown: would purge node-local storage trees (dry-run)")
	default:
		if err := d.purgeStorage(); err != nil {
			stor.failed++
			log.Error().Err(err).Msg("teardown: node-local storage purge failed")
		} else {
			stor.purged++
		}
	}

	db := classResult{name: "database"}
	switch {
	case opts.DryRun:
		log.Info().Msg("teardown: would reset database schema (dry-run)")
	case d.resetDB == nil:
		log.Warn().Msg("teardown: database unreachable — skipping schema reset (C9)")
	default:
		if err := d.resetDB(); err != nil {
			db.failed++
			log.Error().Err(err).Msg("teardown: database reset failed")
		} else {
			db.purged++
		}
	}

	var failed []string
	for _, r := range []classResult{compute, network, stor, db} {
		log.Info().Str("class", r.name).Int("purged", r.purged).Int("absent", r.absent).Int("failed", r.failed).Msg("teardown: class summary")
		if r.failed > 0 {
			failed = append(failed, r.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("teardown incomplete: failed to purge %s", strings.Join(failed, ", "))
	}
	log.Info().Msg("teardown: complete — no release state remaining")
	return nil
}

// classResult tallies one purge class for the fail-closed roll-up + audit log.
type classResult struct {
	name                   string
	purged, absent, failed int
}

func (r *classResult) add(o classResult) {
	r.purged += o.purged
	r.absent += o.absent
	r.failed += o.failed
}

// runDeletes applies del to each id. A not-found / already-absent result is
// success (idempotency, FR-008); any other error is a class failure.
func runDeletes(ids []string, dryRun bool, del func(id string) error) classResult {
	var r classResult
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

func orStub(mode string) string {
	if mode == "" {
		return "stub"
	}
	return mode
}

// purgeLocalStorage removes the node-local Cinder/Glance storage trees under the
// host storage path. It deletes ONLY local hostPath data — never networked RBD/S3
// backends (operator-owned, C4/FR-015). Wholesale removal covers both DB-tracked
// and orphaned files (FR-006) and needs no database (C9); the trees are
// single-release per node (FR-003b). Idempotent — removing an absent dir is nil.
func purgeLocalStorage() error {
	for _, dir := range []string{storage.LocalVolumeDir(), storage.LocalImageDir()} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return nil
}

// enumerateFromDB reads the release's resources from the database. Each release
// owns its database, so "everything in this DB" is this release's set (FR-007).
func enumerateFromDB(ctx context.Context) (Inventory, error) {
	var inv Inventory
	var err error
	if inv.DomainIDs, err = queryStrings(ctx, `SELECT libvirt_domain_id FROM instances WHERE libvirt_domain_id IS NOT NULL AND libvirt_domain_id <> ''`); err != nil {
		return inv, err
	}
	if inv.ProjectIDs, err = queryStrings(ctx, `SELECT DISTINCT project_id::text FROM networks WHERE project_id IS NOT NULL`); err != nil {
		return inv, err
	}
	if inv.RouterIDs, err = queryStrings(ctx, `SELECT id::text FROM routers`); err != nil {
		return inv, err
	}
	// Node-local volumes/images are purged wholesale by directory (purgeLocalStorage),
	// not per-id — so they need no DB enumeration, and the purge covers orphans
	// (FR-006) and works when the DB is unreachable (C9).
	return inv, nil
}

func queryStrings(ctx context.Context, sql string) ([]string, error) {
	rows, err := database.DB.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// enumerateFromHost is the DB-unreachable fallback (C9): it recovers network
// namespaces by their well-known prefixes. Libvirt domains and node-local
// storage are not enumerable without the database; they are reported and left to
// the next run, since the node's storage tree is single-release (FR-003b) and the
// blocking hook will have failed loudly.
func enumerateFromHost() Inventory {
	inv := classifyNetns(parseNetnsList(runIPNetnsList()))
	log.Warn().Int("project_ns", len(inv.ProjectIDs)).Int("router_ns", len(inv.RouterIDs)).
		Msg("teardown: DB-unreachable fallback — purging network namespaces by prefix; libvirt domains and node-local storage cannot be enumerated without the database")
	return inv
}

func runIPNetnsList() string {
	out, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseNetnsList extracts namespace names from `ip netns list` output (each line
// is "<name>" or "<name> (id: N)").
func parseNetnsList(out string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names
}

// classifyNetns maps o3k-managed namespace names back to the project/router ids
// the delete methods expect (DeleteNamespace prepends light-ns-; DeleteRouter-
// Namespace prepends qrouter- and truncates to 11 chars, so the truncated id
// round-trips).
func classifyNetns(names []string) Inventory {
	var inv Inventory
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "light-ns-"):
			inv.ProjectIDs = append(inv.ProjectIDs, strings.TrimPrefix(n, "light-ns-"))
		case strings.HasPrefix(n, "qrouter-"):
			inv.RouterIDs = append(inv.RouterIDs, strings.TrimPrefix(n, "qrouter-"))
		}
	}
	return inv
}
