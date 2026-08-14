package keystone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-023 US5: contracts F1–F4. Six identity collections discarded every query
// parameter and returned the whole collection, which is indistinguishable from a
// successful match to any caller that reads the first entry.

func countIn(t *testing.T, body, key string) int {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %s: %v (body %s)", key, err, body)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(m[key], &arr); err != nil {
		t.Fatalf("decode %s array: %v", key, err)
	}
	return len(arr)
}

// TestListFiltersApply: F1. The role case is the one that mattered — the
// multi-tenancy suite asked for the member role by name, got all three, and took
// the first, which sorts to "admin".
func TestListFiltersApply(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}

	for _, tc := range []struct{ name, path, unfiltered, key string }{
		{"roles by name", "/v3/roles?name=member", "/v3/roles", "roles"},
		{"users by name", "/v3/users?name=admin", "/v3/users", "users"},
		{"projects by name", "/v3/projects?name=default", "/v3/projects", "projects"},
		{"services by type", "/v3/services?type=identity", "/v3/services", "services"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codeAll, all := doReq(r, http.MethodGet, tc.unfiltered, admin)
			codeOne, one := doReq(r, http.MethodGet, tc.path, admin)
			if codeAll != http.StatusOK || codeOne != http.StatusOK {
				t.Fatalf("codes %d / %d; want 200. bodies: %s | %s", codeAll, codeOne, all, one)
			}
			nAll, nOne := countIn(t, all, tc.key), countIn(t, one, tc.key)
			if nAll <= 1 {
				t.Skipf("only %d %s seeded — the filter cannot be distinguished from the full list", nAll, tc.key)
			}
			if nOne >= nAll {
				t.Errorf("filtered returned %d of %d — the filter was discarded", nOne, nAll)
			}
		})
	}
}

// TestListFilterNoMatchIsEmpty: F2. Returning the FULL collection for a filter
// that matches nothing is the specific failure being fixed.
func TestListFilterNoMatchIsEmpty(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}

	for _, tc := range []struct{ path, key string }{
		{"/v3/roles?name=no-such-role", "roles"},
		{"/v3/users?name=no-such-user", "users"},
		{"/v3/projects?name=no-such-project", "projects"},
		{"/v3/groups?name=no-such-group", "groups"},
		{"/v3/services?type=no-such-type", "services"},
		{"/v3/endpoints?interface=no-such-interface", "endpoints"},
		// Not a UUID, on a uuid column: the value reached the column, Postgres
		// refused the cast (22P02), and these answered 500. groups.domain_id
		// joined them when migration 072 made it a uuid.
		{"/v3/users?domain_id=no-such-domain", "users"},
		{"/v3/projects?domain_id=no-such-domain", "projects"},
		{"/v3/groups?domain_id=no-such-domain", "groups"},
		{"/v3/endpoints?service_id=no-such-service", "endpoints"},
		// No project hierarchy: a project is nobody's parent, so a project id
		// matches nothing — and neither does a value that names nothing at all.
		{"/v3/projects?parent_id=00000000-0000-0000-0000-000000000002", "projects"},
		{"/v3/projects?parent_id=no-such-parent", "projects"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, tc.path, admin)
			if code != http.StatusOK {
				t.Fatalf("code=%d body=%s; want 200 with an empty collection", code, body)
			}
			if n := countIn(t, body, tc.key); n != 0 {
				t.Errorf("returned %d %s for a filter that matches nothing; want 0", n, tc.key)
			}
		})
	}
}

// TestProjectParentIsItsDomain: T072. Lightstack has no project hierarchy, so
// every project is top-level, and upstream gives a top-level project its domain
// as parent (keystone/resource/core.py). Filtering by parent is filtering by
// domain — so `openstack project list --parent default` and Terraform's project
// data source find the domain's projects rather than nothing.
func TestProjectParentIsItsDomain(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}

	byDomain := mustGet(t, r, "/v3/projects?domain_id=default", admin)
	byParent := mustGet(t, r, "/v3/projects?parent_id=default", admin)
	nDomain, nParent := countIn(t, byDomain, "projects"), countIn(t, byParent, "projects")
	if nDomain == 0 {
		t.Fatal("no project in the default domain — nothing to compare against")
	}
	if nParent != nDomain {
		t.Errorf("parent_id=default returned %d projects, domain_id=default %d", nParent, nDomain)
	}
}

// TestUnfilteredUnchanged: F3 / FR-026. The compatibility guarantee.
func TestUnfilteredUnchanged(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}
	for _, path := range []string{"/v3/roles", "/v3/users", "/v3/projects",
		"/v3/groups", "/v3/services", "/v3/endpoints"} {
		if code, body := doReq(r, http.MethodGet, path, admin); code != http.StatusOK {
			t.Errorf("GET %s: code=%d body=%s; want 200", path, code, body)
		}
	}
}

// TestUnknownFilterIgnored: F4. Consistent with the domain collection, which
// ignores parameters it does not implement.
func TestUnknownFilterIgnored(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}
	_, all := doReq(r, http.MethodGet, "/v3/roles", admin)
	_, bogus := doReq(r, http.MethodGet, "/v3/roles?bogus=1", admin)
	if countIn(t, all, "roles") != countIn(t, bogus, "roles") {
		t.Errorf("an unrecognised parameter changed the answer:\n%s\n%s", all, bogus)
	}
}

// TestEnabledFilterAcceptsUpstreamVocabulary: SRC-4. `?enabled=yes` was silently
// dropped by strconv.ParseBool, so the caller got the UNFILTERED collection back —
// the same "filter discarded" defect US5 exists to fix, inside the change that
// fixed it. The seeded users and projects are all enabled, so an `enabled=true`
// filter in any spelling must return them and `enabled=false` must return none.
func TestEnabledFilterAcceptsUpstreamVocabulary(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	admin := map[string]string{"X-Test-Roles": "admin"}

	all := countIn(t, mustGet(t, r, "/v3/users", admin), "users")
	if all == 0 {
		t.Skip("no users seeded")
	}

	for _, spelling := range []string{"true", "True", "1", "yes", "on", "y", "t"} {
		t.Run("enabled="+spelling, func(t *testing.T) {
			if n := countIn(t, mustGet(t, r, "/v3/users?enabled="+spelling, admin), "users"); n != all {
				t.Errorf("?enabled=%s returned %d of %d enabled users", spelling, n, all)
			}
		})
	}
	for _, spelling := range []string{"false", "False", "0", "no", "off", "n", "f"} {
		t.Run("enabled="+spelling, func(t *testing.T) {
			if n := countIn(t, mustGet(t, r, "/v3/users?enabled="+spelling, admin), "users"); n != 0 {
				t.Errorf("?enabled=%s returned %d users; every seeded user is enabled", spelling, n)
			}
		})
	}
	// Unrecognised is ignored, not narrowed — a wrong subset is worse than none.
	if n := countIn(t, mustGet(t, r, "/v3/users?enabled=banana", admin), "users"); n != all {
		t.Errorf("?enabled=banana returned %d of %d; an unparseable value must be ignored", n, all)
	}
}

func mustGet(t *testing.T, r *gin.Engine, path string, hdr map[string]string) string {
	t.Helper()
	code, body := doReq(r, http.MethodGet, path, hdr)
	if code != http.StatusOK {
		t.Fatalf("GET %s: code=%d body=%s", path, code, body)
	}
	return body
}

// TestEqDomainResolvesTheDefaultAlias: SRC-6. `default` is the domain reference
// every OpenStack client sends, and it is not a uuid — matching it literally
// against domain_id would return an empty collection, which "honours" the filter
// while being wrong. It resolves through a subquery against the domains table
// instead, so the deployment's Default domain is whatever the database says.
//
// No database needed: assert the rendered predicate and its arguments.
func TestEqDomainResolvesTheDefaultAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("the default alias resolves via subquery", func(t *testing.T) {
		var f listFilter
		f.eqDomain(filterCtx("domain_id=default"), "domain_id", "domain_id")
		if want := " WHERE domain_id = (SELECT id FROM domains WHERE name = $1)"; f.Where() != want {
			t.Errorf("where() = %q, want %q", f.Where(), want)
		}
		if len(f.Args()) != 1 || f.Args()[0] != defaultDomainName {
			t.Errorf("args = %v, want [%q] — the alias must resolve against the domains "+
				"table, not be matched literally", f.Args(), defaultDomainName)
		}
	})

	t.Run("a concrete id is matched directly", func(t *testing.T) {
		var f listFilter
		id := "00000000-0000-0000-0000-000000000100"
		f.eqDomain(filterCtx("domain_id="+id), "domain_id", "domain_id")
		if want := " WHERE domain_id = $1"; f.Where() != want {
			t.Errorf("where() = %q, want %q", f.Where(), want)
		}
		if len(f.Args()) != 1 || f.Args()[0] != id {
			t.Errorf("args = %v, want [%q]", f.Args(), id)
		}
	})

	t.Run("a value that is not a UUID matches nothing", func(t *testing.T) {
		var f listFilter
		f.eqDomain(filterCtx("domain_id=no-such-domain"), "domain_id", "domain_id")
		if want := " WHERE FALSE"; f.Where() != want {
			t.Errorf("where() = %q, want %q", f.Where(), want)
		}
		if len(f.Args()) != 0 {
			t.Errorf("args = %v; the value must never reach the uuid column", f.Args())
		}
	})

	t.Run("absent adds nothing", func(t *testing.T) {
		var f listFilter
		f.eqDomain(filterCtx(""), "domain_id", "domain_id")
		if f.Where() != "" || len(f.Args()) != 0 {
			t.Errorf("where()=%q args=%v; want no predicate", f.Where(), f.Args())
		}
	})
}

// TestEqIDMatchesOnlyAUUID: a uuid column refuses a value that is not a UUID —
// Postgres raises 22P02 and the handler answered 500. Such a value names no row,
// so it matches nothing. A UUID is bound in canonical form, because uuid.Parse
// accepts a spelling Postgres refuses (`urn:uuid:`).
//
// No database needed: assert the rendered predicate and its arguments.
func TestEqIDMatchesOnlyAUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"

	for _, spelling := range []string{id, strings.ToUpper(id), "{" + id + "}",
		"urn:uuid:" + id, strings.ReplaceAll(id, "-", "")} {
		t.Run(spelling, func(t *testing.T) {
			var f listFilter
			f.eqID(filterCtx("service_id="+url.QueryEscape(spelling)), "service_id", "e.service_id")
			if want := " WHERE e.service_id = $1"; f.Where() != want {
				t.Errorf("where() = %q, want %q", f.Where(), want)
			}
			if len(f.Args()) != 1 || f.Args()[0] != id {
				t.Errorf("args = %v, want [%q]", f.Args(), id)
			}
		})
	}

	for _, v := range []string{"no-such-service", id[:len(id)-1]} {
		t.Run(v, func(t *testing.T) {
			var f listFilter
			f.eqID(filterCtx("service_id="+v), "service_id", "e.service_id")
			if want := " WHERE FALSE"; f.Where() != want {
				t.Errorf("where() = %q, want %q", f.Where(), want)
			}
			if len(f.Args()) != 0 {
				t.Errorf("args = %v; the value must never reach the uuid column", f.Args())
			}
		})
	}

	t.Run("absent adds nothing", func(t *testing.T) {
		var f listFilter
		f.eqID(filterCtx(""), "service_id", "e.service_id")
		if f.Where() != "" || len(f.Args()) != 0 {
			t.Errorf("where()=%q args=%v; want no predicate", f.Where(), f.Args())
		}
	})
}

func filterCtx(rawQuery string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v3/users?"+rawQuery, nil)
	return c
}
