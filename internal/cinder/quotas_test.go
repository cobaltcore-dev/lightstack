package cinder

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-020 (cloud-storage#533) — block-storage metadata Horizon's Create Volume
// dialog loads while it is being constructed, plus quota-write authorization.

func cinderQuotaSet(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		QuotaSet map[string]any `json:"quota_set"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse quota_set: %v; body=%s", err, body)
	}
	return env.QuotaSet
}

func cinderAbsolute(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		Limits struct {
			Absolute map[string]any `json:"absolute"`
		} `json:"limits"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse limits: %v; body=%s", err, body)
	}
	return env.Limits.Absolute
}

func cinderNum(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing", key)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("key %q = %#v; want a number", key, v)
	}
	return f
}

// TestListExtensions advertises AvailabilityZones. Horizon's volume-create form
// calls extension_supported() while building the dialog; this endpoint returned 404
// before the fix, so the form raised and the modal never opened (F8). No DB required.
func TestListExtensions(t *testing.T) {
	r := newCinderTestRouter()
	code, body := doReq(r, http.MethodGet, "/v3/extensions", "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("GET /v3/extensions: code=%d body=%s; want 200", code, body)
	}
	var env struct {
		Extensions []struct {
			Alias string `json:"alias"`
			Name  string `json:"name"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse extensions: %v; body=%s", err, body)
	}
	for _, e := range env.Extensions {
		if e.Name == "AvailabilityZones" {
			return
		}
	}
	t.Errorf("extensions do not advertise AvailabilityZones; got %+v", env.Extensions)
}

// TestCinderLimitsAgreeWithQuotaSet is the FR-003 guard. Before the fix the limits
// view hardcoded maxTotalVolumes=1000 while the quota-set defaulted to 100, and it
// ignored cinder_quotas entirely.
func TestCinderLimitsAgreeWithQuotaSet(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	code, limBody := doReq(r, http.MethodGet, "/v3/limits", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("limits: code=%d body=%s", code, limBody)
	}
	code, qsBody := doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("quota-set: code=%d body=%s", code, qsBody)
	}

	abs, qs := cinderAbsolute(t, limBody), cinderQuotaSet(t, qsBody)
	for _, pair := range []struct{ limitKey, quotaKey string }{
		{"maxTotalVolumes", "volumes"},
		{"maxTotalSnapshots", "snapshots"},
		{"maxTotalVolumeGigabytes", "gigabytes"},
	} {
		l, q := cinderNum(t, abs, pair.limitKey), cinderNum(t, qs, pair.quotaKey)
		if l != q {
			t.Errorf("%s=%v but quota_set.%s=%v; the two views must agree (FR-003)",
				pair.limitKey, l, pair.quotaKey, q)
		}
	}
}

// TestBackupUsageIsCounted is assertion 6 of contracts/cinder-extensions-and-quota-sets.md:
// backup usage must reflect seeded backups, not 0. Both views hardcoded 0, justified by a
// comment claiming Lightstack has no backups table — it has had `volume_backups` since migration
// 022, and internal/cinder/backups.go writes to it (FR-004).
func TestBackupUsageIsCounted(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const sizeGB = 3

	// A project of its own. The test compares the limits view with the usage view, and
	// packages run in parallel against one test database: on the shared seeded project
	// another package's test could create a volume between the two reads and make two
	// agreeing counters look divergent. Deleting the project cascades to everything
	// seeded below.
	projectID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, description, enabled) VALUES ($1, $2, '', true)`,
		projectID, "backup-usage-"+projectID); err != nil {
		t.Fatalf("seeding project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", projectID)
	})

	// volume_backups.volume_id is NOT NULL and references volumes(id), so the backup
	// needs a volume. ON DELETE CASCADE takes the backup with it during cleanup.
	var volumeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO volumes (project_id, name, size_gb, status)
		VALUES ($1, 'spec020-backup-src', $2, 'available') RETURNING id`,
		projectID, sizeGB).Scan(&volumeID); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			`DELETE FROM volumes WHERE id = $1`, volumeID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO volume_backups (project_id, volume_id, name, size_gb)
		VALUES ($1, $2, 'spec020-backup-probe', $3)`,
		projectID, volumeID, sizeGB); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	r := newCinderTestRouter()
	hdr := cinderAuthHdr(projectID, seededUserID)

	_, limBody := doReq(r, http.MethodGet, "/v3/limits", "", hdr)
	abs := cinderAbsolute(t, limBody)
	if got := cinderNum(t, abs, "totalBackupsUsed"); got < 1 {
		t.Errorf("totalBackupsUsed=%v; want >=1 — a backup exists, so 0 means the counter is hardcoded again", got)
	}
	if got := cinderNum(t, abs, "totalBackupGigabytesUsed"); got < sizeGB {
		t.Errorf("totalBackupGigabytesUsed=%v; want >=%d", got, sizeGB)
	}

	// A snapshot too, so every counted resource has a non-zero case below rather than
	// only the backup pair.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO snapshots (name, volume_id, project_id, size_gb)
		VALUES ('spec020-usage-snap', $1, $2, $3)`, volumeID, projectID, sizeGB); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// And the usage view must agree, on every resource — not just the pair that was
	// hardcoded. An assertion that only checks the keys are present would pass with every
	// in_use stuck at 0, which is the defect this whole test exists for.
	_, detBody := doReq(r, http.MethodGet, "/v3/quota-sets/"+projectID+"?usage=true", "", hdr)
	det := cinderQuotaSet(t, detBody)
	_, limBody = doReq(r, http.MethodGet, "/v3/limits", "", hdr)
	abs = cinderAbsolute(t, limBody)

	for _, pair := range []struct {
		limitKey, quotaKey string
		wantAtLeast        float64
	}{
		{"totalBackupsUsed", "backups", 1},
		{"totalBackupGigabytesUsed", "backup_gigabytes", sizeGB},
		{"totalVolumesUsed", "volumes", 1},
		{"totalGigabytesUsed", "gigabytes", sizeGB},
		{"totalSnapshotsUsed", "snapshots", 1},
	} {
		entry, ok := det[pair.quotaKey].(map[string]any)
		if !ok {
			t.Errorf("usage view: %s is not an object; body=%s", pair.quotaKey, detBody)
			continue
		}
		inUse := cinderNum(t, entry, "in_use")
		if inUse < pair.wantAtLeast {
			t.Errorf("usage view %s.in_use=%v; want >=%v — this project has one seeded, so a "+
				"lower value means the counter is not reading the table", pair.quotaKey, inUse, pair.wantAtLeast)
		}
		if l := cinderNum(t, abs, pair.limitKey); l != inUse {
			t.Errorf("%s=%v but usage view %s.in_use=%v; the two views must agree (FR-004)",
				pair.limitKey, l, pair.quotaKey, inUse)
		}
	}
}

// TestQuotaSetUsageAndDefaults — `/defaults` returned 404 before the fix, and the usage
// view did not exist in any spelling (FR-002). Block Storage v3 shows usage through
// `?usage=true`, not Nova's `/detail` sub-resource.
func TestQuotaSetUsageAndDefaults(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID+"?usage=true", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("usage view: code=%d body=%s; want 200", code, body)
	}
	entry, ok := cinderQuotaSet(t, body)["volumes"].(map[string]any)
	if !ok {
		t.Fatalf("usage view: volumes is not an object; body=%s", body)
	}
	for _, key := range []string{"in_use", "limit", "reserved"} {
		if _, present := entry[key]; !present {
			t.Errorf("usage view: volumes is missing %q", key)
		}
	}

	// The parameter must be what switches the shape, and only when asked. Without it the
	// response stays flat ints — clients that never pass `usage` see unchanged behavior
	// (FR-016). `usage=false` and a junk value are both the documented default.
	for _, query := range []string{"", "?usage=false", "?usage=nonsense"} {
		code, body := doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID+query, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", query, code, body)
		}
		if _, nested := cinderQuotaSet(t, body)["volumes"].(map[string]any); nested {
			t.Errorf("GET %q returned the nested usage shape; want flat ints", query)
		}
	}

	code, body = doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID+"/defaults", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("defaults: code=%d body=%s; want 200", code, body)
	}
	assertCinderDefaultLimits(t, cinderQuotaSet(t, body))
}

// assertCinderDefaultLimits pins the default limits as literals.
//
// Comparing the response to cinderQuotaDefaults instead — as this did — passes for any
// value the map happens to hold, so it cannot notice a default silently changing. Nova's
// equivalent pins literals for exactly that reason.
//
// These are Lightstack's defaults, deliberately **not** upstream Cinder's, which are 10× smaller
// (volumes 10, snapshots 10, gigabytes 1000, backups 10, backup_gigabytes 1000,
// per_volume_gigabytes -1). FR-003 reconciled that same 10× gap for *compute* but says
// nothing about block storage, so the divergence stands — pinned here so that changing it
// is a deliberate act with a failing test, not a silent edit.
func assertCinderDefaultLimits(t *testing.T, quotaSet map[string]any) {
	t.Helper()
	for resource, want := range map[string]float64{
		"volumes":              100,
		"snapshots":            100,
		"gigabytes":            10000,
		"backups":              100,
		"backup_gigabytes":     10000,
		"per_volume_gigabytes": 1000,
	} {
		if got := cinderNum(t, quotaSet, resource); got != want {
			t.Errorf("defaults %s=%v; want %v", resource, got, want)
		}
	}
}

// TestDeleteQuotaSetResetsToDefaults executes DeleteQuotaSet's body, which nothing did:
// every other DELETE test stops at 403 (non-admin) or 404 (phantom project), so the reset
// — the endpoint's entire purpose — was never performed.
func TestDeleteQuotaSetResetsToDefaults(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newCinderTestRouter()
	admin := cinderAuthHdr(seededProjectID, seededUserID)
	const override = 42

	t.Cleanup(func() {
		testPool.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			`DELETE FROM cinder_quotas WHERE project_id = $1 AND resource = 'volumes'`, seededProjectID)
	})

	code, body := doReq(r, http.MethodPut, "/v3/quota-sets/"+seededProjectID,
		`{"quota_set":{"volumes":42}}`, admin)
	if code != http.StatusOK {
		t.Fatalf("PUT: code=%d body=%s; want 200", code, body)
	}
	_, body = doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID, "", admin)
	if got := cinderNum(t, cinderQuotaSet(t, body), "volumes"); got != override {
		t.Fatalf("after PUT, volumes=%v; want %d — the override did not take", got, override)
	}

	code, body = doReq(r, http.MethodDelete, "/v3/quota-sets/"+seededProjectID, "", admin)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE: code=%d body=%s; want 204", code, body)
	}

	_, body = doReq(r, http.MethodGet, "/v3/quota-sets/"+seededProjectID, "", admin)
	if got := cinderNum(t, cinderQuotaSet(t, body), "volumes"); got != 100 {
		t.Errorf("after DELETE, volumes=%v; want the default 100 — the reset did not happen", got)
	}

	// And the row must really be gone, not merely masked by the response.
	var rows int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM cinder_quotas WHERE project_id = $1 AND resource = 'volumes'`,
		seededProjectID).Scan(&rows); err != nil {
		t.Fatalf("count cinder_quotas rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("cinder_quotas still has %d volumes row(s) for the project after DELETE", rows)
	}
}

// TestQuotaWritesRequireAdmin — PUT and DELETE had NO authorization at all before the
// fix, so any authenticated caller could rewrite or reset any project's quota (FR-020).
func TestQuotaWritesRequireAdmin(t *testing.T) {
	r := newCinderTestRouter()
	nonAdmin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID}

	code, body := doReq(r, http.MethodPut, "/v3/quota-sets/"+seededProjectID,
		`{"quota_set":{"volumes":1}}`, nonAdmin)
	if code != http.StatusForbidden {
		t.Errorf("non-admin PUT quota-set: code=%d body=%s; want 403", code, body)
	}

	code, body = doReq(r, http.MethodDelete, "/v3/quota-sets/"+seededProjectID, "", nonAdmin)
	if code != http.StatusForbidden {
		t.Errorf("non-admin DELETE quota-set: code=%d body=%s; want 403", code, body)
	}
}

// TestCinderQuotaReadsAreProjectScoped (FR-002a).
func TestCinderQuotaReadsAreProjectScoped(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	const otherProject = "11111111-1111-1111-1111-111111111111"

	// Upstream's volume_extension:quotas:show (SYSTEM_READER_OR_PROJECT_READER) guards
	// both show and defaults, so a non-admin naming another project gets a 403 on each.
	for _, path := range []string{"/v3/quota-sets/" + otherProject, "/v3/quota-sets/" + otherProject + "/defaults"} {
		code, body := doReq(r, http.MethodGet, path, "",
			map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID})
		if code != http.StatusForbidden {
			t.Errorf("non-admin GET %s: code=%d body=%s; want 403 (FR-002a)", path, code, body)
		}
	}
}

// TestRouteRegistration_NoWildcardConflict guards route registration in
// internal/cinder/volumes.go: every flat /v3/<static> route must be registered at
// all — order does not matter (see the note in volumes.go).
// This feature adds three such routes (/v3/extensions and the two quota-set
// variants), so the failure mode is worth catching in a test rather than in a
// runbook pass (SPEC-020 T052).
func TestRouteRegistration_NoWildcardConflict(t *testing.T) {
	r := newCinderTestRouter()

	// The routes this feature adds must resolve to a handler, not fall through to 404.
	registered := map[string]bool{}
	for _, ri := range r.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{
		"GET /v3/extensions",
		"GET /v3/quota-sets/:id/defaults",
		"GET /v3/:project_id/extensions",
		"GET /v3/:project_id/quota-sets/:id/defaults",
		// B1: the groups sub-resource cinderclient actually calls, at both the flat
		// and the project-scoped path.
		"GET /v3/groups/detail",
		"GET /v3/:project_id/groups",
		"GET /v3/:project_id/groups/detail",
		"GET /v3/:project_id/groups/:id",
		// N3: os-quota-sets is upstream Cinder's spelling and the one cinderclient uses.
		"GET /v3/os-quota-sets/:id",
		"GET /v3/os-quota-sets/:id/defaults",
		"PUT /v3/os-quota-sets/:id",
		"DELETE /v3/os-quota-sets/:id",
		"GET /v3/:project_id/os-quota-sets/:id",
		"GET /v3/:project_id/os-quota-sets/:id/defaults",
		// Every alias the /extensions response advertises must resolve at BOTH bases,
		// or discovery points a client at a 404. The scoped form was missing while the
		// scoped /extensions advertised it (raised on PR #79).
		"GET /v3/os-availability-zone",
		"GET /v3/:project_id/os-availability-zone",
		// SPEC-022 B2: the project-scoped volume collection, absent as a GET while
		// POST was registered — a 405 that surprised a client — plus the four
		// sub-resource names a sibling :id route in the same group was swallowing.
		"GET /v3/:project_id/volumes",
		"GET /v3/:project_id/volumes/detail",
		"GET /v3/:project_id/snapshots/detail",
		"GET /v3/:project_id/types/default",
		"GET /v3/:project_id/os-volume-transfer/detail",
	} {
		if !registered[want] {
			t.Errorf("route %q is not registered", want)
		}
	}

	// SPEC-022 FR-016a: `backups/detail` stays absent from the project-scoped group.
	// It is not shadowed — that group registers no backup route at all, so the path
	// answers an honest 404 from the no-route case rather than being served as a
	// lookup for a backup named "detail". It belongs with the 13 merely-absent routes
	// a later ticket closes deliberately, not with the four that answered wrongly.
	if registered["GET /v3/:project_id/backups/detail"] {
		t.Error("GET /v3/:project_id/backups/detail was added; FR-016a scopes it out — " +
			"it is absent rather than shadowed, so adding it here is untested surface")
	}

	// And `/detail` must NOT be registered. It is Nova's spelling; Block Storage v3
	// has no such sub-resource and shows usage via `?usage=true` on the show route.
	// Registering it was speculative surface no client requests (FR-002, FR-014).
	for _, unwanted := range []string{
		"GET /v3/quota-sets/:id/detail",
		"GET /v3/os-quota-sets/:id/detail",
		"GET /v3/:project_id/quota-sets/:id/detail",
		"GET /v3/:project_id/os-quota-sets/:id/detail",
	} {
		if registered[unwanted] {
			t.Errorf("route %q is registered; block storage has no /detail sub-resource", unwanted)
		}
	}
}

// TestQuotaTargetMustExist: a syntactically valid UUID is not enough — the project
// must exist. Upstream validates the target and errors when it is unknown (Nova's
// Pike change made this explicit: "validate the project_id with Keystone … to prevent
// invalid data from being stored"; Cinder's _get_project raises NotFound). Lightstack must
// not serve defaults for a phantom project, and must never write quota rows under one.
func TestQuotaTargetMustExist(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	admin := cinderAuthHdr(seededProjectID, seededUserID)

	for _, tc := range []struct{ name, ref string }{
		{"unknown but well-formed uuid", "11111111-1111-1111-1111-111111111111"},
		{"nil uuid", "00000000-0000-0000-0000-000000000000"},
		{"unknown project name", "no-such-project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{
				"/v3/quota-sets/" + tc.ref,
				"/v3/quota-sets/" + tc.ref + "?usage=true",
				"/v3/quota-sets/" + tc.ref + "/defaults",
			} {
				code, body := doReq(r, http.MethodGet, path, "", admin)
				if code != http.StatusNotFound {
					t.Errorf("GET %s: code=%d body=%s; want 404 for a project that does not exist",
						path, code, body)
				}
			}

			code, body := doReq(r, http.MethodPut, "/v3/quota-sets/"+tc.ref,
				`{"quota_set":{"volumes":5}}`, admin)
			if code != http.StatusNotFound {
				t.Errorf("PUT quota-set %s: code=%d body=%s; want 404 (never store quota rows "+
					"against a nonexistent project)", tc.ref, code, body)
			}

			code, body = doReq(r, http.MethodDelete, "/v3/quota-sets/"+tc.ref, "", admin)
			if code != http.StatusNotFound {
				t.Errorf("DELETE quota-set %s: code=%d body=%s; want 404", tc.ref, code, body)
			}
		})
	}
}

// TestQuotaTargetByName keeps the non-upstream name extension working: the contract
// suite addresses quota-sets as the literal "default".
func TestQuotaTargetByName(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	admin := cinderAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodGet, "/v3/quota-sets/default", "", admin)
	if code != http.StatusOK {
		t.Fatalf("GET quota-sets/default: code=%d body=%s; want 200", code, body)
	}
	if id, _ := cinderQuotaSet(t, body)["id"].(string); id != seededProjectID {
		t.Errorf("quota_set.id=%q; want the resolved project id %q", id, seededProjectID)
	}
}

// TestQuotaWriteTargetMatchesRead: PUT and DELETE resolve their target the way GET
// does. The reads treat `default` as the caller's own project, so the writes must too —
// otherwise an admin scoped to any project but the one NAMED `default` rewrites that
// project's quota, and a DELETE silently wipes its overrides, while GET keeps showing
// the caller's own. TestQuotaTargetByName cannot catch this: its admin sits in the
// project named `default`, where both meanings coincide. Raised on PR #79.
func TestQuotaWriteTargetMatchesRead(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	var caller string
	if err := testPool.QueryRow(ctx,
		"INSERT INTO projects (name, description) VALUES ($1, 'spec020 test fixture') RETURNING id::text",
		"spec020-"+t.Name(),
	).Scan(&caller); err != nil {
		t.Fatalf("create caller project: %v", err)
	}
	// cinder_quotas has no foreign key to projects, so nothing cascades.
	t.Cleanup(func() {
		testPool.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			"DELETE FROM cinder_quotas WHERE project_id IN ($1, $2)", caller, seededProjectID)
		testPool.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			"DELETE FROM projects WHERE id = $1", caller)
	})
	// An override the project named `default` already holds; the caller's DELETE must
	// leave it alone.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cinder_quotas (project_id, resource, "limit") VALUES ($1, 'snapshots', 55)
		 ON CONFLICT (project_id, resource) DO UPDATE SET "limit" = 55`, seededProjectID); err != nil {
		t.Fatalf("seed the default project's override: %v", err)
	}

	r := newCinderTestRouter()
	admin := cinderAuthHdr(caller, seededUserID)

	code, body := doReq(r, http.MethodPut, "/v3/quota-sets/default", `{"quota_set":{"volumes":7}}`, admin)
	if code != http.StatusOK {
		t.Fatalf("PUT quota-sets/default: code=%d body=%s; want 200", code, body)
	}
	put := cinderQuotaSet(t, body)
	_, body = doReq(r, http.MethodGet, "/v3/quota-sets/default", "", admin)
	get := cinderQuotaSet(t, body)
	if put["id"] != caller || get["id"] != caller || cinderNum(t, get, "volumes") != 7 {
		t.Errorf("PUT answered id=%v, GET answered id=%v volumes=%v; want both on the caller's project %s with volumes 7",
			put["id"], get["id"], get["volumes"], caller)
	}

	if code, body = doReq(r, http.MethodDelete, "/v3/quota-sets/default", "", admin); code != http.StatusNoContent {
		t.Fatalf("DELETE quota-sets/default: code=%d body=%s; want 204", code, body)
	}
	var snapshots int
	if err := testPool.QueryRow(ctx,
		`SELECT "limit" FROM cinder_quotas WHERE project_id = $1 AND resource = 'snapshots'`,
		seededProjectID).Scan(&snapshots); err != nil {
		t.Errorf("DELETE quota-sets/default by a caller scoped elsewhere reset the project named `default`: %v", err)
	}
}

// TestLimitsRejectUnscopedToken: an unscoped token carries no project. GetLimits handed
// its empty id to the uuid-typed cinder_quotas query — a 500 on both limits routes —
// while the quota routes answered the same token 404 through QuotaTargetProject. All
// now answer alike. Raised on PR #79.
func TestLimitsRejectUnscopedToken(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	unscoped := map[string]string{"X-Test-User": seededUserID} // no project, no roles

	for _, path := range []string{
		"/v3/limits",
		"/v3/" + seededProjectID + "/limits",
		"/v3/quota-sets/" + seededProjectID,
	} {
		if code, body := doReq(r, http.MethodGet, path, "", unscoped); code != http.StatusNotFound {
			t.Errorf("GET %s with an unscoped token: code=%d body=%s; want 404", path, code, body)
		}
	}
}

// TestUpdateQuotaSet_PersistsUnderResolvedID proves the write lands under the real
// project id (the handler used to parse into a uuid.UUID whose zero value is the nil
// UUID when parsing failed).
func TestUpdateQuotaSet_PersistsUnderResolvedID(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	admin := cinderAuthHdr(seededProjectID, seededUserID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			"DELETE FROM cinder_quotas WHERE project_id = $1", seededProjectID)
	})

	code, body := doReq(r, http.MethodPut, "/v3/quota-sets/"+seededProjectID,
		`{"quota_set":{"volumes":42}}`, admin)
	if code != http.StatusOK {
		t.Fatalf("PUT: code=%d body=%s; want 200", code, body)
	}
	if got := cinderNum(t, cinderQuotaSet(t, body), "volumes"); got != 42 {
		t.Errorf("PUT echoed volumes=%v; want 42", got)
	}

	var stored int
	if err := testPool.QueryRow(context.Background(),
		`SELECT "limit" FROM cinder_quotas WHERE project_id = $1 AND resource = 'volumes'`,
		seededProjectID).Scan(&stored); err != nil {
		t.Fatalf("quota row was not stored under %s: %v", seededProjectID, err)
	}
	if stored != 42 {
		t.Errorf("stored limit=%d; want 42", stored)
	}

	// And it must be visible through the read path (FR-003).
	_, body = doReq(r, http.MethodGet, "/v3/limits", "", admin)
	if got := cinderNum(t, cinderAbsolute(t, body), "maxTotalVolumes"); got != 42 {
		t.Errorf("maxTotalVolumes=%v after the override; want 42", got)
	}
}
