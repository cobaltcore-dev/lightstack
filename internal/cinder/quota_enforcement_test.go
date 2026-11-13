package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-025-followup US3: block-storage quota is enforced on every route that adds to a
// project's consumption.
//
// Nothing enforced anything before this feature: the table existed, three views reported
// limits and usage from it, and no creation path read them. A project could exhaust the
// node's storage while every quota view reported it inside its allowance.
//
// The tests are organised by ROUTE rather than by resource, because the defect was never
// "the check is wrong" — it was "this route has no check". Nine sites consume quota across
// four files, and the spec's own enumeration was short twice: it named two volume-adding
// routes, planning found four, and re-grepping the finished code found a ninth
// quota-consuming site in ManageSnapshot.

const (
	quotaProject      = "00000000-0000-0000-0000-0000000000c1"
	quotaOtherProject = "00000000-0000-0000-0000-0000000000c2"
)

// setQuotaLimit configures one limit for the test project, creating the cinder_quotas row.
//
// Worth knowing: the shared database has NO cinder_quotas rows for the default project, so
// limits normally resolve from the code defaults. Setting one here moves the project onto
// the configured path, which is why TestQuotaDefaultsPath exercises the other one
// explicitly rather than assuming both behave alike.
func setQuotaLimit(t *testing.T, resource string, limit int) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO cinder_quotas (project_id, resource, "limit") VALUES ($1, $2, $3)
		 ON CONFLICT (project_id, resource) DO UPDATE SET "limit" = EXCLUDED."limit"`,
		quotaProject, resource, limit); err != nil {
		t.Fatalf("setting %s limit: %v", resource, err)
	}
}

func seedQuotaProject(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for id, name := range map[string]string{quotaProject: "spec025q-a", quotaOtherProject: "spec025q-b"} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO projects (id, name, description, enabled, domain_id)
			 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))
			 ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
			t.Fatalf("seeding project %s: %v", name, err)
		}
	}
	clear := func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM volume_backups WHERE project_id = ANY($1)`, []string{quotaProject, quotaOtherProject})
		_, _ = testPool.Exec(c, `DELETE FROM snapshots WHERE project_id = ANY($1)`, []string{quotaProject, quotaOtherProject})
		_, _ = testPool.Exec(c, `DELETE FROM volumes WHERE project_id = ANY($1)`, []string{quotaProject, quotaOtherProject})
		_, _ = testPool.Exec(c, `DELETE FROM cinder_quotas WHERE project_id = ANY($1)`, []string{quotaProject, quotaOtherProject})
	}
	clear()
	t.Cleanup(clear)
}

// seedVolume inserts a volume directly, so a project can be brought to its limit without
// going through the code under test.
func seedSizedVolume(t *testing.T, project string, sizeGB int) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volumes (id, name, project_id, size_gb, status, created_at, updated_at)
		 VALUES ($1, 'seeded', $2, $3, 'available', NOW(), NOW())`, id, project, sizeGB); err != nil {
		t.Fatalf("seeding volume: %v", err)
	}
	return id
}

func seedSnapshot(t *testing.T, project, volumeID string, sizeGB int) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, created_at)
		 VALUES ($1, 'seeded-snap', $2, $3, $4, 'available', NOW())`, id, volumeID, project, sizeGB); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	return id
}

func countRows(t *testing.T, table, project string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM "+table+" WHERE project_id = $1", project).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// quotaRouter and quotaReq wrap the package's existing newCinderTestRouter / doReq so
// every request in this file is scoped to the quota test project without repeating the
// header map at ~30 call sites.
func quotaRouter(t *testing.T) *gin.Engine {
	t.Helper()
	return newCinderTestRouter()
}

func quotaReq(r *gin.Engine, method, path, body, project string) (int, string) {
	return doReq(r, method, path, body, map[string]string{
		"X-Test-Project": project,
		"X-Test-User":    "00000000-0000-0000-0000-000000000001",
		"X-Test-Roles":   "member",
	})
}

// TestQuotaVolumesCount is T043/T044 for `volumes`: refused at the limit, allowed below,
// and — the assertion that matters — NO VOLUME left behind by the refusal.
func TestQuotaVolumesCount(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 2)
	r := quotaRouter(t)

	// Below the limit: succeeds.
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusAccepted {
		t.Fatalf("first create: code=%d body=%s; want 202", code, body)
	}
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusAccepted {
		t.Fatalf("second create: code=%d body=%s; want 202", code, body)
	}

	// At the limit: refused, 413, naming the resource.
	code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("create at limit: code=%d body=%s; want 413", code, body)
	}
	if !strings.Contains(body, "volumes") {
		t.Errorf("create at limit: body=%s; want the resource named", body)
	}
	if n := countRows(t, "volumes", quotaProject); n != 2 {
		t.Errorf("after a refused create the project has %d volumes; want 2 — a refusal must leave nothing behind", n)
	}
}

// TestQuotaGigabytes is T043/T044 for `gigabytes`, and T045's companion: a request can be
// refused on total size while the volume COUNT still permits another volume.
func TestQuotaGigabytes(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 100)
	setQuotaLimit(t, "gigabytes", 10)
	r := quotaRouter(t)

	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":8}}`, quotaProject); code != http.StatusAccepted {
		t.Fatalf("8G create: code=%d body=%s; want 202", code, body)
	}
	// 5 more would exceed 10 even though the count allows it.
	code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":5}}`, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized create: code=%d body=%s; want 413", code, body)
	}
	if !strings.Contains(body, "gigabytes") {
		t.Errorf("oversized create: body=%s; want gigabytes named", body)
	}
	// 2 more fits exactly — the boundary in the allowed direction (T046).
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":2}}`, quotaProject); code != http.StatusAccepted {
		t.Errorf("exact-fit create: code=%d body=%s; want 202 — the boundary must be inclusive", code, body)
	}
}

// TestQuotaPerVolumeCeiling is T045: FR-024. A single request larger than the per-volume
// ceiling is refused on that ground even when the project's total allowance permits it.
func TestQuotaPerVolumeCeiling(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "gigabytes", 10000)
	setQuotaLimit(t, "per_volume_gigabytes", 5)
	r := quotaRouter(t)

	code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":6}}`, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-ceiling create: code=%d body=%s; want 413", code, body)
	}
	if !strings.Contains(body, "per_volume_gigabytes") {
		t.Errorf("over-ceiling create: body=%s; want per_volume_gigabytes named", body)
	}
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":5}}`, quotaProject); code != http.StatusAccepted {
		t.Errorf("at-ceiling create: code=%d body=%s; want 202", code, body)
	}
}

// TestQuotaDefaultsPath is T047: FR-025. A project with NO cinder_quotas row is enforced
// against the package defaults — the common case, and the one the shared database is
// actually in.
func TestQuotaDefaultsPath(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	// No setQuotaLimit call: the project has no row at all.
	r := quotaRouter(t)

	// The default per-volume ceiling is 1000G, so a larger single volume is refused
	// without any row existing.
	code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1001}}`, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("no-row project, over default ceiling: code=%d body=%s; want 413 — defaults must be enforced too", code, body)
	}
	// And an ordinary volume still succeeds.
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusAccepted {
		t.Errorf("no-row project, normal create: code=%d body=%s; want 202", code, body)
	}
}

// TestQuotaSnapshots is T043 for `snapshots`, plus the snapshot half of FR-026: a snapshot
// consumes both its count and its size.
func TestQuotaSnapshots(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "snapshots", 1)
	setQuotaLimit(t, "gigabytes", 10000)
	vol := seedSizedVolume(t, quotaProject, 4)
	r := quotaRouter(t)

	body := fmt.Sprintf(`{"snapshot":{"volume_id":"%s","name":"s1"}}`, vol)
	if code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/snapshots", body, quotaProject); code != http.StatusAccepted {
		t.Fatalf("first snapshot: code=%d body=%s; want 202", code, resp)
	}
	code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/snapshots", body, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("snapshot at limit: code=%d body=%s; want 413", code, resp)
	}
	if n := countRows(t, "snapshots", quotaProject); n != 1 {
		t.Errorf("after a refused snapshot the project has %d; want 1", n)
	}
}

// TestQuotaSnapshotSizeCountsTowardGigabytes is T053: FR-026. The reported and enforced
// `gigabytes` figure includes snapshot size, matching upstream's "total gigabytes for
// volumes and snapshots". The counter read volume size only.
func TestQuotaSnapshotSizeCountsTowardGigabytes(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	vol := seedSizedVolume(t, quotaProject, 5)
	seedSnapshot(t, quotaProject, vol, 5)

	usage, err := cinderQuotaUsage(context.Background(), testPool, quotaProject)
	if err != nil {
		t.Fatalf("counting usage: %v", err)
	}
	if usage["gigabytes"] != 10 {
		t.Errorf("gigabytes usage = %d; want 10 (a 5G volume plus a 5G snapshot) — snapshot size must count", usage["gigabytes"])
	}

	// And the same figure must reach the absolute-limits view, which reads the same
	// resolution (FR-026a).
	r := quotaRouter(t)
	code, body := quotaReq(r, http.MethodGet, "/v3/"+quotaProject+"/limits", "", quotaProject)
	if code != http.StatusOK {
		t.Fatalf("limits: code=%d body=%s", code, body)
	}
	var resp struct {
		Limits struct {
			Absolute map[string]any `json:"absolute"`
		} `json:"limits"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshalling limits %s: %v", body, err)
	}
	if got, ok := resp.Limits.Absolute["totalGigabytesUsed"]; !ok || int(got.(float64)) != 10 {
		t.Errorf("limits totalGigabytesUsed = %v; want 10 — the corrected count must reach every view", resp.Limits.Absolute["totalGigabytesUsed"])
	}
}

// TestQuotaSnapshotFillsAllowance is T054: a snapshot of a volume whose size alone
// exhausts the remaining gigabytes is refused.
func TestQuotaSnapshotFillsAllowance(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "snapshots", 100)
	setQuotaLimit(t, "gigabytes", 10)
	vol := seedSizedVolume(t, quotaProject, 8)
	r := quotaRouter(t)

	// The volume already uses 8 of 10; a snapshot of it needs another 8.
	body := fmt.Sprintf(`{"snapshot":{"volume_id":"%s","name":"too-big"}}`, vol)
	code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/snapshots", body, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("snapshot over gigabytes: code=%d body=%s; want 413", code, resp)
	}
	if n := countRows(t, "snapshots", quotaProject); n != 0 {
		t.Errorf("a refused snapshot left %d rows; want 0", n)
	}
}

// TestQuotaBackups is T043 for `backups` and `backup_gigabytes`.
func TestQuotaBackups(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "backups", 1)
	setQuotaLimit(t, "backup_gigabytes", 10000)
	vol := seedSizedVolume(t, quotaProject, 2)
	r := quotaRouter(t)

	body := fmt.Sprintf(`{"backup":{"volume_id":"%s"}}`, vol)
	if code, resp := quotaReq(r, http.MethodPost, "/v3/backups", body, quotaProject); code != http.StatusAccepted {
		t.Fatalf("first backup: code=%d body=%s; want 202", code, resp)
	}
	code, resp := quotaReq(r, http.MethodPost, "/v3/backups", body, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("backup at limit: code=%d body=%s; want 413", code, resp)
	}
	if n := countRows(t, "volume_backups", quotaProject); n != 1 {
		t.Errorf("after a refused backup the project has %d; want 1", n)
	}
}

// TestQuotaBackupGigabytes pins the backup size allowance separately from the count, so a
// project with room for another backup but not its bytes is refused.
func TestQuotaBackupGigabytes(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "backups", 100)
	setQuotaLimit(t, "backup_gigabytes", 5)
	vol := seedSizedVolume(t, quotaProject, 6)
	r := quotaRouter(t)

	body := fmt.Sprintf(`{"backup":{"volume_id":"%s"}}`, vol)
	code, resp := quotaReq(r, http.MethodPost, "/v3/backups", body, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("backup over backup_gigabytes: code=%d body=%s; want 413", code, resp)
	}
	if !strings.Contains(resp, "backup_gigabytes") {
		t.Errorf("backup refusal body=%s; want backup_gigabytes named", resp)
	}
}

// TestQuotaExtendChargesTheDelta is T052: FR-028. Extension consumes the gigabytes it
// ADDS. Charging the resulting size would double-count what the volume already holds and
// refuse extensions that fit.
func TestQuotaExtendChargesTheDelta(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "gigabytes", 10)
	vol := seedSizedVolume(t, quotaProject, 8)
	r := quotaRouter(t)

	// 8 → 10 adds 2, which fits the remaining 2. Charging the RESULT would see 10 against
	// a used 8 and refuse.
	code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes/"+vol+"/action",
		`{"os-extend":{"new_size":10}}`, quotaProject)
	if code != http.StatusAccepted {
		t.Errorf("extend within allowance: code=%d body=%s; want 202 — the delta fits", code, resp)
	}

	// 10 → 12 adds 2 more, which does not fit.
	code, resp = quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes/"+vol+"/action",
		`{"os-extend":{"new_size":12}}`, quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("extend past allowance: code=%d body=%s; want 413", code, resp)
	}
	var size int
	if err := testPool.QueryRow(context.Background(),
		"SELECT size_gb FROM volumes WHERE id = $1", vol).Scan(&size); err != nil {
		t.Fatalf("reading size: %v", err)
	}
	if size != 10 {
		t.Errorf("size after a refused extend = %d; want 10 unchanged", size)
	}
}

// TestQuotaTransferAcceptEnforced is T051: FR-019 and FR-019a. Accepting a transfer
// provisions nothing, yet the accepting project's usage rises — and a refusal must leave
// the transfer ACCEPTABLE, since there is nothing to roll back but plenty to lose.
func TestQuotaTransferAcceptEnforced(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 1)
	// The accepting project is already at its one-volume limit.
	seedSizedVolume(t, quotaProject, 1)
	// A volume in the other project, offered for transfer.
	vol := seedSizedVolume(t, quotaOtherProject, 1)

	transferID := uuid.NewString()
	authKey := "secret-key"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volume_transfers (id, volume_id, name, auth_key, source_project_id, accepted, created_at)
		 VALUES ($1, $2, 'xfer', $3, $4, false, NOW())`,
		transferID, vol, authKey, quotaOtherProject); err != nil {
		t.Skipf("volume_transfers not usable in this schema: %v", err)
	}
	markAwaitingTransfer(t, vol)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_transfers WHERE id = $1", transferID)
	})
	r := quotaRouter(t)

	code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/os-volume-transfer/"+transferID+"/accept",
		fmt.Sprintf(`{"accept":{"auth_key":"%s"}}`, authKey), quotaProject)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("accept at limit: code=%d body=%s; want 413", code, resp)
	}

	// The volume must still belong to the SENDER, and the transfer must still be pending.
	var owner string
	if err := testPool.QueryRow(context.Background(),
		"SELECT project_id::text FROM volumes WHERE id = $1", vol).Scan(&owner); err != nil {
		t.Fatalf("reading owner: %v", err)
	}
	if owner != quotaOtherProject {
		t.Errorf("volume owner after a refused accept = %s; want the sender %s — nothing may be lost", owner, quotaOtherProject)
	}
	var accepted bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT accepted FROM volume_transfers WHERE id = $1", transferID).Scan(&accepted); err != nil {
		t.Fatalf("reading transfer: %v", err)
	}
	if accepted {
		t.Error("a refused accept consumed the transfer; it must stay pending so it can be retried")
	}
	var status string
	if err := testPool.QueryRow(context.Background(),
		"SELECT status FROM volumes WHERE id = $1", vol).Scan(&status); err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != "awaiting-transfer" {
		t.Errorf("volume status after a refused accept = %q; want it still awaiting transfer, or the retry cannot succeed", status)
	}

	// Raise the limit and the same transfer is acceptable — the retry path (T051).
	setQuotaLimit(t, "volumes", 5)
	if code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/os-volume-transfer/"+transferID+"/accept",
		fmt.Sprintf(`{"accept":{"auth_key":"%s"}}`, authKey), quotaProject); code != http.StatusAccepted {
		t.Errorf("accept after raising the limit: code=%d body=%s; want 202", code, resp)
	}
}

// TestQuotaImmediateEffect is T057: FR-027. A limit raised by an administrator applies to
// the next request, with no restart — the check must not cache the limit.
func TestQuotaImmediateEffect(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 1)
	r := quotaRouter(t)

	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusAccepted {
		t.Fatalf("first create: code=%d body=%s", code, body)
	}
	if code, _ := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second create: code=%d; want 413", code)
	}
	setQuotaLimit(t, "volumes", 5)
	if code, body := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusAccepted {
		t.Errorf("create after raising the limit: code=%d body=%s; want 202 with no restart", code, body)
	}
}

// TestQuotaExistingOverageIsKept is T058: a project already past its allowance keeps what
// it has; further additions are refused and nothing is deleted.
func TestQuotaExistingOverageIsKept(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	seedSizedVolume(t, quotaProject, 1)
	seedSizedVolume(t, quotaProject, 1)
	seedSizedVolume(t, quotaProject, 1)
	setQuotaLimit(t, "volumes", 1) // already over
	r := quotaRouter(t)

	if code, _ := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject); code != http.StatusRequestEntityTooLarge {
		t.Errorf("create while over the limit: code=%d; want 413", code)
	}
	if n := countRows(t, "volumes", quotaProject); n != 3 {
		t.Errorf("the project has %d volumes; want its 3 kept — enforcement must not delete anything", n)
	}
}

// TestQuotaConcurrentCreates is T056: FR-023. Two simultaneous creations against a single
// remaining allowance must yield exactly one volume.
func TestQuotaConcurrentCreates(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 1)
	r := quotaRouter(t)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":1}}`, quotaProject)
		}(i)
	}
	wg.Wait()

	accepted := 0
	for _, code := range codes {
		if code == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("concurrent creates against one remaining allowance: %d accepted (codes %v); want exactly 1", accepted, codes)
	}
	if n := countRows(t, "volumes", quotaProject); n != 1 {
		t.Errorf("the project has %d volumes after two concurrent creates; want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: the lock must be held across the check AND the write it authorises.
//
// TestQuotaConcurrentCreates above pins this for ONE path — the volume create path
// through provisionVolume, which is the only site that ever held the lock across its
// write. Branch review found the other SEVEN released it: the check-only helper opened its
// own transaction, took pg_advisory_xact_lock, compared and COMMITTED, after which the
// caller wrote outside any transaction. checkLockedCinderQuota's own comment described
// that as the defect it exists to prevent.
//
// Eight write paths, nine routes — volume create and compute's boot path share
// provisionVolume. The package's own comments count routes, which is why "nine" and
// "eight" both appear in them and mean different things.
//
// A test per route, because the defect is per route. `accepted` counts 202s; the row/size
// assertion is the one that matters, since it holds even if the HTTP codes race.

// concurrentQuotaReqs fires n identical requests at once and reports how many were
// accepted. admin marks the caller's role: os-volume-manage requires an admin
// while still consuming the target project's quota, so its test
// drives the route as an administrator.
func concurrentQuotaReqs(r *gin.Engine, n int, method, path, body string) []int {
	return concurrentQuotaReqsAs(r, n, method, path, body, "member")
}

func concurrentQuotaReqsAs(r *gin.Engine, n int, method, path, body, roles string) []int {
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = doReq(r, method, path, body, map[string]string{
				"X-Test-Project": quotaProject,
				"X-Test-User":    "00000000-0000-0000-0000-000000000001",
				"X-Test-Roles":   roles,
			})
		}(i)
	}
	wg.Wait()
	return codes
}

func countAccepted(codes []int) int {
	n := 0
	for _, c := range codes {
		if c == http.StatusAccepted {
			n++
		}
	}
	return n
}

func sumVolumeGigabytes(t *testing.T, project string) int {
	t.Helper()
	var total int
	if err := testPool.QueryRow(context.Background(),
		"SELECT COALESCE(SUM(size_gb), 0) FROM volumes WHERE project_id = $1", project).Scan(&total); err != nil {
		t.Fatalf("summing volume size: %v", err)
	}
	return total
}

// TestQuotaConcurrentSnapshots is the widest of these windows: between the check and the
// INSERT the handler makes a Ceph CreateSnapshot call, so the two requests are unserialised
// across a network round trip.
func TestQuotaConcurrentSnapshots(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "snapshots", 1)
	setQuotaLimit(t, "gigabytes", 10000)
	vol := seedSizedVolume(t, quotaProject, 1)
	r := quotaRouter(t)

	body := fmt.Sprintf(`{"snapshot":{"volume_id":"%s","name":"race"}}`, vol)
	codes := concurrentQuotaReqs(r, 4, http.MethodPost, "/v3/"+quotaProject+"/snapshots", body)

	if n := countAccepted(codes); n != 1 {
		t.Errorf("concurrent snapshots against one remaining allowance: %d accepted (codes %v); want exactly 1", n, codes)
	}
	if n := countRows(t, "snapshots", quotaProject); n != 1 {
		t.Errorf("the project has %d snapshots after concurrent creates; want 1 — the limit is 1", n)
	}
}

// TestQuotaConcurrentBackups pins the same for the backup count.
func TestQuotaConcurrentBackups(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "backups", 1)
	setQuotaLimit(t, "backup_gigabytes", 10000)
	vol := seedSizedVolume(t, quotaProject, 1)
	r := quotaRouter(t)

	// NOTE the path: backups are routed at /v3/backups, with no project segment
	// (volumes.go:137). The project-prefixed alias every other collection has does not
	// exist here, which is a separate pre-existing gap — `openstack volume backup create`
	// gets a 404. Using the prefixed form makes this test pass against nothing.
	body := fmt.Sprintf(`{"backup":{"volume_id":"%s","name":"race"}}`, vol)
	codes := concurrentQuotaReqs(r, 4, http.MethodPost, "/v3/backups", body)

	if n := countAccepted(codes); n != 1 {
		t.Errorf("concurrent backups against one remaining allowance: %d accepted (codes %v); want exactly 1", n, codes)
	}
	if n := countRows(t, "volume_backups", quotaProject); n != 1 {
		t.Errorf("the project has %d backups after concurrent creates; want 1", n)
	}
}

// TestQuotaConcurrentExtends needs several volumes rather than several requests against
// one, because an extend writes an ABSOLUTE size: two concurrent extends of the same
// volume to the same target both leave it at that target, and nothing is over-consumed.
//
// Four volumes of 2 GiB against a 10 GiB allowance leaves 2 GiB spare, so exactly one
// +2 extend fits. Serialised, the second sees usage 10 and is refused; concurrently, all
// four read usage 8 and pass.
func TestQuotaConcurrentExtends(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "gigabytes", 10)
	vols := make([]string, 4)
	for i := range vols {
		vols[i] = seedSizedVolume(t, quotaProject, 2)
	}
	r := quotaRouter(t)

	codes := make([]int, len(vols))
	var wg sync.WaitGroup
	for i, v := range vols {
		wg.Add(1)
		go func(i int, v string) {
			defer wg.Done()
			codes[i], _ = quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes/"+v+"/action",
				`{"os-extend":{"new_size":4}}`, quotaProject)
		}(i, v)
	}
	wg.Wait()

	if n := countAccepted(codes); n != 1 {
		t.Errorf("concurrent extends against 2 GiB of headroom: %d accepted (codes %v); want exactly 1", n, codes)
	}
	if total := sumVolumeGigabytes(t, quotaProject); total > 10 {
		t.Errorf("the project holds %d GiB after concurrent extends; the limit is 10 — quota was exceeded", total)
	}
}

// TestQuotaConcurrentManagedVolumes pins the route that imports an existing backing store,
// and with it the old checkVolumeQuota: that helper ran the ceiling read, the count check
// and the gigabytes check as three separate round trips under two separate lock
// acquisitions, so its own three parts were not atomic with each other either.
// The caller is an administrator because os-volume-manage requires the
// upstream volume_admin_actions:manage policy while the import still
// consumes the target project's quota.
func TestQuotaConcurrentManagedVolumes(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 1)
	setQuotaLimit(t, "gigabytes", 10000)
	r := quotaRouter(t)

	body := `{"volume":{"host":"h1","ref":{"source-name":"race"},"name":"race"}}`
	codes := concurrentQuotaReqsAs(r, 4, http.MethodPost, "/v3/"+quotaProject+"/os-volume-manage", body, "admin")

	if n := countAccepted(codes); n != 1 {
		t.Errorf("concurrent managed imports against one remaining allowance: %d accepted (codes %v); want exactly 1", n, codes)
	}
	if n := countRows(t, "volumes", quotaProject); n != 1 {
		t.Errorf("the project has %d volumes after concurrent imports; want 1", n)
	}
}

// TestQuotaConcurrentTransferAccepts pins the receiving side. Accepting provisions
// nothing, so the write is an ownership UPDATE — but the accepting project's usage rises
// all the same, and two accepts racing past one remaining allowance acquire two volumes.
func TestQuotaConcurrentTransferAccepts(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 1)
	setQuotaLimit(t, "gigabytes", 10000)

	type xfer struct{ id, key string }
	xfers := make([]xfer, 4)
	for i := range xfers {
		vol := seedSizedVolume(t, quotaOtherProject, 1)
		id, key := uuid.NewString(), fmt.Sprintf("key-%d", i)
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO volume_transfers (id, volume_id, name, auth_key, source_project_id, accepted, created_at)
			 VALUES ($1, $2, 'xfer', $3, $4, false, NOW())`,
			id, vol, key, quotaOtherProject); err != nil {
			t.Skipf("volume_transfers not usable in this schema: %v", err)
		}
		markAwaitingTransfer(t, vol)
		xfers[i] = xfer{id, key}
	}
	t.Cleanup(func() {
		for _, x := range xfers {
			_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_transfers WHERE id = $1", x.id)
		}
	})
	r := quotaRouter(t)

	codes := make([]int, len(xfers))
	var wg sync.WaitGroup
	for i, x := range xfers {
		wg.Add(1)
		go func(i int, x xfer) {
			defer wg.Done()
			codes[i], _ = quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/os-volume-transfer/"+x.id+"/accept",
				fmt.Sprintf(`{"accept":{"auth_key":"%s"}}`, x.key), quotaProject)
		}(i, x)
	}
	wg.Wait()

	if n := countAccepted(codes); n != 1 {
		t.Errorf("concurrent transfer accepts against one remaining allowance: %d accepted (codes %v); want exactly 1", n, codes)
	}
	if n := countRows(t, "volumes", quotaProject); n != 1 {
		t.Errorf("the accepting project holds %d volumes after concurrent accepts; the limit is 1", n)
	}
}

// ---------------------------------------------------------------------------
// Two gaps branch review found at 0% (SRC-4).

// TestQuotaUnlimitedLimitIsHonoured pins upstream's spelling for "no limit": a NEGATIVE
// limit, not a large one and not a missing row.
//
// The branch that recognises it had no test, and getting it wrong is not a small error
// in either direction — read as a literal ceiling, -1 refuses every request; ignored
// entirely, an administrator who set "unlimited" gets the package default instead.
func TestQuotaUnlimitedLimitIsHonoured(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", -1)
	setQuotaLimit(t, "gigabytes", -1)
	// Already well past any default allowance.
	for i := 0; i < 3; i++ {
		seedSizedVolume(t, quotaProject, 500)
	}
	r := quotaRouter(t)

	code, resp := quotaReq(r, http.MethodPost, "/v3/"+quotaProject+"/volumes", `{"volume":{"size":100}}`, quotaProject)
	if code != http.StatusAccepted {
		t.Errorf("create against an unlimited quota: code=%d body=%s; want 202 — a negative limit means no limit", code, resp)
	}
}

// TestQuotaExceededErrorMessage covers the error string an operator reads.
//
// It carries the four numbers that make a denial actionable. sendQuotaError logs them
// separately, so nothing exercised Error() itself — and an Error() that dropped the
// resource name would leave the wrapped-error form of the message useless.
func TestQuotaExceededErrorMessage(t *testing.T) {
	err := &QuotaExceededError{Resource: "gigabytes", Limit: 10, Usage: 8, Requested: 5}
	msg := err.Error()
	for _, want := range []string{"gigabytes", "10", "8", "5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q; must name %q so the denial is actionable from the message alone", msg, want)
		}
	}
	if err.QuotaResource() != "gigabytes" {
		t.Errorf("QuotaResource() = %q; want the resource name", err.QuotaResource())
	}
}
