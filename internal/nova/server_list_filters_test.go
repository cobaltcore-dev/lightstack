package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// SPEC-025-followup US1: the server listing honours the filters it accepts.
//
// These are row-level and DB-backed on purpose. FR-009 requires that a NON-MATCHING
// value return an empty collection, and only a real query proves that: an assertion on
// the rendered SQL proves a predicate was appended, not that the predicate selects
// anything. The SQL-level assertions that belong here — the refusal, and the
// unfiltered-request-unchanged property — live in server_filter_test.go alongside the
// scope tests that established the pattern.
//
// THE FAILURE MODE THESE GUARD AGAINST: every positive case below passes against the
// pre-change code, because an ignored filter returns the whole project and the matching
// server is in it. Only the negative cases distinguish a working filter from no filter
// at all, which is why each subtest asserts both directions.

const (
	filterProject      = "00000000-0000-0000-0000-0000000000f1"
	filterOtherProject = "00000000-0000-0000-0000-0000000000f2"
)

type filterFixture struct {
	alphaID, betaID, otherID string
	alphaImage, betaImage    string
	alphaFlavor, betaFlavor  string
	beforeAll                time.Time
}

// seedFilterFixtures creates two servers in one project that differ in every
// filterable attribute, plus one in a second project so scope violations are visible.
func seedFilterFixtures(t *testing.T) filterFixture {
	t.Helper()
	ctx := context.Background()
	var f filterFixture

	for id, name := range map[string]string{filterProject: "spec025f-a", filterOtherProject: "spec025f-b"} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO projects (id, name, description, enabled, domain_id)
			 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))
			 ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
			t.Fatalf("seeding project %s: %v", name, err)
		}
	}

	// Two distinct flavors so ?flavor= can discriminate; fall back to the same one
	// when the deployment seeds only a single flavor.
	rows, err := testPool.Query(ctx, `SELECT id::text FROM flavors ORDER BY name LIMIT 2`)
	if err != nil {
		t.Fatalf("reading flavors: %v", err)
	}
	var flavors []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning flavor: %v", err)
		}
		flavors = append(flavors, id)
	}
	rows.Close()
	if len(flavors) == 0 {
		t.Fatal("no flavors seeded; the filter fixtures need at least one")
	}
	f.alphaFlavor = flavors[0]
	f.betaFlavor = flavors[len(flavors)-1]

	f.alphaImage = "00000000-0000-0000-0000-0000000000e1"
	f.betaImage = "00000000-0000-0000-0000-0000000000e2"

	_, _ = testPool.Exec(ctx, `DELETE FROM instances WHERE project_id = ANY($1)`,
		[]string{filterProject, filterOtherProject})

	f.beforeAll = time.Now().UTC().Add(-1 * time.Hour)

	seed := func(name, project, image, flavor, status, keyName string, locked bool) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO instances (name, project_id, user_id, flavor_id, image_id, status, power_state, key_name, locked)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
			name, project, seededUserID, flavor, image, status, 1, keyName, locked).Scan(&id); err != nil {
			t.Fatalf("seeding instance %s: %v", name, err)
		}
		return id
	}
	f.alphaID = seed("filter-alpha", filterProject, f.alphaImage, f.alphaFlavor, "ACTIVE", "key-alpha", false)
	f.betaID = seed("filter-beta", filterProject, f.betaImage, f.betaFlavor, "ERROR", "key-beta", true)
	f.otherID = seed("filter-alpha", filterOtherProject, f.alphaImage, f.alphaFlavor, "ACTIVE", "key-alpha", false)

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM instances WHERE project_id = ANY($1)`,
			[]string{filterProject, filterOtherProject})
	})
	return f
}

// listedNames issues a listing and returns the server names it contains.
func listedNames(t *testing.T, path, project string, admin bool) []string {
	t.Helper()
	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": project, "X-Test-User": seededUserID}
	if admin {
		hdr["X-Test-Roles"] = "admin"
	} else {
		hdr["X-Test-Roles"] = "member"
	}
	code, body := doReq(r, http.MethodGet, path, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("GET %s: code=%d body=%s; want 200", path, code, body)
	}
	var resp struct {
		Servers []struct{ Name string } `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("GET %s: unmarshalling %s: %v", path, body, err)
	}
	names := make([]string, 0, len(resp.Servers))
	for _, s := range resp.Servers {
		names = append(names, s.Name)
	}
	return names
}

// hasName is defined in server_detail_secgroups_test.go — same package, same need.

// TestServerListFiltersMatchAndReject is T007 + T008: every honoured filter, asserted
// in BOTH directions, over both the brief and the detailed listing (FR-001, FR-002,
// FR-003, FR-009).
//
// The `wantEmpty` case is the load-bearing one. Against the pre-change code every
// `wantNames` case passes — the filter is ignored, the whole project comes back, and the
// expected server is in it — so a suite containing only those would have gone green
// against the defect it was written to catch.
func TestServerListFiltersMatchAndReject(t *testing.T) {
	requireDB(t)
	f := seedFilterFixtures(t)

	changesSince := f.beforeAll.Format(time.RFC3339)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	for _, tc := range []struct {
		filter    string
		match     string // query that must return exactly filter-alpha
		miss      string // query that must return NOTHING
		adminOnly bool
	}{
		{filter: "name", match: "name=filter-alpha", miss: "name=filter-nonexistent"},
		{filter: "status", match: "status=ACTIVE", miss: "status=SUSPENDED"},
		{filter: "image", match: "image=" + f.alphaImage, miss: "image=00000000-0000-0000-0000-0000000000ee"},
		{filter: "changes-since", match: "changes-since=" + changesSince, miss: "changes-since=" + future},
		{filter: "key_name", match: "key_name=key-alpha", miss: "key_name=key-nonexistent", adminOnly: true},
		{filter: "user_id", match: "user_id=" + seededUserID, miss: "user_id=" + filterOtherProject, adminOnly: true},
	} {
		for _, path := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
			t.Run(tc.filter+" matches ("+path+")", func(t *testing.T) {
				names := listedNames(t, path+"?"+tc.match, filterProject, tc.adminOnly)
				if !hasName(names, "filter-alpha") {
					t.Errorf("?%s returned %v; want it to contain filter-alpha", tc.match, names)
				}
			})
			t.Run(tc.filter+" excludes non-matching ("+path+")", func(t *testing.T) {
				names := listedNames(t, path+"?"+tc.miss, filterProject, tc.adminOnly)
				if len(names) != 0 {
					t.Errorf("?%s returned %v; want an EMPTY collection — an ignored filter returns the project's servers", tc.miss, names)
				}
			})
		}
	}
}

// TestServerListFilterNarrowsWithinProject is T007's discrimination check: a filter must
// select a subset, not merely be syntactically present. `status=ERROR` must return beta
// and NOT alpha.
func TestServerListFilterNarrowsWithinProject(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	names := listedNames(t, "/v2.1/servers?status=ERROR", filterProject, false)
	if !hasName(names, "filter-beta") {
		t.Errorf("?status=ERROR returned %v; want filter-beta", names)
	}
	if hasName(names, "filter-alpha") {
		t.Errorf("?status=ERROR returned %v; filter-alpha is ACTIVE and must be excluded", names)
	}
}

// TestServerListIPFilterMatches: the ?ip= filter is resolved in Go against the scope's
// port addresses (research R8). A server whose port holds the address is returned; the
// address of no port returns nothing.
func TestServerListIPFilterMatches(t *testing.T) {
	requireDB(t)
	f := seedFilterFixtures(t)
	ctx := context.Background()
	var portID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO ports (project_id, device_id, device_owner, fixed_ips) VALUES ($1, $2, 'compute:nova', $3::jsonb) RETURNING id`,
		filterProject, f.alphaID, `[{"ip_address":"10.11.12.13"}]`).Scan(&portID); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM ports WHERE id = $1`, portID) })

	if names := listedNames(t, "/v2.1/servers?ip=10.11.12.13", filterProject, false); !hasName(names, "filter-alpha") {
		t.Errorf("?ip=10.11.12.13 returned %v; want filter-alpha", names)
	}
	if names := listedNames(t, "/v2.1/servers?ip=10.11.12.99", filterProject, false); len(names) != 0 {
		t.Errorf("?ip=10.11.12.99 returned %v; want an empty collection (no port holds it)", names)
	}
}

// TestServerListNameIsRegex is T009: FR-003a pins upstream's semantics, which are
// "exact and regex matching" — not a substring match and not equality.
func TestServerListNameIsRegex(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	// A pattern matching both fixtures.
	names := listedNames(t, "/v2.1/servers?name=^filter-", filterProject, false)
	if !hasName(names, "filter-alpha") || !hasName(names, "filter-beta") {
		t.Errorf("?name=^filter- returned %v; want both fixtures", names)
	}

	// An anchored pattern matching exactly one.
	names = listedNames(t, "/v2.1/servers?name=^filter-alpha$", filterProject, false)
	if !hasName(names, "filter-alpha") || hasName(names, "filter-beta") {
		t.Errorf("?name=^filter-alpha$ returned %v; want only filter-alpha", names)
	}

	// A pattern that cannot match.
	names = listedNames(t, "/v2.1/servers?name=^zzz", filterProject, false)
	if len(names) != 0 {
		t.Errorf("?name=^zzz returned %v; want empty", names)
	}
}

// TestServerListInvalidFilterValue is T017's guard (FR-008): a value that is invalid for
// its column must not surface as a server fault. A malformed regex and a non-UUID flavor
// are the two shapes that reach the database as errors if unguarded.
func TestServerListInvalidFilterValue(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)
	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": filterProject, "X-Test-User": seededUserID, "X-Test-Roles": "member"}

	for _, q := range []string{"name=%5B", "flavor=not-a-uuid", "image=not-a-uuid", "changes-since=not-a-date"} {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers?"+q, "", hdr)
		if code >= 500 {
			t.Errorf("?%s: code=%d body=%s; a bad filter value must never be a server fault", q, code, body)
		}
	}
}

// TestServerListFilterCannotWidenScope is T012: FR-006 and FR-006a. A filter narrows;
// only the administrator-only opt-in widens.
func TestServerListFilterCannotWidenScope(t *testing.T) {
	requireDB(t)
	f := seedFilterFixtures(t)

	// A member naming another project sees only their own.
	names := listedNames(t, "/v2.1/servers?project_id="+filterOtherProject, filterProject, false)
	for _, n := range names {
		if n == "filter-alpha" {
			continue // their own, same name as the other project's — legitimate
		}
		if n != "filter-beta" {
			t.Errorf("member filtering by another project saw %v; want only their own servers", names)
		}
	}
	if len(names) == 0 {
		t.Error("member filtering by another project got nothing; their own servers should still be scoped in")
	}

	// An administrator naming another project WITHOUT all_tenants gets an empty
	// collection: scope ∩ filter. The filter must not imply the opt-in.
	names = listedNames(t, "/v2.1/servers?project_id="+filterOtherProject, filterProject, true)
	if len(names) != 0 {
		t.Errorf("admin filtering by another project without all_tenants returned %v; want empty — a filter must not imply the opt-in", names)
	}

	// With the opt-in, the same filter narrows the cross-tenant view to that project.
	names = listedNames(t, "/v2.1/servers?all_tenants=1&project_id="+filterOtherProject, filterProject, true)
	if len(names) != 1 || names[0] != "filter-alpha" {
		t.Errorf("admin with all_tenants filtering by project returned %v; want exactly the other project's one server", names)
	}
	_ = f
}

// TestServerListRefusedFilters is T010: FR-004's enumerated set. The product records
// none of these attributes, so each is refused naming the filter rather than answered
// with a list. `ip` is in the set until FR-003b's condition is met.
func TestServerListRefusedFilters(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)
	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": filterProject, "X-Test-User": seededUserID, "X-Test-Roles": "admin"}

	// `ip` is deliberately ABSENT from this list: FR-003b promoted it to the honoured set
	// once US7's ports query existed. TestServerListIPFilter covers it.
	for _, tc := range []struct{ param, value string }{
		{"host", "compute-01"},
		{"hostname", "web-1"},
		{"availability_zone", "nova"},
		{"tags", "prod"},
	} {
		for _, path := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
			code, body := doReq(r, http.MethodGet, path+"?"+tc.param+"="+tc.value, "", hdr)
			if code != http.StatusBadRequest {
				t.Errorf("GET %s?%s=%s: code=%d body=%s; want 400 — an unrecordable attribute must be refused, not answered with a list",
					path, tc.param, tc.value, code, body)
			}
			if !strings.Contains(body, tc.param) {
				t.Errorf("GET %s?%s=%s: body=%s; the refusal must name the filter", path, tc.param, tc.value, body)
			}
		}
	}
}

// TestServerListIgnoresUnknownParameters is T011: FR-004a bounds the refusal to the
// enumerated set. Refusing everything unrecognised would newly break every client that
// sends a benign extra — paging, sorting and the deleted flag among them.
func TestServerListIgnoresUnknownParameters(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	for _, q := range []string{"sort_key=created_at", "sort_dir=desc", "deleted=false", "some_future_param=x"} {
		names := listedNames(t, "/v2.1/servers?"+q, filterProject, false)
		if len(names) == 0 {
			t.Errorf("?%s returned nothing; an unrecognised parameter must be ignored, not treated as a filter", q)
		}
	}
}

// TestServerListFilterWithPaging is T013: FR-005. The marker lookup must run under the
// same predicates as the page query, or a filtered page boundary silently restarts.
func TestServerListFilterWithPaging(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	// Both fixtures match; page through them one at a time and assert no repeat.
	first := listedNames(t, "/v2.1/servers?name=^filter-&limit=1", filterProject, false)
	if len(first) != 1 {
		t.Fatalf("first filtered page = %v; want exactly 1 server", first)
	}

	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": filterProject, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	_, body := doReq(r, http.MethodGet, "/v2.1/servers?name=^filter-&limit=1", "", hdr)
	var page struct {
		Servers []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("unmarshalling first page %s: %v", body, err)
	}
	marker := page.Servers[0].ID

	second := listedNames(t, fmt.Sprintf("/v2.1/servers?name=^filter-&limit=1&marker=%s", marker), filterProject, false)
	if len(second) != 1 {
		t.Fatalf("second filtered page = %v; want exactly 1 server", second)
	}
	if second[0] == first[0] {
		t.Errorf("filtered paging repeated %q; the marker must advance past it", second[0])
	}
}

// TestServerListBriefAndDetailAgree is T014: FR-002. A client must not get different
// rows for one query depending on which representation it asked for.
func TestServerListBriefAndDetailAgree(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	for _, q := range []string{"", "?name=filter-alpha", "?status=ERROR", "?name=filter-nonexistent"} {
		brief := listedNames(t, "/v2.1/servers"+q, filterProject, false)
		detail := listedNames(t, "/v2.1/servers/detail"+q, filterProject, false)
		if len(brief) != len(detail) {
			t.Errorf("query %q: brief=%v detail=%v; the two listings must return the same servers", q, brief, detail)
			continue
		}
		for _, n := range brief {
			if !hasName(detail, n) {
				t.Errorf("query %q: %q in brief but not in detail", q, n)
			}
		}
	}
}

// TestServerListMarkerParityAcrossListings extends T014 to the marker path: the two
// listings use different paging mechanisms internally — a ROW_NUMBER offset in the brief
// listing, a (created_at, id) keyset in the detailed one — so "both honour the same
// filters" has to be asserted with a marker present, not only without one.
func TestServerListMarkerParityAcrossListings(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": filterProject, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	_, body := doReq(r, http.MethodGet, "/v2.1/servers?name=^filter-&limit=1", "", hdr)
	var page struct {
		Servers []struct{ ID string } `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("unmarshalling %s: %v", body, err)
	}
	marker := page.Servers[0].ID

	q := fmt.Sprintf("?name=^filter-&limit=1&marker=%s", marker)
	brief := listedNames(t, "/v2.1/servers"+q, filterProject, false)
	detail := listedNames(t, "/v2.1/servers/detail"+q, filterProject, false)
	if len(brief) != len(detail) {
		t.Fatalf("filtered+marker: brief=%v detail=%v; the two listings must agree", brief, detail)
	}
	for i := range brief {
		if brief[i] != detail[i] {
			t.Errorf("filtered+marker: brief=%v detail=%v; the two listings must agree", brief, detail)
		}
	}
}

// TestServerListUnfilteredUnchanged is T015: FR-007. This is a CONTROL — it passes
// before and after the change, and its job is to catch a filter implementation that
// alters the unfiltered path.
func TestServerListUnfilteredUnchanged(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	names := listedNames(t, "/v2.1/servers", filterProject, false)
	if len(names) != 2 || !hasName(names, "filter-alpha") || !hasName(names, "filter-beta") {
		t.Errorf("unfiltered listing = %v; want exactly the project's two servers", names)
	}
}

// ---------------------------------------------------------------------------
// The three filter sets, and the refusal message derived from them (branch review SRC-8).
//
// `ip` spent the first part of this feature in refusedListFilters and the rest of it
// honoured. The refusal message was a hand-written copy of the honoured list and was not
// updated when it moved, so the error whose job is to say what a caller CAN filter on was
// telling it `ip` was unavailable. These tests pin the sets against each other and against
// what the routes actually answer, which is what makes another such move visible.

// TestFilterSetsAreDisjoint is a pure check: no filter may appear in two of the three
// sets. A name in both refusedListFilters and honouredListFilters would be refused while
// being advertised as supported, in the same message.
func TestFilterSetsAreDisjoint(t *testing.T) {
	sets := map[string][]string{
		"refused":  refusedListFilters,
		"honoured": honouredListFilters,
		"admin":    adminListFilters,
	}
	for aName, a := range sets {
		for bName, b := range sets {
			if aName >= bName {
				continue
			}
			for _, x := range a {
				for _, y := range b {
					if x == y {
						t.Errorf("%q appears in both %s and %s; a filter cannot be in two sets", x, aName, bName)
					}
				}
			}
		}
	}
}

// TestRefusedFiltersAreRefusedAndHonouredOnesAreNot is the drift gate. Every name in
// refusedListFilters must produce a 400, and every name in honouredListFilters must NOT —
// so moving a filter between the sets without changing its handling fails here.
func TestRefusedFiltersAreRefusedAndHonouredOnesAreNot(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := map[string]string{
		"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	for _, name := range refusedListFilters {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers?"+name+"=anything", "", hdr)
		if code != http.StatusBadRequest {
			t.Errorf("?%s=anything: code=%d body=%s; want 400 — it is in refusedListFilters", name, code, body)
		}
	}

	// A value that is valid for each honoured filter, so a 400 can only mean the filter
	// itself was rejected.
	values := map[string]string{
		"name":          "anything",
		"status":        "ACTIVE",
		"image":         "not-a-uuid",
		"flavor":        "not-a-uuid",
		"changes-since": "2020-01-01T00:00:00Z",
		"ip":            "10.0.0.1",
	}
	for _, name := range honouredListFilters {
		v, ok := values[name]
		if !ok {
			t.Fatalf("honouredListFilters names %q but this test has no valid value for it; add one rather than skipping", name)
		}
		code, body := doReq(r, http.MethodGet, "/v2.1/servers?"+name+"="+v, "", hdr)
		if code == http.StatusBadRequest {
			t.Errorf("?%s=%s: code=400 body=%s; it is in honouredListFilters and must not be refused", name, v, body)
		}
	}
}

// TestRefusalNamesWhatTheCallerCanActuallyUse pins the message content, in both
// directions, by EXACT SET comparison rather than substring search.
//
// Substring search would be the wrong tool and quietly so: strings.Contains(body, "ip")
// matches "ip" inside any longer word, so the assertion for the shortest filter name
// would pass on a message that never mentioned it. Parsing the advertised list and
// comparing sets is what makes the check able to fail.
//
// The administrator-only filters are named to an administrator and NOT to anyone else:
// applyAdminFilters ignores rather than refuses them for other callers, precisely so their
// existence is not advertised, and a message listing them to every tenant would undo that.
func TestRefusalNamesWhatTheCallerCanActuallyUse(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	// advertised returns the filter names the refusal claims are supported.
	advertised := func(t *testing.T, body string) []string {
		t.Helper()
		var envelope struct {
			BadRequest struct {
				Details string `json:"details"`
			} `json:"badRequest"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("refusal body is not the expected envelope: %v (%s)", err, body)
		}
		const marker = "Supported filters: "
		i := strings.Index(envelope.BadRequest.Details, marker)
		if i < 0 {
			t.Fatalf("the refusal does not advertise a filter list at all: %q", envelope.BadRequest.Details)
		}
		list := strings.TrimSuffix(strings.TrimSpace(envelope.BadRequest.Details[i+len(marker):]), ".")
		names := strings.Split(list, ", ")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		return names
	}

	for _, tc := range []struct {
		role string
		want []string
	}{
		{"member", honouredListFilters},
		{"admin", append(append([]string{}, honouredListFilters...), adminListFilters...)},
	} {
		t.Run(tc.role, func(t *testing.T) {
			hdr := map[string]string{
				"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": tc.role,
			}
			code, body := doReq(r, http.MethodGet, "/v2.1/servers?tags=x", "", hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s; want 400", code, body)
			}

			got := advertised(t, body)
			gotSet := map[string]bool{}
			for _, n := range got {
				gotSet[n] = true
			}
			wantSet := map[string]bool{}
			for _, n := range tc.want {
				wantSet[n] = true
			}

			for n := range wantSet {
				if !gotSet[n] {
					t.Errorf("the %s refusal does not advertise %q, which this caller may use; advertised %v",
						tc.role, n, got)
				}
			}
			for n := range gotSet {
				if !wantSet[n] {
					if tc.role == "member" {
						t.Errorf("a non-administrator's refusal advertises %q; the gated filters are ignored "+
							"rather than refused so their existence is not advertised. Advertised %v", n, got)
					} else {
						t.Errorf("the %s refusal advertises %q, which is in no filter set; advertised %v",
							tc.role, n, got)
					}
				}
			}
		})
	}
}

// TestServerListNameFollowsTheDatabasesDialect: name is matched by the database, as
// upstream matches it by its own (nova/db/main/api.py), so an escape the dialects read
// differently follows the backend. `\b` is a backspace to PostgreSQL and a word boundary
// to the regular expressions SQLite is given.
func TestServerListNameFollowsTheDatabasesDialect(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)

	names := listedNames(t, "/v2.1/servers?name=%5Cbfilter-alpha", filterProject, false)
	if testPool.Backend() == database.SQLite {
		if !hasName(names, "filter-alpha") || hasName(names, "filter-beta") {
			t.Errorf(`SQLite: ?name=\bfilter-alpha returned %v; want filter-alpha (\b is a word boundary)`, names)
		}
	} else if len(names) != 0 {
		t.Errorf(`PostgreSQL: ?name=\bfilter-alpha returned %v; want none (\b is a backspace)`, names)
	}
}
