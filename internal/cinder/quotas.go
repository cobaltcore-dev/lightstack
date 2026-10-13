package cinder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// cinderQuotaDefaults holds the default block-storage limits used when a project has
// no row in cinder_quotas.
//
// This is the SINGLE source of default limits for the package: the quota-set views and
// the absolute-limits view both resolve through resolveCinderQuotaLimits, so they
// cannot report different values for the same resource (SPEC-020 FR-003). Previously
// the limits handler hardcoded its own values (maxTotalVolumes 1000 against a
// quota-set default of 100) and ignored cinder_quotas entirely, so an admin's quota
// override had no effect on the limits view at all.
var cinderQuotaDefaults = map[string]int{
	"volumes":              100,
	"snapshots":            100,
	"gigabytes":            10000,
	"backups":              100,
	"backup_gigabytes":     10000,
	"per_volume_gigabytes": 1000,
}

// resolveCinderQuotaLimits returns the effective limits for a project: the package
// defaults overlaid with whatever cinder_quotas configures.
func resolveCinderQuotaLimits(ctx context.Context, pool database.DB, projectID string) (map[string]int, error) {
	limits := make(map[string]int, len(cinderQuotaDefaults))
	maps.Copy(limits, cinderQuotaDefaults)

	rows, err := pool.Query(ctx, `
		SELECT resource, "limit"
		FROM cinder_quotas
		WHERE project_id = $1
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query cinder quotas for project %s: %w", projectID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var resource string
		var limit int
		if err := rows.Scan(&resource, &limit); err != nil {
			return nil, fmt.Errorf("scan cinder quota row for project %s: %w", projectID, err)
		}
		limits[resource] = limit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cinder quotas for project %s: %w", projectID, err)
	}
	return limits, nil
}

// cinderQuotaUsage counts the project's block-storage consumption, keyed by the same
// resource names as cinderQuotaDefaults so the two can be zipped. A failed count is
// returned, never reported as zero usage.
func cinderQuotaUsage(ctx context.Context, pool database.DB, projectID string) (map[string]int, error) {
	var volumes, gigabytes, snapshots, snapshotGigabytes, backups, backupGigabytes int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volumes WHERE project_id = $1 AND status != 'deleted'",
		projectID).Scan(&volumes, &gigabytes); err != nil {
		return nil, fmt.Errorf("count volume usage: %w", err)
	}
	// Snapshot SIZE counts toward `gigabytes` as well as the snapshot count
	// (SPEC-025-followup FR-026). Upstream's configuration reference describes the
	// allowance as "total gigabytes for volumes and snapshots", and a snapshot occupies
	// real backend storage — counting only volumes left a project able to exceed its
	// storage allowance without exceeding its quota.
	//
	// This CHANGES A NUMBER USERS SEE: reported usage rises for any project holding
	// snapshots, in the quota-set views, the absolute-limits view and the dashboard's usage
	// bars, and a project with many snapshots can read as over its limit the moment this
	// ships. That is the correct number — it is what the storage actually holds — and it is
	// a release-note item rather than something to smooth over.
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM snapshots WHERE project_id = $1 AND status != 'deleted'",
		projectID).Scan(&snapshots, &snapshotGigabytes); err != nil {
		return nil, fmt.Errorf("count snapshot usage: %w", err)
	}
	gigabytes += snapshotGigabytes
	// volume_backups (migration 022) carries no 'deleted' status — DeleteBackup removes
	// the row — so this counts every row for the project.
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volume_backups WHERE project_id = $1",
		projectID).Scan(&backups, &backupGigabytes); err != nil {
		return nil, fmt.Errorf("count backup usage: %w", err)
	}
	return map[string]int{
		"volumes":          volumes,
		"gigabytes":        gigabytes,
		"snapshots":        snapshots,
		"backups":          backups,
		"backup_gigabytes": backupGigabytes,
	}, nil
}

// GetQuotaSetDefaults returns the default quota limits (code defaults only — no
// cinder_quotas overlay and no usage), from the same source as every other quota
// view so the values cannot drift (FR-003).
func (svc *Service) GetQuotaSetDefaults(c *gin.Context) {
	projectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	quotaSet := gin.H{"id": projectID}
	for resource, limit := range cinderQuotaDefaults {
		quotaSet[resource] = limit
	}
	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// GetQuotaSet returns quota limits for a project, flat by default.
//
// With `?usage=true` each resource becomes {in_use, limit, reserved} instead — the
// Block Storage v3 spelling for the usage view ("Show project's quota usage
// information. Default is false"). Note this is NOT Nova's `/detail` sub-resource:
// block storage has no such path, so `cinder quota-show --usage` and
// `cinderclient.quotas.get(project, usage=True)` both arrive here with a query
// parameter (SPEC-020 FR-002). `reserved` is always 0 — Lightstack completes operations
// synchronously and models no reservations (Art. II).
func (svc *Service) GetQuotaSet(c *gin.Context) {
	targetProjectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// Resolve limits through the shared helper so this view and the absolute-limits
	// view always agree (FR-003).
	quotas, err := resolveCinderQuotaLimits(c.Request.Context(), svc.pool, targetProjectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_quota_set").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get quota set"))
		return
	}

	quotaSet := gin.H{"id": targetProjectID}
	// ParseBool accepts the "True" that python-cinderclient sends as well as "true"
	// and "1"; anything unparseable is the documented default, false.
	if withUsage, _ := strconv.ParseBool(c.Query("usage")); withUsage {
		usage, usageErr := cinderQuotaUsage(c.Request.Context(), svc.pool, targetProjectID)
		if usageErr != nil {
			log.Error().Err(usageErr).Str("project_id", targetProjectID).Msg("failed to query quota usage")
			common.SendError(c, common.NewInternalServerError("failed to get quota usage"))
			return
		}
		for resource, limit := range quotas {
			quotaSet[resource] = gin.H{
				"in_use":   usage[resource],
				"limit":    limit,
				"reserved": 0,
			}
		}
	} else {
		for resource, limit := range quotas {
			quotaSet[resource] = limit
		}
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// UpdateQuotaSet updates quota limits for a project
func (svc *Service) UpdateQuotaSet(c *gin.Context) {
	// Admin-only: quota limits are an operator concern. Checked before the body is
	// bound so a non-admin gets 403, not a request-validation 400 (SPEC-020 FR-020).
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	// Resolved exactly as the reads resolve it, so `default` names the same project on
	// PUT as on GET — the caller's own.
	resolved, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}
	var req struct {
		QuotaSet map[string]any `json:"quota_set"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Update each quota resource
	for resource, limitVal := range req.QuotaSet {
		// Skip non-integer fields like "id"
		if resource == "id" {
			continue
		}

		var limit int
		switch v := limitVal.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		case json.Number:
			// Handle json.Number from any unmarshaling
			val, err := v.Int64()
			if err != nil {
				continue
			}
			limit = int(val)
		default:
			continue
		}

		_, err := svc.pool.Exec(c.Request.Context(), `
			INSERT INTO cinder_quotas (project_id, resource, "limit")
			VALUES ($1, $2, $3)
			ON CONFLICT (project_id, resource)
			DO UPDATE SET "limit" = $3, updated_at = NOW()
		`, resolved, resource, limit)
		if err != nil {
			log.Error().Err(err).Str("operation", "update_quota").Str("resource", resource).Msg("failed to update quota")
			common.SendError(c, common.NewInternalServerError("failed to update quota"))
			return
		}
	}

	// Return the resulting quotas (defaults + overrides) from the shared helper.
	limits, err := resolveCinderQuotaLimits(c.Request.Context(), svc.pool, resolved)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_quota").Msg("failed to read back quotas")
		common.SendError(c, common.NewInternalServerError("failed to read back quotas"))
		return
	}

	quotaSet := make(map[string]any)
	quotaSet["id"] = resolved
	for k, v := range limits {
		quotaSet[k] = v
	}

	c.JSON(http.StatusOK, gin.H{"quota_set": quotaSet})
}

// DeleteQuotaSet resets quotas to defaults for a project
func (svc *Service) DeleteQuotaSet(c *gin.Context) {
	// Admin-only, matching UpdateQuotaSet and upstream Cinder (SPEC-020 FR-020).
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow quota updates to be performed. Admin role required."))
		return
	}

	// Same target resolution as the reads and UpdateQuotaSet.
	targetProjectID, apiErr := common.QuotaTargetProject(c, svc.pool)
	if apiErr != nil {
		common.SendError(c, apiErr)
		return
	}

	// A DELETE matching no rows is not an error: resetting a project that never had an
	// override is a successful no-op, and Exec reports that as a zero row count, never
	// as database.ErrNoRows (that sentinel only comes from QueryRow/Scan).
	if _, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM cinder_quotas WHERE project_id = $1",
		targetProjectID); err != nil {
		log.Error().Err(err).Str("operation", "delete_quota_set").Msg("failed to reset quotas")
		common.SendError(c, common.NewInternalServerError("failed to reset quotas"))
		return
	}

	c.Status(http.StatusNoContent)
}

// Block-storage quota ENFORCEMENT (SPEC-025-followup US3, FR-018 – FR-029a).
//
// Until this feature nothing on any path consulted a quota: the cinder_quotas table
// existed, both quota views and the absolute-limits view reported limits and usage from
// it, and no creation route read them. A single project could exhaust the node's storage
// or a Ceph pool while every quota view reported it comfortably inside its allowance.
//
// TWO THINGS ABOUT THE SHAPE, both of which the spec got wrong before planning read the
// code:
//
//  1. The signature is ctx-based, NOT gin-based. nova.CheckQuota takes a *gin.Context
//     because every compute quota check happens inside a request. Block storage has a
//     caller that does not: compute's boot-from-new-volume path reaches provisionVolume
//     directly through CreateBootVolumeFromImage, with no HTTP request of its own. A
//     gin-shaped check would have been silently bypassed by every server boot.
//  2. Far more routes consume quota than the two the volume API exposes. FIVE of them add
//     a volume — the create handler, compute's boot path, a backup restored into a new
//     volume, a managed-volume import, and the receiving side of a transfer accept — and
//     the rest add snapshots (create and manage), a backup, or gigabytes to an existing
//     volume.
//
//     DO NOT TRUST THAT LIST, OR ANY COUNT. Derive it:
//
//         grep -rn 'withCinderQuota(\|withVolumeQuota(' internal/cinder/ \
//             | grep -v _test | grep -v 'quotas\.go'
//
//     Those two functions are the only way to consume quota in this package, so the grep
//     is exact and stays exact. Excluding this file matters: without it the definitions and
//     withVolumeQuota's own call to withCinderQuota are counted too, which is how a
//     derivation can be as misleading as a stale number.
//
//     A number here would not stay exact. Earlier revisions of this comment said "four
//     routes" and "EIGHT call sites", and both were already wrong when written — the
//     enumeration omitted transfer-accept and managed snapshots, and a route added later
//     and left unchecked reproduces the original defect exactly.
//
// The interior is nova.CheckQuota's, deliberately: one transaction, a row lock on the
// project's quota row, count-then-compare inside it. That is what makes two concurrent
// creations against a single remaining allowance serialise (FR-023).
//
// AND THE LOCK IS HELD ACROSS THE WRITE, not just the check. Every route goes through
// withCinderQuota or withVolumeQuota, which take the write as a closure over the same
// transaction. There is deliberately NO check-only entry point: the first version of this
// package had one, and SEVEN OF THE EIGHT write paths used it — each committing the
// checking transaction, which releases the advisory lock, before writing. Concurrent
// requests then all passed the same check. Branch review measured four-of-four accepted
// against a limit of one on the managed-import and transfer-accept routes, and 12 GiB
// consumed against a 10 GiB allowance on extend. A helper that can be called without a
// write is a helper that will be, so there isn't one.
//
// Eight write paths, nine routes: volume create and compute's boot-from-new-volume share
// provisionVolume, which was the one path that already held the lock correctly.

// QuotaExceededError reports that a request would take a project past a limit. Carries
// the numbers so the caller can name the resource without re-reading them.
type QuotaExceededError struct {
	Resource  string
	Limit     int
	Usage     int
	Requested int
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded for %s: limit %d, in use %d, requested %d",
		e.Resource, e.Limit, e.Usage, e.Requested)
}

// QuotaResource names the exhausted resource.
//
// This exists so compute can recognise a quota refusal WITHOUT importing this package.
// The two services are deliberately decoupled — compute reaches block storage through a
// narrow interface it declares itself — and a type assertion on *QuotaExceededError would
// have coupled them just to distinguish two error kinds. Compute asserts the behaviour
// instead, which is what lets it answer 403 for an exhausted quota where it answers 503
// for a storage backend that is down. Without it, a tenant over quota is told the service
// is unavailable.
func (e *QuotaExceededError) QuotaResource() string { return e.Resource }

// QuotaUsage reports the numbers behind the refusal, for compute's message: what was
// asked for, what is in use and the limit.
func (e *QuotaExceededError) QuotaUsage() (requested, used, limit int) {
	return e.Requested, e.Usage, e.Limit
}

// quotaWant is one resource a request consumes, and how much of it.
//
// A SLICE of these rather than a map, because the order decides which resource a refusal
// names when a request would breach two at once. Map iteration in Go is randomised, so the
// same request would report `snapshots` on one call and `gigabytes` on the next — and a
// client cannot act on a limit that changes name between retries. Callers list the count
// before the size, as upstream's own messages do.
type quotaWant struct {
	resource string
	amount   int
}

// withCinderQuota holds the project's quota lock across the checks AND the write they
// authorise, in one transaction.
//
// This is the package's ONLY way to consume quota, and the reason is the shape it replaced:
// a check-only helper that opened its own transaction, took the lock, compared, and
// committed. Committing released the lock, so the caller's write — always afterwards, and
// in one case across a Ceph round trip — was not serialised against a concurrent request
// that had passed the same check. checkLockedCinderQuota's comment described that failure
// while seven of the eight write paths were doing it.
//
// A project with NO cinder_quotas row runs on the package defaults, which is the common
// case — a fresh deployment configures nothing. That is why the lock is advisory rather
// than a row lock: there is no row to lock, and a guarantee that held only for projects an
// administrator had already configured is one nobody could rely on.
//
// An empty projectID performs the write with no checks: internal callers legitimately have
// no project scope, and refusing them here would break paths that never consumed quota.
func withCinderQuota(ctx context.Context, pool database.DB, projectID string, want []quotaWant, write func(tx database.Tx) error) error {
	return database.WithTx(ctx, pool, func(tx database.Tx) error {
		if projectID != "" {
			if err := lockCinderQuota(ctx, tx, projectID); err != nil {
				return err
			}
			for _, w := range want {
				if err := checkLockedCinderQuota(ctx, tx, projectID, w.resource, w.amount); err != nil {
					return err
				}
			}
		}
		return write(tx)
	})
}

// lockCinderQuota serialises every quota decision for one project for the life of tx.
//
// An ADVISORY lock rather than a row lock, because the row may not exist: a project with
// no cinder_quotas entry runs on the code defaults, which is the common case and the state
// a fresh deployment is in. Locking only the row would give the concurrency guarantee to
// projects an administrator had configured and to nobody else — a guarantee nothing could
// rely on. The lock is transaction-scoped, so it is released on commit or rollback with no
// unlock path to forget.
func lockCinderQuota(ctx context.Context, tx database.Tx, projectID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cinder_quota:"+projectID); err != nil {
		return fmt.Errorf("locking cinder quota for project %s: %w", projectID, err)
	}
	return nil
}

// checkLockedCinderQuota compares usage against the limit inside a transaction whose
// caller already holds the project's lock.
//
// Exported within the package so a caller can hold the lock across the check AND the write
// it authorises. A check that commits before the write releases the lock between them, and
// two concurrent requests then both pass — which is not a theoretical window: two
// goroutines find it every time.
func checkLockedCinderQuota(ctx context.Context, tx database.Tx, projectID, resource string, amount int) error {
	if projectID == "" || amount <= 0 {
		return nil
	}
	limit, err := lockedCinderLimit(ctx, tx, projectID, resource)
	if err != nil {
		return err
	}
	// A negative limit is upstream's spelling for "unlimited".
	if limit < 0 {
		return nil
	}
	usage, err := cinderResourceUsage(ctx, tx, projectID, resource)
	if err != nil {
		return err
	}
	if usage+amount > limit {
		return &QuotaExceededError{Resource: resource, Limit: limit, Usage: usage, Requested: amount}
	}
	return nil
}

// lockedCinderLimit reads the configured limit for one resource inside the transaction,
// falling back to the package default. FOR UPDATE on the row keeps a concurrent
// administrator's quota change from interleaving with a check that already read the old
// value; the advisory lock above covers the no-row case that has nothing to lock.
func lockedCinderLimit(ctx context.Context, tx database.Tx, projectID, resource string) (int, error) {
	var limit int
	err := tx.QueryRow(ctx,
		`SELECT "limit" FROM cinder_quotas WHERE project_id = $1 AND resource = $2 FOR UPDATE`,
		projectID, resource).Scan(&limit)
	if errors.Is(err, database.ErrNoRows) {
		def, ok := cinderQuotaDefaults[resource]
		if !ok {
			return 0, fmt.Errorf("unknown block-storage quota resource: %s", resource)
		}
		return def, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading cinder quota %s for project %s: %w", resource, projectID, err)
	}
	return limit, nil
}

// cinderResourceUsage counts one resource's consumption inside the transaction.
//
// These MUST agree with what cinderQuotaUsage reports (FR-026), because enforcement and
// reporting disagreeing is worse than either being wrong alone: an operator reads one
// number and the product enforces another. `gigabytes` therefore counts volume AND
// snapshot size, matching upstream's "total gigabytes for volumes and snapshots" — the
// reporting side was corrected to match in cinderQuotaUsage.
func cinderResourceUsage(ctx context.Context, tx database.Tx, projectID, resource string) (int, error) {
	var query string
	switch resource {
	case "volumes":
		query = `SELECT COUNT(*) FROM volumes WHERE project_id = $1 AND status != 'deleted'`
	case "gigabytes":
		query = `SELECT COALESCE((SELECT SUM(size_gb) FROM volumes WHERE project_id = $1 AND status != 'deleted'), 0)
		              + COALESCE((SELECT SUM(size_gb) FROM snapshots WHERE project_id = $1 AND status != 'deleted'), 0)`
	case "snapshots":
		query = `SELECT COUNT(*) FROM snapshots WHERE project_id = $1 AND status != 'deleted'`
	case "backups":
		query = `SELECT COUNT(*) FROM volume_backups WHERE project_id = $1`
	case "backup_gigabytes":
		query = `SELECT COALESCE(SUM(size_gb), 0) FROM volume_backups WHERE project_id = $1`
	default:
		return 0, fmt.Errorf("unknown block-storage quota resource: %s", resource)
	}
	var usage int
	if err := tx.QueryRow(ctx, query, projectID).Scan(&usage); err != nil {
		return 0, fmt.Errorf("counting %s usage for project %s: %w", resource, projectID, err)
	}
	return usage, nil
}

// checkPerVolumeCeiling refuses a single request larger than per_volume_gigabytes,
// independently of what the project's total allowance permits (FR-024).
func checkPerVolumeCeiling(ctx context.Context, pool database.DB, projectID string, sizeGB int) error {
	if projectID == "" || sizeGB <= 0 {
		return nil
	}
	limits, err := resolveCinderQuotaLimits(ctx, pool, projectID)
	if err != nil {
		return err
	}
	ceiling, ok := limits["per_volume_gigabytes"]
	if !ok || ceiling < 0 {
		return nil
	}
	if sizeGB > ceiling {
		return &QuotaExceededError{Resource: "per_volume_gigabytes", Limit: ceiling, Usage: 0, Requested: sizeGB}
	}
	return nil
}

// withVolumeQuota is withCinderQuota for the routes that add ONE volume of a given size and
// nothing else: the per-request ceiling, the count and the total gigabytes, with the write.
// Transfer accept adds a volume AND its snapshots, so it applies the same ceiling and puts the
// snapshots into the same withCinderQuota call instead (AcceptVolumeTransfer).
//
// Every route that adds a volume goes through this function or, for a transfer, through the
// same two steps, so none of them can enforce a different subset — and none of them can hold
// the lock for a different span.
//
// The ceiling stays OUTSIDE the transaction on purpose. It compares one request's size
// against one configured maximum, so no concurrent request can change its answer and it
// has nothing to serialise against (FR-024). Keeping it out holds the locked section to
// the two checks that actually count rows.
func withVolumeQuota(ctx context.Context, pool database.DB, projectID string, sizeGB int, write func(tx database.Tx) error) error {
	if err := checkPerVolumeCeiling(ctx, pool, projectID, sizeGB); err != nil {
		return err
	}
	return withCinderQuota(ctx, pool, projectID, []quotaWant{
		{"volumes", 1},
		{"gigabytes", sizeGB},
	}, write)
}

// sendQuotaError maps a quota refusal to the 413 / overLimit answer stock clients already
// understand, and records the denial.
//
// The denial is the one refusal in this feature that IS logged (FR-029a): a project hitting
// its ceiling is actionable by whoever configured the ceiling, and it is invisible to them
// otherwise — the client sees its own error and the operator sees nothing. The other
// refusals this feature introduces are client errors already in the caller's hands, and
// logging them would let any looping client fill the log.
//
// Returns true when err was a quota refusal and a response has been sent.
func sendQuotaError(c *gin.Context, err error) bool {
	var qe *QuotaExceededError
	if !errors.As(err, &qe) {
		return false
	}
	log.Warn().
		Str("event", "quota_denied").
		Str("project_id", c.GetString("project_id")).
		Str("resource", qe.Resource).
		Int("limit", qe.Limit).
		Int("in_use", qe.Usage).
		Int("requested", qe.Requested).
		Msg("block-storage quota denied")
	common.SendError(c, common.NewQuotaExceededError(qe.Resource))
	return true
}
