package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// SPEC-023 US6: surfaces that describe or reshape the DEPLOYMENT, not a project.
//
// These encode contracts/operator-surfaces.md O1–O4. They have no owning
// project, so the owner-or-administrator rule has nothing to fall back to —
// access is administrative or nothing.
//
// Host aggregates are the sharp one: nine handlers with no role check at all,
// six of them writes. Before SPEC-023 any authenticated tenant could create,
// rename and delete aggregates and move hypervisors between them. That is the
// only place in this feature where a tenant could *change* something that is
// not theirs.

func operatorHdr(role string) map[string]string {
	return map[string]string{
		"X-Test-Project": authzProjectA,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   role,
	}
}

// seedAggregate creates one aggregate and returns its id, so the show/update/
// delete/action paths address something real rather than 404ing for the wrong
// reason.
func seedAggregate(t *testing.T) string {
	t.Helper()
	var id string
	// Routes address aggregates by their uuid column, not the integer PK.
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO host_aggregates (name, availability_zone) VALUES ('spec023-agg', 'nova')
		 RETURNING uuid::text`).Scan(&id); err != nil {
		t.Fatalf("seeding aggregate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM host_aggregates WHERE name = 'spec023-agg'`)
	})
	return id
}

// TestAggregateWritesRequireAdmin: O3 — the privilege escalation. Every one of
// these succeeded for any authenticated tenant before SPEC-023.
func TestAggregateWritesRequireAdmin(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	id := seedAggregate(t)
	r := newNovaTestRouter()
	member := operatorHdr("member")

	for _, tc := range []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/v2.1/os-aggregates", `{"aggregate":{"name":"pwn","availability_zone":"nova"}}`},
		{"update", http.MethodPut, "/v2.1/os-aggregates/" + id, `{"aggregate":{"name":"renamed"}}`},
		{"delete", http.MethodDelete, "/v2.1/os-aggregates/" + id, ""},
		{"add_host", http.MethodPost, "/v2.1/os-aggregates/" + id + "/action", `{"add_host":{"host":"h1"}}`},
		{"remove_host", http.MethodPost, "/v2.1/os-aggregates/" + id + "/action", `{"remove_host":{"host":"h1"}}`},
		{"set_metadata", http.MethodPost, "/v2.1/os-aggregates/" + id + "/action", `{"set_metadata":{"metadata":{"k":"v"}}}`},
		// The dispatcher's unrecognised-action branch must be gated too: a gate
		// placed only on the three recognised actions would leave this open.
		{"unknown action", http.MethodPost, "/v2.1/os-aggregates/" + id + "/action", `{"no_such_action":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, tc.method, tc.path, tc.body, member)
			if code != http.StatusForbidden {
				t.Errorf("member %s: code=%d, want 403. body: %s", tc.name, code, body)
			}
		})
	}
}

// TestAggregateReadsRequireAdmin: O2. Reads are gated too — aggregate topology
// tells a tenant how the deployment schedules, which is operator information.
// Upstream gates the whole policy family.
func TestAggregateReadsRequireAdmin(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	id := seedAggregate(t)
	r := newNovaTestRouter()
	member := operatorHdr("member")

	for _, tc := range []struct{ name, path string }{
		{"list", "/v2.1/os-aggregates"},
		{"show", "/v2.1/os-aggregates/" + id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, tc.path, "", member)
			if code != http.StatusForbidden {
				t.Errorf("member %s: code=%d, want 403. body: %s", tc.name, code, body)
			}
		})
	}
}

// TestAggregateAdminUnchanged: O5. Adding the gate must not break the operator
// path it exists to protect.
func TestAggregateAdminUnchanged(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	id := seedAggregate(t)
	r := newNovaTestRouter()
	admin := operatorHdr("admin")

	for _, tc := range []struct{ name, path string }{
		{"list", "/v2.1/os-aggregates"},
		{"show", "/v2.1/os-aggregates/" + id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, tc.path, "", admin)
			if code != http.StatusOK {
				t.Errorf("admin %s: code=%d, want 200. body: %s", tc.name, code, body)
			}
		})
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/os-aggregates",
		`{"aggregate":{"name":"spec023-agg-2","availability_zone":"nova"}}`, admin)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Errorf("admin create: code=%d, want 200/201. body: %s", code, body)
	}
	_, _ = testPool.Exec(context.Background(), `DELETE FROM host_aggregates WHERE name = 'spec023-agg-2'`)
}

// TestMigrationListRequiresAdmin: O1. The deployment-wide list reports every
// project's instance movements — server uuid, source and destination node,
// flavour changes — and was readable by any authenticated tenant. The handler's
// own comment already said "admin endpoint in real OpenStack".
func TestMigrationListRequiresAdmin(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	r := newNovaTestRouter()

	code, body := doReq(r, http.MethodGet, "/v2.1/os-migrations", "", operatorHdr("member"))
	if code != http.StatusForbidden {
		t.Errorf("member listing all migrations: code=%d, want 403. body: %s", code, body)
	}

	code, body = doReq(r, http.MethodGet, "/v2.1/os-migrations", "", operatorHdr("admin"))
	if code != http.StatusOK {
		t.Errorf("admin listing all migrations: code=%d, want 200. body: %s", code, body)
	}
}

// TestServerMigrationsAreOwnershipScoped: O4. The four per-server migration
// handlers had NO project predicate and NO role check, so any tenant could read,
// act on and DELETE another tenant's server migration records. Unlike the
// aggregate surfaces these have an owner, so they take the ordinary override —
// and 404 rather than 403, per the disclosure rule.
func TestServerMigrationsAreOwnershipScoped(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	memberInA := operatorHdr("member")

	// A REAL migration record for B's server. Without it every path 404s because
	// the record is absent, which would pass for the wrong reason and prove
	// nothing about ownership.
	var migID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO server_migrations (server_uuid, source_node, dest_node, status)
		 VALUES ($1, 'src', 'dst', 'running') RETURNING id::text`, idB).Scan(&migID); err != nil {
		t.Fatalf("seeding migration for B's server: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM server_migrations WHERE id = $1`, migID)
	})

	for _, tc := range []struct{ name, method, path, body string }{
		{"list", http.MethodGet, "/v2.1/servers/" + idB + "/migrations", ""},
		{"show", http.MethodGet, "/v2.1/servers/" + idB + "/migrations/" + migID, ""},
		{"delete", http.MethodDelete, "/v2.1/servers/" + idB + "/migrations/" + migID, ""},
		{"action", http.MethodPost, "/v2.1/servers/" + idB + "/migrations/" + migID + "/action", `{"force_complete":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, tc.method, tc.path, tc.body, memberInA)
			if code != http.StatusNotFound {
				t.Errorf("member in A, %s on B's server migrations: code=%d, want 404. body: %s",
					tc.name, code, body)
			}
		})
	}

	// The owner still reaches their own.
	ownerB := map[string]string{
		"X-Test-Project": authzProjectB,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "member",
	}
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idB+"/migrations", "", ownerB)
	if code != http.StatusOK {
		t.Errorf("owner listing their own server's migrations: code=%d, want 200. body: %s", code, body)
	}
}

// TestListServersAllTenants: contracts L1–L4. The default view must not change
// for anyone — that is the compatibility guarantee — and widening happens only
// on the explicit opt-in, only for an administrator.
func TestListServersAllTenants(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	admin := operatorHdr("admin")
	member := operatorHdr("member")

	ids := func(body string) map[string]bool {
		out := map[string]bool{}
		for _, id := range []string{idA, idB} {
			if strings.Contains(body, id) {
				out[id] = true
			}
		}
		return out
	}

	t.Run("default view is unchanged for an admin", func(t *testing.T) {
		_, body := doReq(r, http.MethodGet, "/v2.1/servers", "", admin)
		got := ids(body)
		if !got[idA] || got[idB] {
			t.Errorf("admin default listing: A=%v B=%v; want only A. body: %s", got[idA], got[idB], body)
		}
	})

	// Every spelling upstream accepts, including the two a naive
	// strconv.ParseBool implementation would silently drop.
	for _, q := range []string{"all_tenants=1", "all_tenants=true", "all_tenants=True",
		"all_tenants=yes", "all_tenants=on", "all_tenants", "all_tenants="} {
		t.Run("admin widens with ?"+q, func(t *testing.T) {
			_, body := doReq(r, http.MethodGet, "/v2.1/servers?"+q, "", admin)
			got := ids(body)
			if !got[idA] || !got[idB] {
				t.Errorf("?%s: A=%v B=%v; want both. body: %s", q, got[idA], got[idB], body)
			}
		})
	}

	for _, q := range []string{"all_tenants=0", "all_tenants=false", "all_tenants=no",
		"all_tenants=off", "all_tenants=banana"} {
		t.Run("admin stays narrow with ?"+q, func(t *testing.T) {
			_, body := doReq(r, http.MethodGet, "/v2.1/servers?"+q, "", admin)
			if ids(body)[idB] {
				t.Errorf("?%s widened the listing; want only the caller's project. body: %s", q, body)
			}
		})
	}

	t.Run("non-admin opt-in is ignored, not refused", func(t *testing.T) {
		codeWith, bodyWith := doReq(r, http.MethodGet, "/v2.1/servers?all_tenants=1", "", member)
		codeWithout, bodyWithout := doReq(r, http.MethodGet, "/v2.1/servers", "", member)
		if codeWith != http.StatusOK {
			t.Fatalf("member with all_tenants: code=%d, want 200 (ignored, never refused)", codeWith)
		}
		if bodyWith != bodyWithout {
			t.Errorf("member's answer differs with and without all_tenants — that reveals the parameter is gated.\nwith:    %s\nwithout: %s", bodyWith, bodyWithout)
		}
		if codeWith != codeWithout {
			t.Errorf("codes differ: %d vs %d", codeWith, codeWithout)
		}
	})

	t.Run("entries carry their owning project", func(t *testing.T) {
		_, body := doReq(r, http.MethodGet, "/v2.1/servers?all_tenants=1", "", admin)
		if !strings.Contains(body, authzProjectA) || !strings.Contains(body, authzProjectB) {
			t.Errorf("cross-tenant listing must name each entry's owner; body: %s", body)
		}
	})
}

// TestListServersDetailAllTenants: the detail listing pages by a different
// mechanism than the summary listing (a created_at comparison rather than
// ROW_NUMBER), so it needs its own coverage — the two disagreeing about scope is
// exactly how a cross-tenant marker silently returned page 1.
func TestListServersDetailAllTenants(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	admin := operatorHdr("admin")

	_, narrow := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", admin)
	if !strings.Contains(narrow, idA) || strings.Contains(narrow, idB) {
		t.Errorf("default detail listing must stay the caller's own project. body: %s", narrow)
	}

	_, wide := doReq(r, http.MethodGet, "/v2.1/servers/detail?all_tenants=1", "", admin)
	if !strings.Contains(wide, idA) || !strings.Contains(wide, idB) {
		t.Errorf("?all_tenants detail listing must span projects. body: %s", wide)
	}

	// A marker naming another project's server must resolve under the widened
	// view rather than being silently ignored.
	code, paged := doReq(r, http.MethodGet,
		"/v2.1/servers/detail?all_tenants=1&limit=1&marker="+idB, "", admin)
	if code != http.StatusOK {
		t.Fatalf("marker page: code=%d. body: %s", code, paged)
	}
	if strings.Contains(paged, idB) {
		t.Errorf("marker page still contains the marker itself — the marker did not resolve. body: %s", paged)
	}
}

// TestListServersPagingIsStable: L5. Walking the pages must yield every server
// exactly once, including when several share a created_at — the tie case the
// composite ordering exists for.
func TestListServersPagingIsStable(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	ctx := context.Background()
	// Five servers with an IDENTICAL created_at, so ordering by created_at alone
	// would be non-deterministic across pages.
	for i := range 5 {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO instances (name, project_id, status, created_at)
			 VALUES ($1, $2, 'ACTIVE', '2026-01-01 00:00:00')`,
			fmt.Sprintf("tie-%d", i), authzProjectA); err != nil {
			t.Fatalf("seeding tie instance: %v", err)
		}
	}
	r := newNovaTestRouter()
	admin := operatorHdr("admin")

	seen := map[string]int{}
	marker := ""
	for range 8 { // bounded: 5 tie servers + web-1, two per page
		q := "/v2.1/servers?limit=2"
		if marker != "" {
			q += "&marker=" + marker
		}
		code, body := doReq(r, http.MethodGet, q, "", admin)
		if code != http.StatusOK {
			t.Fatalf("paging: code=%d body=%s", code, body)
		}
		var got struct {
			Servers []struct{ ID string } `json:"servers"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Servers) == 0 {
			break
		}
		for _, srv := range got.Servers {
			seen[srv.ID]++
		}
		marker = got.Servers[len(got.Servers)-1].ID
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("server %s appeared %d times across pages; want exactly once", id, n)
		}
	}
	if len(seen) < 6 {
		t.Errorf("saw %d distinct servers across pages; want at least 6 (5 ties + web-1)", len(seen))
	}
}

// TestListMarkerOutsideViewIsRefused: L6. A marker the caller cannot see —
// another project's server, one that does not exist, or a malformed reference —
// was logged and ignored, so the listing silently restarted at page 1. It is
// refused with the 400 upstream gives an unknown marker, and identically whether
// or not the foreign server exists.
func TestListMarkerOutsideViewIsRefused(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	member := operatorHdr("member") // scoped to project A
	const absent = "11111111-1111-1111-1111-111111111111"

	for _, list := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
		t.Run(list, func(t *testing.T) {
			bodies := map[string]string{}
			for _, marker := range []string{idB, absent, "not-a-uuid"} {
				code, body := doReq(r, http.MethodGet, list+"?limit=5&marker="+marker, "", member)
				if code != http.StatusBadRequest {
					t.Errorf("marker %s: code=%d, want 400. body: %s", marker, code, body)
				}
				bodies[marker] = strings.ReplaceAll(body, marker, "<marker>")
			}
			if bodies[idB] != bodies[absent] {
				t.Errorf("a foreign marker is answered differently from an absent one, which "+
					"discloses that the server exists:\n%s\n%s", bodies[idB], bodies[absent])
			}
			if code, body := doReq(r, http.MethodGet, list+"?limit=5&marker="+idA, "", member); code != http.StatusOK {
				t.Errorf("own marker: code=%d, want 200. body: %s", code, body)
			}
		})
	}
}

// TestTenantUsageConfidentiality: US3 / contracts U1–U3. The aggregate form
// reported every project's consumption to any authenticated caller, and the
// per-project form took its `:id` straight from the path — so a tenant could
// read a specific named project's usage just by knowing its id.
func TestTenantUsageConfidentiality(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	r := newNovaTestRouter()
	member := operatorHdr("member") // scoped to project A
	admin := operatorHdr("admin")

	t.Run("aggregate is admin-only", func(t *testing.T) {
		if code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage", "", member); code != http.StatusForbidden {
			t.Errorf("member reading every project's usage: code=%d, want 403. body: %s", code, body)
		}
		if code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage", "", admin); code != http.StatusOK {
			t.Errorf("admin reading the aggregate: code=%d, want 200. body: %s", code, body)
		}
	})

	t.Run("member naming any other project is refused, the same way every time", func(t *testing.T) {
		// Foreign, nonexistent and malformed answer alike (U4): a 404 for one and
		// a 403 for another would tell a tenant which projects exist. Answering 200
		// with the caller's OWN usage instead is the silent substitution FR-008d
		// forbids.
		var first string
		for i, ref := range []string{authzProjectB, "00000000-0000-0000-0000-999999999999", "not-a-uuid"} {
			code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+ref, "", member)
			if code != http.StatusForbidden {
				t.Errorf("member naming %q: code=%d, want 403. body: %s", ref, code, body)
			}
			if !strings.Contains(body, "os-simple-tenant-usage:show") {
				t.Errorf("member naming %q: the refusal should name the usage policy. body: %s", ref, body)
			}
			if i == 0 {
				first = body
			} else if body != first {
				t.Errorf("member naming %q answered %s, unlike %s for a project that exists", ref, body, first)
			}
		}
		if code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+authzProjectA, "", member); code != http.StatusOK {
			t.Errorf("member reading their own usage: code=%d, want 200. body: %s", code, body)
		}
	})

	t.Run("admin may name any project", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+authzProjectB, "", admin)
		if code != http.StatusOK || !strings.Contains(body, authzProjectB) {
			t.Errorf("admin reading B's usage: code=%d body=%s; want 200 describing B", code, body)
		}
	})

	t.Run("bad targets are refused, not zeroed", func(t *testing.T) {
		for _, ref := range []string{"not-a-uuid", "00000000-0000-0000-0000-999999999999"} {
			code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+ref, "", admin)
			if code == http.StatusOK {
				t.Errorf("target %q: code=200 body=%s; want a client error — zeroes are "+
					"indistinguishable from a real empty project", ref, body)
			}
			if code >= 500 {
				t.Errorf("target %q: code=%d; want a client error, not a server error", ref, code)
			}
		}
	})
}
