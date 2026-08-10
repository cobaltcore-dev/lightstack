package keystone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		if want := " WHERE domain_id = (SELECT id FROM domains WHERE name = $1)"; f.where() != want {
			t.Errorf("where() = %q, want %q", f.where(), want)
		}
		if len(f.args) != 1 || f.args[0] != defaultDomainName {
			t.Errorf("args = %v, want [%q] — the alias must resolve against the domains "+
				"table, not be matched literally", f.args, defaultDomainName)
		}
	})

	t.Run("a concrete id is matched directly", func(t *testing.T) {
		var f listFilter
		id := "00000000-0000-0000-0000-000000000100"
		f.eqDomain(filterCtx("domain_id="+id), "domain_id", "domain_id")
		if want := " WHERE domain_id = $1"; f.where() != want {
			t.Errorf("where() = %q, want %q", f.where(), want)
		}
		if len(f.args) != 1 || f.args[0] != id {
			t.Errorf("args = %v, want [%q]", f.args, id)
		}
	})

	t.Run("absent adds nothing", func(t *testing.T) {
		var f listFilter
		f.eqDomain(filterCtx(""), "domain_id", "domain_id")
		if f.where() != "" || len(f.args) != 0 {
			t.Errorf("where()=%q args=%v; want no predicate", f.where(), f.args)
		}
	})
}

func filterCtx(rawQuery string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v3/users?"+rawQuery, nil)
	return c
}
