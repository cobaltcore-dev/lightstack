package nova

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newNovaTestRouter wires the real Nova RegisterRoutes behind a stub auth
// middleware that sets project_id / user_id / roles from X-Test-* headers, so
// tests exercise the handlers without minting JWTs.
//
// Nova's RegisterRoutes(r *gin.RouterGroup) takes no auth arg (unlike keystone's),
// so the middleware is attached to the engine with r.Use before registering routes.
// Stub mode: NewService(testPool, "", "stub", nil) leaves vmManager/cache/neutronSvc nil —
// all of which are nil-guarded on the server create/get/list paths under test
// (svc.cache only in flavor paths; svc.neutronSvc only when networks are supplied).
func newNovaTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	NewService(testPool, "", "stub", nil).RegisterRoutes(&r.RouterGroup)
	return r
}

// doReq issues an httptest request with an optional JSON body and headers,
// returning the status code and response body.
func doReq(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// Seeded scope from migration 002_seed_data (the default project has a seeded
// `default` security group and flavors). Feature tests that need a real DB scope
// use these; harness smoke tests below do not.
const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

// testAuthHdr returns X-Test-* headers scoping a request to the given project/user
// with the admin role.
func testAuthHdr(projectID, userID string) map[string]string {
	return map[string]string{
		"X-Test-Project": projectID,
		"X-Test-User":    userID,
		"X-Test-Roles":   "admin",
	}
}

// TestNovaHarness_Version proves the harness wiring (service construction, route
// registration, auth middleware, httptest plumbing) works end-to-end without a
// database, so harness breakage is caught even when LIGHTSTACK_TEST_DB_URL is unset.
func TestNovaHarness_Version(t *testing.T) {
	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v2.1: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, "2.1") {
		t.Errorf("GET /v2.1 body=%s; want it to mention version 2.1", body)
	}
}

// TestNovaHarness_DB proves the DB-backed harness works: with LIGHTSTACK_TEST_DB_URL set,
// an authenticated request scoped to the seeded default project flows through
// router → auth middleware → handler → Postgres and returns the servers envelope.
// It also exercises requireDB / testAuthHdr / the seeded scope constants that the
// forthcoming key_name + security_groups read-back tests will rely on. Skips when
// no test DB is configured.
func TestNovaHarness_DB(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.1/servers", "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("GET /v2.1/servers: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, "servers") {
		t.Errorf("GET /v2.1/servers body=%s; want a servers list envelope", body)
	}
}
