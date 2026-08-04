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
	// Counted, not just permissive: the assertions below need to tell "the route is on
	// an authenticated group" apart from "the route is public", and a pass-through stub
	// cannot. Registering a management route on `v3` instead of `mgmt` is a one-token
	// slip that would leave the resource readable without a token.
	authCalls := 0
	stubAuth := func(c *gin.Context) { authCalls++; c.Next() } // authenticated, no roles
	svc := NewService(nil, nil)
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

	// GET /domains/:id also lives on mgmt, for a different reason: it must resolve
	// the "default" alias, which the uuid middleware would reject before the handler
	// ran (SPEC-022 FR-001a). The narrow exception is that one alias — every other
	// non-identifier value still gets #515's verbatim 404, now produced in-handler.
	// No database needed: this asserts the 404 shape, which precedes any query.
	w = httptest.NewRecorder()
	authBefore := authCalls
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v3/domains/not-a-uuid", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Could not find domain: not-a-uuid.") {
		t.Fatalf("GET /domains/not-a-uuid: code=%d body=%s; want 404 verbatim from the handler", w.Code, w.Body.String())
	}

	// Moving the route off `val` must not have moved it off the authenticated tree as
	// well: registered on the public `v3` group by mistake, it would answer the same
	// 404 above while leaving every domain readable without a token. The status alone
	// cannot see that, so assert the middleware ran. This piggybacks on the malformed
	// id deliberately — resolveDomainRef answers it without a query, so the assertion
	// needs no database (TestResolveDomainRefMalformedNeedsNoDatabase pins that).
	if authCalls == authBefore {
		t.Fatal("GET /v3/domains/:id bypassed the auth middleware — it must be on mgmt, not the public v3 group")
	}

	// The sibling domain routes stay on val, so the middleware still answers for
	// them — the exception is one route and one method, not the whole resource.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPatch, "/v3/domains/not-a-uuid"},
		{http.MethodDelete, "/v3/domains/not-a-uuid"},
		{http.MethodGet, "/v3/domains/not-a-uuid/config"},
	} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Could not find domain: not-a-uuid.") {
			t.Fatalf("%s %s: code=%d body=%s; want the middleware's 404", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
