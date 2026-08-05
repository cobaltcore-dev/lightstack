package nova

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
)

// SPEC-020 (cloud-storage#533) — compute quota/limits metadata Horizon's create
// flows read before enabling the Launch Instance action.

// quotaSetBody extracts the quota_set envelope.
func quotaSetBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		QuotaSet map[string]any `json:"quota_set"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse quota_set: %v; body=%s", err, body)
	}
	return env.QuotaSet
}

// absoluteLimits extracts limits.absolute.
func absoluteLimits(t *testing.T, body string) map[string]any {
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

func numOf(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing; got keys %v", key, mapKeys(m))
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("key %q = %#v; want a number", key, v)
	}
	return f
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestQuotaDefaults_MatchUpstream pins the compute defaults to the upstream values.
// GetLimits previously carried its own map at 100/200/512000 — ten times these —
// which disagreed with the quota-set for any project without seeded quota rows
// (FR-003). No DB required.
func TestQuotaDefaults_MatchUpstream(t *testing.T) {
	for resource, want := range map[string]int{"instances": 10, "cores": 20, "ram": 51200} {
		if got := quotaDefaults[resource]; got != want {
			t.Errorf("quotaDefaults[%q] = %d; want the upstream default %d", resource, got, want)
		}
	}
	// The limits view needs these; they must resolve through the same helper.
	for _, resource := range []string{"keypairs", "server_groups", "server_group_members"} {
		if _, ok := limitsOnlyDefaults[resource]; !ok {
			t.Errorf("limitsOnlyDefaults is missing %q, which the absolute-limits view reports", resource)
		}
	}
}

// TestUsageQueryCoversEveryQuotaResource is the drift guard between the two halves of
// the detail view. GetQuotaSetDetail walks quotaDefaults and asks usageQuery for each
// resource's SQL; a resource declared in the map but missing from the switch reports
// in_use: 0 forever, which is indistinguishable from genuinely-zero usage. `snapshots`
// shipped exactly that way — GetQuotaSet counted it, GetQuotaSetDetail reported 0.
// No DB required.
func TestUsageQueryCoversEveryQuotaResource(t *testing.T) {
	for resource := range quotaDefaults {
		if _, err := usageQuery(resource); err != nil {
			t.Errorf("usageQuery(%q): %v — quotaDefaults declares this resource, so the detail view would report in_use: 0 for it regardless of real usage", resource, err)
		}
	}
}

// TestQuotaSetDefaults_NoDB proves the defaults endpoint reports the shared defaults
// and carries no usage keys. No DB required.
func TestQuotaSetDefaults_NoDB(t *testing.T) {
	r := newNovaTestRouter()
	// Deliberately NOT an admin header. This test is about the defaults response
	// SHAPE, and a non-admin is scoped to its own project, so QuotaTargetProject
	// never reaches the database — which is what lets the test keep its name and run
	// in a bare `go test`. With an admin role plus an explicit :id it would take the
	// resolve branch and need a DB (it used to, and segfaulted on the nil pool).
	nonAdmin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID}
	code, body := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID+"/defaults", "",
		nonAdmin)
	if code != http.StatusOK {
		t.Fatalf("defaults: code=%d body=%s; want 200", code, body)
	}
	qs := quotaSetBody(t, body)
	if got := numOf(t, qs, "instances"); got != 10 {
		t.Errorf("defaults instances=%v; want 10", got)
	}
	if _, present := qs["instances_used"]; present {
		t.Error("defaults response must not carry usage keys")
	}
}

// TestLimitsAgreeWithQuotaSet is the FR-003 regression guard: the absolute-limits
// view and the quota-set view must report the same value for the same resource.
func TestLimitsAgreeWithQuotaSet(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	code, limBody := doReq(r, http.MethodGet, "/v2.1/limits", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("limits: code=%d body=%s", code, limBody)
	}
	code, qsBody := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("quota-set: code=%d body=%s", code, qsBody)
	}

	abs := absoluteLimits(t, limBody)
	qs := quotaSetBody(t, qsBody)
	for _, pair := range []struct{ limitKey, quotaKey string }{
		{"maxTotalInstances", "instances"},
		{"maxTotalCores", "cores"},
		{"maxTotalRAMSize", "ram"},
	} {
		l, q := numOf(t, abs, pair.limitKey), numOf(t, qs, pair.quotaKey)
		if l != q {
			t.Errorf("%s=%v but quota_set.%s=%v; the two views must agree (FR-003)",
				pair.limitKey, l, pair.quotaKey, q)
		}
	}
}

// TestLimitsHonorQuotaOverride proves an admin's quotas-table override reaches BOTH
// views. Before the fix GetLimits used its own constants for unseeded resources.
func TestLimitsHonorQuotaOverride(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const override = 77
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO quotas (project_id, resource, hard_limit) VALUES ($1, 'instances', $2)
		ON CONFLICT (project_id, resource) DO UPDATE SET hard_limit = $2`,
		seededProjectID, override); err != nil {
		t.Fatalf("seed quota override: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, //nolint:errcheck // best-effort restore of the seeded value
			`UPDATE quotas SET hard_limit = 10 WHERE project_id = $1 AND resource = 'instances'`,
			seededProjectID)
	})

	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	_, limBody := doReq(r, http.MethodGet, "/v2.1/limits", "", hdr)
	if got := numOf(t, absoluteLimits(t, limBody), "maxTotalInstances"); got != override {
		t.Errorf("maxTotalInstances=%v; want the quotas-table override %d", got, override)
	}
	_, qsBody := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID, "", hdr)
	if got := numOf(t, quotaSetBody(t, qsBody), "instances"); got != override {
		t.Errorf("quota_set.instances=%v; want the quotas-table override %d", got, override)
	}
}

// TestQuotaSetDetail returns per-resource {in_use, limit, reserved} (FR-002).
// This endpoint returned 404 before the fix.
func TestQuotaSetDetail(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// A stock database has no snapshots, so the cross-view comparison below would be a
	// trivial 0 == 0 and would not catch the drift it exists to catch. One row makes it
	// bite. volume_id stays NULL so nothing else's usage moves.
	var snapshotID string
	if err := database.DB.QueryRow(ctx, `
		INSERT INTO snapshots (name, project_id, size_gb) VALUES ('spec020-usage-probe', $1, 1)
		RETURNING id`, seededProjectID).Scan(&snapshotID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			`DELETE FROM snapshots WHERE id = $1`, snapshotID)
	})

	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID+"/detail", "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s; want 200", code, body)
	}
	detail := quotaSetBody(t, body)

	// Every declared resource, not just instances: checking one entry is how the
	// snapshots gap shipped unnoticed.
	for resource := range quotaDefaults {
		entry, ok := detail[resource].(map[string]any)
		if !ok {
			t.Errorf("detail %s = %#v; want an object", resource, detail[resource])
			continue
		}
		for _, key := range []string{"in_use", "limit", "reserved"} {
			if _, present := entry[key]; !present {
				t.Errorf("detail %s is missing %q; got %v", resource, key, mapKeys(entry))
			}
		}
	}

	// The two views of the same number must agree. GetQuotaSet publishes usage as
	// flat `<resource>_used` keys and the detail view as nested `in_use`, computed by
	// separate code — so they can and did disagree (snapshots).
	code, body = doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID, "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("quota-set: code=%d body=%s; want 200", code, body)
	}
	flat := quotaSetBody(t, body)
	for resource := range quotaDefaults {
		entry, ok := detail[resource].(map[string]any)
		if !ok {
			continue // already reported above
		}
		want := numOf(t, flat, resource+"_used")
		if got := numOf(t, entry, "in_use"); got != want {
			t.Errorf("detail %s.in_use = %v but quota-set %s_used = %v; the two views must report the same usage",
				resource, got, resource, want)
		}
	}

	// The limits-only resources must appear here too, with limits agreeing with the
	// absolute-limits view. This view used to omit them, making it the only one that
	// disagreed about which resources exist.
	_, limBody := doReq(r, http.MethodGet, "/v2.1/limits", "", testAuthHdr(seededProjectID, seededUserID))
	abs := absoluteLimits(t, limBody)
	for _, pair := range []struct{ resource, limitKey string }{
		{"keypairs", "maxTotalKeypairs"},
		{"server_groups", "maxServerGroups"},
		{"server_group_members", "maxServerGroupMembers"},
	} {
		entry, ok := detail[pair.resource].(map[string]any)
		if !ok {
			t.Errorf("detail is missing %q; the absolute-limits view reports it as %s",
				pair.resource, pair.limitKey)
			continue
		}
		for _, key := range []string{"in_use", "limit", "reserved"} {
			if _, present := entry[key]; !present {
				t.Errorf("detail %s is missing %q; got %v", pair.resource, key, mapKeys(entry))
			}
		}
		if got, want := numOf(t, entry, "limit"), numOf(t, abs, pair.limitKey); got != want {
			t.Errorf("detail %s.limit = %v but %s = %v; the two views must agree (FR-003)",
				pair.resource, got, pair.limitKey, want)
		}
	}

	// And keypair usage must be counted, not reported as a constant 0.
	code, kpBody := doReq(r, http.MethodPost, "/v2.1/os-keypairs",
		`{"keypair":{"name":"spec020-detail-probe"}}`, testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create keypair: code=%d body=%s; want 200/201", code, kpBody)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			`DELETE FROM keypairs WHERE name = 'spec020-detail-probe'`)
	})
	_, body = doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID+"/detail", "",
		testAuthHdr(seededProjectID, seededUserID))
	kp, ok := quotaSetBody(t, body)["keypairs"].(map[string]any)
	if !ok {
		t.Fatalf("detail keypairs is not an object; body=%s", body)
	}
	if got := numOf(t, kp, "in_use"); got < 1 {
		t.Errorf("detail keypairs.in_use = %v after creating one; want >=1 (0 means it is hardcoded)", got)
	}
}

// TestQuotaReadsAreProjectScoped: a non-admin may not read another project's quota;
// an admin may (FR-002a).
func TestQuotaReadsAreProjectScoped(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	// A REAL second project. Using a nonexistent id here would conflate this test
	// with SC-007a, which requires a phantom target to 404 — see
	// TestNovaQuotaTargetMustExist.
	// Deliberately NOT 1111…/0000… — those are the phantom ids
	// TestNovaQuotaTargetMustExist requires to be absent, and createTestProject does
	// not clean up, so reusing one would make the two tests fight.
	const otherProject = "22222222-2222-2222-2222-222222222222"
	createTestProject(t, otherProject, "spec020-scoping-other")

	// Non-admin: no X-Test-Roles header => no roles => scoped to own project.
	code, body := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+otherProject, "",
		map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID})
	if code != http.StatusOK {
		t.Fatalf("non-admin quota read: code=%d body=%s; want 200", code, body)
	}
	if id, _ := quotaSetBody(t, body)["id"].(string); id != seededProjectID {
		t.Errorf("non-admin got quota for project %q; want its own %q (FR-002a)", id, seededProjectID)
	}

	// Admin may address another project.
	code, body = doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+otherProject, "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("admin quota read: code=%d body=%s; want 200", code, body)
	}
	if id, _ := quotaSetBody(t, body)["id"].(string); id != otherProject {
		t.Errorf("admin got quota for project %q; want the requested %q", id, otherProject)
	}
}

// TestUpdateQuotaSet_AuthzBeforeBodyBinding: a non-admin sending a malformed body
// gets 403, not the 400 that would leak a request-validation result (FR-020).
// TestQuotaSetPutRoundTrips is the FR-016 guard on the write path. UpdateQuotaSet
// persists whatever resource key it is handed and then returns GetQuotaSet's response,
// so a resource stored outside the 13 declared ones must still be echoed. It wasn't:
// the PUT succeeded, GetLimits honoured the new value, and the 200 that confirmed the
// write omitted the very field it had just written. The pre-existing handler echoed
// every stored row.
//
// Rejecting unknown keys with 400 — what upstream Nova does — would close the gap from
// the other side, but that is a behavior change FR-016 does not sanction here, so the
// echo is what gets restored.
func TestQuotaSetPutRoundTrips(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const want = 5

	// keypairs is a real Nova quota resource, but it lives in limitsOnlyDefaults rather
	// than quotaDefaults — so it exercises the stored-but-undeclared path.
	t.Cleanup(func() {
		database.DB.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup
			`DELETE FROM quotas WHERE project_id = $1 AND resource = 'keypairs'`, seededProjectID)
	})

	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID) // carries X-Test-Roles: admin

	code, body := doReq(r, http.MethodPut, "/v2.1/os-quota-sets/"+seededProjectID,
		`{"quota_set":{"keypairs":5}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("PUT: code=%d body=%s; want 200", code, body)
	}
	if got := numOf(t, quotaSetBody(t, body), "keypairs"); got != want {
		t.Errorf("PUT response keypairs=%v; want %d — the write is invisible in its own response", got, want)
	}

	// And the stored value must actually be in effect, not merely echoed.
	_, limBody := doReq(r, http.MethodGet, "/v2.1/limits", "", hdr)
	if got := numOf(t, absoluteLimits(t, limBody), "maxTotalKeypairs"); got != want {
		t.Errorf("maxTotalKeypairs=%v; want %d", got, want)
	}
}

func TestUpdateQuotaSet_AuthzBeforeBodyBinding(t *testing.T) {
	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodPut, "/v2.1/os-quota-sets/"+seededProjectID, "{not json",
		map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID})
	if code != http.StatusForbidden {
		t.Errorf("non-admin PUT with a malformed body: code=%d body=%s; want 403 (authz precedes binding)", code, body)
	}
}

// TestLimitsUsageCountersAreReal: the absolute-limits view must report real usage for
// floating IPs, security groups and server groups, not hardcoded zeros.
//
// GetQuotaSet counted these resources for real while GetLimits returned 0 for all
// three, so the Overview widget and the quota-set view disagreed about usage — the same
// class of drift FR-003 fixed for the limit values themselves (FR-004). The seeded
// project has a `default` security group from migration 002, so totalSecurityGroupsUsed
// is non-zero on a stock database and a regression back to the literal 0 is caught.
func TestLimitsUsageCountersAreReal(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	_, limBody := doReq(r, http.MethodGet, "/v2.1/limits", "", hdr)
	abs := absoluteLimits(t, limBody)

	// Present and numeric on all three — a missing key would read as 0 and hide a
	// regression.
	for _, key := range []string{"totalFloatingIpsUsed", "totalSecurityGroupsUsed", "totalServerGroupsUsed"} {
		if _, present := abs[key]; !present {
			t.Errorf("absolute limits is missing %q", key)
		}
	}

	// The seeded project always has the `default` security group (migration 002), so
	// this counter cannot legitimately be 0 — which is what the hardcoded literal gave.
	if got := numOf(t, abs, "totalSecurityGroupsUsed"); got < 1 {
		t.Errorf("totalSecurityGroupsUsed=%v; want >=1 (migration 002 seeds a default "+
			"security group — 0 means the counter is hardcoded again)", got)
	}

	// And it must agree with what the quota-set view counts for the same resource.
	_, qsBody := doReq(r, http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID, "", hdr)
	qs := quotaSetBody(t, qsBody)
	if lim, used := numOf(t, abs, "totalSecurityGroupsUsed"), numOf(t, qs, "security_groups_used"); lim != used {
		t.Errorf("security-group usage disagrees: limits=%v quota_set=%v (FR-004)", lim, used)
	}
}
