package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/keystone"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// AuthMiddleware had no tests at all — this file is its first. It is the only thing
// between an anonymous request and every service in the product, and branch review found
// that removing a line from it left all 17 packages green.
//
// No database is needed. There is no exported token constructor (minting happens inside
// AuthenticatePassword, which does need one), but TokenClaims is exported and
// ValidateToken accepts any HS256 token signed with the service's secret — so a test can
// mint its own and stay a unit test.

const (
	authTestSecret  = "auth-middleware-test-secret"
	authTestUser    = "00000000-0000-0000-0000-0000000000a1"
	authTestProject = "00000000-0000-0000-0000-0000000000b1"
)

// mintToken signs a token the way AuthService does (HS256 over TokenClaims).
func mintToken(t *testing.T, secret, userID, projectID string, roles []string, ttl time.Duration) string {
	t.Helper()
	claims := &keystone.TokenClaims{
		UserID:    userID,
		UserName:  "auth-test",
		ProjectID: projectID,
		Roles:     roles,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("minting token: %v", err)
	}
	return signed
}

// authTestRouter wires the real AuthMiddleware in front of a handler that reports what
// landed in the context.
func authTestRouter(secret string, seen map[string]any) *gin.Engine {
	// No pool: token validation is stateless, so these tests need no database.
	svc := keystone.NewAuthService(nil, secret, time.Hour, nil)
	r := gin.New()
	r.Use(AuthMiddleware(svc))
	record := func(c *gin.Context) {
		for _, key := range []string{"user_id", "user_name", "project_id", "roles", CallerProjectKey} {
			if v, ok := c.Get(key); ok {
				seen[key] = v
			}
		}
		c.Status(http.StatusOK)
	}
	for _, p := range []string{"/x", "/v3", "/v2.1", "/v2.0", "/", "/v3/auth/tokens", "/v3/projects"} {
		r.GET(p, record)
		r.POST(p, record)
	}
	return r
}

func authDo(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAuthMiddlewareRecordsCallerProject is the SEAM between AuthMiddleware and
// LoggingMiddleware, and it is the one this file exists for.
//
// B15: the access log read "project_id" after the handler had run, so an administrator's
// cross-tenant action was attributed to the tenant acted UPON — the compute action
// dispatcher rebinds that key to the server's owner (SPEC-023 FR-007). The fix records the
// caller separately, in this middleware, under CallerProjectKey.
//
// Nothing tested the producing half. logging_test.go sets CallerProjectKey ITSELF, under a
// comment reading "What the auth middleware does", so the consumer was covered and the
// producer was not — and logging.go falls back to the rebound project when the key is
// absent rather than losing the field. Deleting the c.Set therefore restored B15 with all
// 17 packages still passing. This test is what makes that deletion fail.
func TestAuthMiddlewareRecordsCallerProject(t *testing.T) {
	seen := map[string]any{}
	r := authTestRouter(authTestSecret, seen)
	token := mintToken(t, authTestSecret, authTestUser, authTestProject, []string{"member"}, time.Hour)

	if w := authDo(r, http.MethodGet, "/v3/projects", token); w.Code != http.StatusOK {
		t.Fatalf("authenticated request: code=%d body=%s; want 200", w.Code, w.Body.String())
	}

	caller, ok := seen[CallerProjectKey]
	if !ok {
		t.Fatal("AuthMiddleware recorded no caller project: the access log then falls back to the " +
			"REBOUND project, attributing a cross-tenant action to the tenant acted upon (B15)")
	}
	if caller != authTestProject {
		t.Errorf("%s = %v; want the token's project %q", CallerProjectKey, caller, authTestProject)
	}
	// They must start EQUAL. Divergence is created later, by a handler rebinding the
	// effective project — never by this middleware.
	if effective := seen["project_id"]; caller != effective {
		t.Errorf("caller = %v, effective project_id = %v; both must start equal", caller, effective)
	}
}

// TestAuthMiddlewarePopulatesClaims covers the rest of what a handler depends on being
// present. A missing key here is a nil-read or a silently unscoped query downstream.
func TestAuthMiddlewarePopulatesClaims(t *testing.T) {
	seen := map[string]any{}
	r := authTestRouter(authTestSecret, seen)
	token := mintToken(t, authTestSecret, authTestUser, authTestProject, []string{"member", "admin"}, time.Hour)

	if w := authDo(r, http.MethodGet, "/v3/projects", token); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s; want 200", w.Code, w.Body.String())
	}
	if got := seen["user_id"]; got != authTestUser {
		t.Errorf("user_id = %v; want %q", got, authTestUser)
	}
	if got := seen["project_id"]; got != authTestProject {
		t.Errorf("project_id = %v; want %q", got, authTestProject)
	}
	roles, ok := seen["roles"].([]string)
	if !ok || len(roles) != 2 || roles[0] != "member" || roles[1] != "admin" {
		t.Errorf("roles = %v; want [member admin] — common.HasRole reads this and an absent value denies every role", seen["roles"])
	}
}

// TestAuthMiddlewareRefusesUnauthenticated pins the two 401s. These are the branches that
// decide whether an anonymous request reaches a handler at all.
func TestAuthMiddlewareRefusesUnauthenticated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no token at all", ""},
		{"not a JWT", "garbage"},
		{"signed with the wrong secret", mintToken(t, "a-different-secret", authTestUser, authTestProject, nil, time.Hour)},
		{"expired", mintToken(t, authTestSecret, authTestUser, authTestProject, nil, -time.Hour)},
		{"algorithm swapped to none", "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJ1c2VyX2lkIjoiYSJ9."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]any{}
			r := authTestRouter(authTestSecret, seen)
			w := authDo(r, http.MethodGet, "/v3/projects", tc.token)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("code=%d body=%s; want 401", w.Code, w.Body.String())
			}
			// The handler must not have run: a rejected request that still reaches a
			// handler is worse than one answered 200, because it runs unscoped.
			if len(seen) != 0 {
				t.Errorf("the handler ran despite a 401 and saw %v; the middleware must abort", seen)
			}
		})
	}
}

// TestAuthMiddlewareSkipsDiscoveryAndTokenIssuance pins the pass-through list. It is
// deliberately exhaustive: this is the set of paths reachable with NO credentials, so a
// path added to it is an authentication decision, and it should not be possible to widen
// the list by accident.
func TestAuthMiddlewareSkipsDiscoveryAndTokenIssuance(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v3"},              // keystone version document
		{http.MethodGet, "/v2.1"},            // nova version document
		{http.MethodGet, "/v2.0"},            // neutron version document
		{http.MethodGet, "/"},                // the version list
		{http.MethodPost, "/v3/auth/tokens"}, // token issuance: the caller has no token yet
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			seen := map[string]any{}
			r := authTestRouter(authTestSecret, seen)
			if w := authDo(r, tc.method, tc.path, ""); w.Code != http.StatusOK {
				t.Errorf("unauthenticated %s %s: code=%d; want 200 — discovery must not require a token",
					tc.method, tc.path, w.Code)
			}
			if _, present := seen[CallerProjectKey]; present {
				t.Errorf("a skipped path recorded a caller project; there is no authenticated caller on %s", tc.path)
			}
		})
	}

	// The converse, and the reason the list is pinned: GET on the token endpoint is NOT
	// skipped — only POST is — and a project resource is never skipped.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v3/auth/tokens"},
		{http.MethodGet, "/v3/projects"},
	} {
		t.Run("not skipped: "+tc.method+" "+tc.path, func(t *testing.T) {
			seen := map[string]any{}
			r := authTestRouter(authTestSecret, seen)
			if w := authDo(r, tc.method, tc.path, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s: code=%d; want 401", tc.method, tc.path, w.Code)
			}
		})
	}
}
