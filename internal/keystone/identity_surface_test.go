package keystone

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newIdentityTestRouter wires the real RegisterRoutes with a stub auth middleware
// that sets roles / user_id from the X-Test-Roles / X-Test-User headers, so tests
// can simulate admin / owner / plain callers without minting JWTs.
func newIdentityTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService(testPool, nil, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			c.Set("user_id", uid)
		}
		c.Next()
	})
	return r
}

func doReq(r *gin.Engine, method, path string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

var adminHdr = map[string]string{"X-Test-Roles": "admin", "X-Test-User": "00000000-0000-0000-0000-00000000a0a0"}

// TestIdentityNotFound_Surface (US2) asserts that a malformed OR a valid-but-unknown
// id returns the identical Keystone-verbatim 404 across the whole identity by-id
// surface (not just projects/users/roles), including nested paths.
func TestIdentityNotFound_Surface(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	const unknown = "00000000-0000-0000-0000-0000000000ff"
	const valid = "00000000-0000-0000-0000-000000000001"

	check := func(method, path, noun, id string) {
		t.Helper()
		code, body := doReq(r, method, path, adminHdr)
		want := fmt.Sprintf("Could not find %s: %s.", noun, id)
		if code != http.StatusNotFound || !strings.Contains(body, want) {
			t.Errorf("%s %s: code=%d body=%s; want 404 %q", method, path, code, body, want)
		}
	}

	// Single-id endpoints: malformed (middleware) and valid-unknown (handler).
	for _, tc := range []struct{ method, tmpl, noun string }{
		{"GET", "/v3/groups/%s", "group"},
		{"GET", "/v3/domains/%s", "domain"},
		{"GET", "/v3/domains/%s/config", "domain"},
		{"GET", "/v3/services/%s", "service"},
		{"GET", "/v3/credentials/%s", "credential"},
		{"GET", "/v3/application_credentials/%s", "Application Credential"},
		{"GET", "/v3/users/%s/projects", "user"},
		{"DELETE", "/v3/endpoints/%s", "endpoint"},
	} {
		check(tc.method, fmt.Sprintf(tc.tmpl, "not-a-uuid"), tc.noun, "not-a-uuid")
		check(tc.method, fmt.Sprintf(tc.tmpl, unknown), tc.noun, unknown)
	}

	// Nested paths: the middleware reports the first malformed segment's noun.
	check("GET", "/v3/projects/"+valid+"/users/not-a-uuid/roles/"+valid, "user", "not-a-uuid")
	check("GET", "/v3/projects/not-a-uuid/users/"+valid+"/roles/"+valid, "project", "not-a-uuid")
	check("DELETE", "/v3/groups/"+valid+"/users/not-a-uuid", "user", "not-a-uuid")
	check("DELETE", "/v3/groups/not-a-uuid/users/"+valid, "group", "not-a-uuid")
}

// TestIdentityNotFound_MutatingVerbs (US2) exercises the handler not-found paths a
// GET can't reach: PATCH/DELETE and ChangePassword on a valid-but-unknown id must
// return the verbatim 404 (RowsAffected==0 / fetch-ErrNoRows / existsOr404), and
// the two-param application-credential lookup too.
func TestIdentityNotFound_MutatingVerbs(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	const unknown = "00000000-0000-0000-0000-0000000000ff"
	const valid = "00000000-0000-0000-0000-000000000001"

	want404 := func(method, path, jsonBody, noun, id string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(jsonBody))
		if jsonBody != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range adminHdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		expect := fmt.Sprintf("Could not find %s: %s.", noun, id)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), expect) {
			t.Errorf("%s %s: code=%d body=%s; want 404 %q", method, path, w.Code, w.Body.String(), expect)
		}
	}

	// DELETE, valid-but-unknown (RowsAffected==0 / DeleteDomain fetch); users is admin-gated.
	want404("DELETE", "/v3/roles/"+unknown, "", "role", unknown)
	want404("DELETE", "/v3/projects/"+unknown, "", "project", unknown)
	want404("DELETE", "/v3/groups/"+unknown, "", "group", unknown)
	want404("DELETE", "/v3/credentials/"+unknown, "", "credential", unknown)
	want404("DELETE", "/v3/domains/"+unknown, "", "domain", unknown)
	want404("DELETE", "/v3/users/"+unknown, "", "user", unknown)

	// PATCH, valid-but-unknown (fetch-after-update ErrNoRows).
	want404("PATCH", "/v3/roles/"+unknown, `{"role":{"name":"x"}}`, "role", unknown)
	want404("PATCH", "/v3/projects/"+unknown, `{"project":{"name":"x"}}`, "project", unknown)

	// ChangePassword: admin passes authz, id is well-formed, fetch hits ErrNoRows.
	want404("POST", "/v3/users/"+unknown+"/password", `{"user":{"original_password":"x","password":"y"}}`, "user", unknown)

	// Two-param application-credential lookup (GetApplicationCredential).
	want404("GET", "/v3/users/"+valid+"/application_credentials/"+unknown, "", "Application Credential", unknown)
}

// TestRoleAssignmentNotFoundMessage (US2) covers the two role-assignment endpoints,
// whose 404 uses Keystone's RoleAssignmentNotFound triple message rather than the
// "Could not find <resource>: <id>." form — an assignment has no single id.
func TestRoleAssignmentNotFoundMessage(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	// All three must be well-formed UUIDs (hex only) so the middleware passes them
	// through and the handler's own assignment lookup produces the 404.
	const proj, user, role = "00000000-0000-0000-0000-0000000000a1",
		"00000000-0000-0000-0000-0000000000b2",
		"00000000-0000-0000-0000-0000000000c3"

	// well-formed but unassigned triple -> verbatim RoleAssignmentNotFound
	path := "/v3/projects/" + proj + "/users/" + user + "/roles/" + role
	want := "Could not find role assignment with role: " + role +
		", user or group: " + user +
		", project, domain, or system: " + proj + "."
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		code, body := doReq(r, method, path, adminHdr)
		if code != http.StatusNotFound || !strings.Contains(body, want) {
			t.Errorf("%s %s: code=%d body=%s; want 404 %q", method, path, code, body, want)
		}
	}
}
