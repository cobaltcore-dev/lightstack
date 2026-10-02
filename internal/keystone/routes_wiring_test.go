package keystone

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRegisterRoutesMiddlewareWiring verifies the val/mgmt split end-to-end via
// the real RegisterRoutes: (1) it registers without panicking, (2) the UUID
// middleware guards the non-authz-gated routes (malformed id → 404 before the
// handler ever runs), and (3) the authz-gated routes are NOT behind the
// middleware, so their in-handler authz check still runs first. No DB needed —
// the middleware aborts on the val route, and the gated handler returns 403
// before any DB access.
func TestRegisterRoutesMiddlewareWiring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	stubAuth := func(c *gin.Context) { c.Next() } // authenticated, no roles
	svc := NewService(testPool, nil, nil)
	svc.RegisterRoutes(&r.RouterGroup, stubAuth)

	// Non-gated route (on val): malformed id → middleware 404, handler never runs.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v3/projects/not-a-uuid", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Could not find project: not-a-uuid.") {
		t.Fatalf("GET /projects/not-a-uuid: code=%d body=%s; want 404 verbatim", w.Code, w.Body.String())
	}

	// Authz-gated route (on mgmt, NOT behind the uuid middleware): a non-admin
	// caller is rejected by the handler's own authz check (403), proving the
	// middleware did not pre-empt it with a 404.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v3/users/not-a-uuid", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("DELETE /users/not-a-uuid (non-admin): code=%d body=%s; want 403 (authz precedes)", w.Code, w.Body.String())
	}
}
