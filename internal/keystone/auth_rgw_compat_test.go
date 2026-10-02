package keystone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Fixtures for the rgw_compat_tenant_ids tests: a dedicated project with a
// fixed hyphenated UUID (so the canonical and dash-free forms are obvious),
// the member role for the seeded admin (RGW's rgw_keystone_accepted_roles
// default), and a catalog service whose endpoint URL carries a {project_id}
// template — proving BuildServiceCatalog keeps receiving the canonical id.
const (
	rgwCompatProjectID         = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	rgwCompatProjectIDDashFree = "1a2b3c4d5e6f4a7b8c9d0e1f2a3b4c5d"
	rgwCompatProjectName       = "rgw-compat-test-project"
	rgwCompatServiceID         = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	rgwCompatCatalogType       = "rgw-compat-test"
	rgwCompatSeededAdminID     = "00000000-0000-0000-0000-000000000001"
	rgwCompatMemberRoleID      = "00000000-0000-0000-0000-000000000004"

	// rgwCompatTestSecret is the signing secret wired into newRGWCompatRouter;
	// the edge-case tests below mint their own JWTs with it to reach states
	// the HTTP issue paths never produce (missing/expired expiry).
	rgwCompatTestSecret = "rgw-compat-test-secret"
)

// newRGWCompatRouter wires the real RegisterRoutes behind a real AuthService
// carrying the rgw_compat_tenant_ids flag, plus the same stub auth middleware
// as newIdentityTestRouter (roles / user_id from X-Test-* headers) so the
// management routes can be exercised without minting JWTs for them.
func newRGWCompatRouter(rgwCompat bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authSvc := NewAuthService(testPool, rgwCompatTestSecret, time.Hour, nil)
	authSvc.SetRGWCompatTenantIDs(rgwCompat)
	NewService(testPool, authSvc, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
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

// seedRGWCompatFixtures installs the project / role assignment / catalog
// template above and restores the seeded admin (enabled, bcrypt('secret')), so
// password auth works regardless of the state other tests in this package
// leave behind. Everything is removed again on cleanup.
func seedRGWCompatFixtures(t *testing.T) {
	t.Helper()
	requireDB(t)
	ctx := context.Background()
	resetAdmin(t)

	if _, err := testPool.Exec(ctx, `
		INSERT INTO projects (id, name, description, enabled, domain_id)
		VALUES ($1, $2, 'rgw_compat_tenant_ids test fixture', true, $3)
		ON CONFLICT (id) DO NOTHING`,
		rgwCompatProjectID, rgwCompatProjectName, defaultDomainID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		rgwCompatSeededAdminID, rgwCompatProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("seed role assignment: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO services (id, type, name, description, enabled)
		VALUES ($1, $2, 'rgw-compat-test', 'rgw_compat_tenant_ids test fixture', true)
		ON CONFLICT (id) DO NOTHING`,
		rgwCompatServiceID, rgwCompatCatalogType); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO endpoints (service_id, interface, url, region, enabled)
		VALUES ($1, 'public', 'http://lightstack:9999/v2.1/{project_id}', 'RegionOne', true)
		ON CONFLICT DO NOTHING`,
		rgwCompatServiceID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM endpoints WHERE service_id = $1", rgwCompatServiceID); err != nil {
			t.Errorf("cleanup fixture endpoints: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM services WHERE id = $1", rgwCompatServiceID); err != nil {
			t.Errorf("cleanup fixture service: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE project_id = $1", rgwCompatProjectID); err != nil {
			t.Errorf("cleanup fixture role assignments: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM projects WHERE id = $1", rgwCompatProjectID); err != nil {
			t.Errorf("cleanup fixture project: %v", err)
		}
	})
}

// passwordAuthBody builds a POST /v3/auth/tokens body for the seeded admin
// (password 'secret'), scoped to projectID; an empty projectID requests an
// explicitly unscoped token.
func passwordAuthBody(projectID string) string {
	scope := `"scope":"unscoped"`
	if projectID != "" {
		scope = fmt.Sprintf(`"scope":{"project":{"id":%q}}`, projectID)
	}
	return `{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},` + scope + `}}`
}

// tokenAuthBody builds a POST /v3/auth/tokens body that re-scopes an existing
// token (identity method "token") to a project; an empty projectID requests an
// explicitly unscoped token.
func tokenAuthBody(token, projectID string) string {
	scope := `"scope":"unscoped"`
	if projectID != "" {
		scope = fmt.Sprintf(`"scope":{"project":{"id":%q}}`, projectID)
	}
	return fmt.Sprintf(`{"auth":{"identity":{"methods":["token"],"token":{"id":%q}},%s}}`, token, scope)
}

// postTokens issues POST /v3/auth/tokens against r and returns the recorder.
func postTokens(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v3/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// tokenResponseBody is the subset of the POST /v3/auth/tokens response these
// tests assert on.
type tokenResponseBody struct {
	Token struct {
		ExpiresAt string   `json:"expires_at"`
		IssuedAt  string   `json:"issued_at"`
		Methods   []string `json:"methods"`
		Project   *struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Domain struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"domain"`
		} `json:"project"`
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		Roles []struct {
			Name string `json:"name"`
		} `json:"roles"`
		Catalog []struct {
			Type      string `json:"type"`
			Endpoints []struct {
				URL string `json:"url"`
			} `json:"endpoints"`
		} `json:"catalog"`
	} `json:"token"`
}

func decodeTokenBody(t *testing.T, w *httptest.ResponseRecorder) tokenResponseBody {
	t.Helper()
	var body tokenResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response: %v (body=%s)", err, w.Body.String())
	}
	return body
}

// getToken issues GET /v3/auth/tokens (token validation) with X-Subject-Token
// — the call Ceph RGW's Swift keystone engine makes for every user token.
func getToken(t *testing.T, r *gin.Engine, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v3/auth/tokens", nil)
	req.Header.Set("X-Subject-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// validationResponseBody is the subset of the GET /v3/auth/tokens response
// these tests assert on — the mandatory fields of Ceph RGW's keystone
// TokenEnvelope parser (user.id, user.name, expires_at, roles[].name,
// project.id, project.name) plus the neighbors that must stay canonical.
type validationResponseBody struct {
	Token struct {
		ExpiresAt string   `json:"expires_at"`
		IssuedAt  string   `json:"issued_at"`
		Methods   []string `json:"methods"`
		User      struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Domain *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"domain"`
		} `json:"user"`
		Project *struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Domain struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"domain"`
		} `json:"project"`
		Roles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"roles"`
		Catalog []struct {
			Type      string `json:"type"`
			Endpoints []struct {
				URL string `json:"url"`
			} `json:"endpoints"`
		} `json:"catalog"`
	} `json:"token"`
}

func decodeValidationBody(t *testing.T, w *httptest.ResponseRecorder) validationResponseBody {
	t.Helper()
	var body validationResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode validation response: %v (body=%s)", err, w.Body.String())
	}
	return body
}

// TestDashFreeTenantID: the RGW boundary uses the existing dash-free form.
func TestDashFreeTenantID(t *testing.T) {
	cases := []struct{ in, want string }{
		{rgwCompatProjectID, rgwCompatProjectIDDashFree},
		{"00000000-0000-0000-0000-000000000002", "00000000000000000000000000000002"},
		{"0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{strings.ToUpper(rgwCompatProjectID), strings.ToUpper(rgwCompatProjectIDDashFree)},
		{"a-b-c", "abc"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := canonicalID(tc.in); got != tc.want {
			t.Errorf("canonicalID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTokenProjectIDGating: the flag gates the transform; the default is off
// (canonical hyphenated UUIDs).
func TestTokenProjectIDGating(t *testing.T) {
	s := NewAuthService(testPool, "k", time.Hour, nil)
	if got := s.tokenProjectID(rgwCompatProjectID); got != rgwCompatProjectID {
		t.Errorf("flag off (default): tokenProjectID = %q, want canonical %q", got, rgwCompatProjectID)
	}
	s.SetRGWCompatTenantIDs(true)
	if got := s.tokenProjectID(rgwCompatProjectID); got != rgwCompatProjectIDDashFree {
		t.Errorf("flag on: tokenProjectID = %q, want %q", got, rgwCompatProjectIDDashFree)
	}
	// A 32-hex alternate encoding is already the same dash-free tenant.
	if got := s.tokenProjectID("0123456789abcdef0123456789abcdef"); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("flag on, 32-hex id: tokenProjectID = %q, want the canonical dash-free form", got)
	}
}

// TestTokenProjectIDRGWCompat: keystone.rgw_compat_tenant_ids changes ONLY the
// token payload's project id (token.project.id) — in both token-payload
// builders (password issue and token re-scope). Everything else in the payload
// (project name/domain, user id, roles) and the service catalog keep the
// canonical, hyphenated UUIDs.
func TestTokenProjectIDRGWCompat(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	cases := []struct {
		name      string
		rgwCompat bool
		rescope   bool // false: password auth; true: token re-scope
		wantID    string
	}{
		{"password issue, flag off", false, false, rgwCompatProjectID},
		{"password issue, flag on", true, false, rgwCompatProjectIDDashFree},
		{"token re-scope, flag off", false, true, rgwCompatProjectID},
		{"token re-scope, flag on", true, true, rgwCompatProjectIDDashFree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRGWCompatRouter(tc.rgwCompat)

			var w *httptest.ResponseRecorder
			if tc.rescope {
				// Mint an unscoped token first, then re-scope it to the project
				// (identity method "token").
				unscoped := postTokens(t, r, passwordAuthBody(""))
				if unscoped.Code != http.StatusCreated {
					t.Fatalf("unscoped auth: code=%d body=%s", unscoped.Code, unscoped.Body.String())
				}
				w = postTokens(t, r, tokenAuthBody(unscoped.Header().Get("X-Subject-Token"), rgwCompatProjectID))
			} else {
				w = postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
			}
			if w.Code != http.StatusCreated {
				t.Fatalf("POST /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
			}

			body := decodeTokenBody(t, w)
			if body.Token.Project == nil {
				t.Fatal("scoped token response missing token.project")
			}
			if body.Token.Project.ID != tc.wantID {
				t.Errorf("token.project.id = %q, want %q", body.Token.Project.ID, tc.wantID)
			}

			// The rest of the payload stays canonical whatever the flag.
			if body.Token.Project.Name != rgwCompatProjectName {
				t.Errorf("token.project.name = %q, want canonical %q", body.Token.Project.Name, rgwCompatProjectName)
			}
			if body.Token.Project.Domain.ID != defaultDomainID {
				t.Errorf("token.project.domain.id = %q, want canonical %q", body.Token.Project.Domain.ID, defaultDomainID)
			}
			if body.Token.User.ID != rgwCompatSeededAdminID {
				t.Errorf("token.user.id = %q, want canonical %q", body.Token.User.ID, rgwCompatSeededAdminID)
			}
			if len(body.Token.Roles) == 0 {
				t.Error("scoped token response missing roles")
			}

			// BuildServiceCatalog must keep receiving the CANONICAL project id:
			// the template endpoint renders the hyphenated form even with the
			// flag on (nova endpoint URLs etc. stay canonical).
			var sawTemplate bool
			for _, svc := range body.Token.Catalog {
				if svc.Type != rgwCompatCatalogType {
					continue
				}
				sawTemplate = true
				for _, ep := range svc.Endpoints {
					if !strings.Contains(ep.URL, rgwCompatProjectID) {
						t.Errorf("catalog endpoint URL %q does not carry the canonical project id %q", ep.URL, rgwCompatProjectID)
					}
					if strings.Contains(ep.URL, rgwCompatProjectIDDashFree) {
						t.Errorf("catalog endpoint URL %q leaked the dash-free project id", ep.URL)
					}
				}
			}
			if !sawTemplate {
				t.Errorf("catalog missing the %s template service; catalog=%+v", rgwCompatCatalogType, body.Token.Catalog)
			}
		})
	}
}

// TestTokenProjectIDRGWCompatManagementCanonical: with the flag ON, the
// management APIs keep returning the canonical (hyphenated) project id — the
// transform is confined to the token payload.
func TestTokenProjectIDRGWCompatManagementCanonical(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	r := newRGWCompatRouter(true)

	code, body := doReq(r, http.MethodGet, "/v3/projects/"+rgwCompatProjectID, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("GET /v3/projects/{id}: code=%d body=%s; want 200", code, body)
	}
	var resp struct {
		Project struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode project response: %v (body=%s)", err, body)
	}
	if resp.Project.ID != rgwCompatProjectID {
		t.Errorf("GET /v3/projects/{id}.project.id = %q, want canonical %q (flag on must not touch management APIs)", resp.Project.ID, rgwCompatProjectID)
	}
	if resp.Project.Name != rgwCompatProjectName {
		t.Errorf("GET /v3/projects/{id}.project.name = %q, want %q", resp.Project.Name, rgwCompatProjectName)
	}
}

// TestTokenProjectIDRGWCompatUnscoped: unscoped tokens carry no project, so
// the flag cannot affect them — for either identity method.
func TestTokenProjectIDRGWCompatUnscoped(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	for _, rgwCompat := range []bool{false, true} {
		r := newRGWCompatRouter(rgwCompat)

		w := postTokens(t, r, passwordAuthBody(""))
		if w.Code != http.StatusCreated {
			t.Fatalf("flag=%v, password unscoped: code=%d body=%s", rgwCompat, w.Code, w.Body.String())
		}
		if body := decodeTokenBody(t, w); body.Token.Project != nil {
			t.Errorf("flag=%v, password unscoped: response carries a project: %+v", rgwCompat, *body.Token.Project)
		}

		w = postTokens(t, r, tokenAuthBody(w.Header().Get("X-Subject-Token"), ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("flag=%v, token unscoped: code=%d body=%s", rgwCompat, w.Code, w.Body.String())
		}
		if body := decodeTokenBody(t, w); body.Token.Project != nil {
			t.Errorf("flag=%v, token unscoped: response carries a project: %+v", rgwCompat, *body.Token.Project)
		}
	}
}

// TestTokenProjectIDRGWCompatValidation: GET /v3/auth/tokens — the endpoint
// Ceph RGW's Swift keystone engine validates every user token through — must
// return the standard keystone v3 token envelope (user{id,name,domain},
// expires_at/issued_at from the JWT claims, roles as objects,
// project{id,name,domain}, catalog), with token.project.id rendered through
// the same flag-gated transform as the issue responses and everything else
// canonical.
func TestTokenProjectIDRGWCompatValidation(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	cases := []struct {
		name      string
		rgwCompat bool
		wantID    string
	}{
		{"flag off", false, rgwCompatProjectID},
		{"flag on", true, rgwCompatProjectIDDashFree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRGWCompatRouter(tc.rgwCompat)

			issue := postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
			if issue.Code != http.StatusCreated {
				t.Fatalf("POST /v3/auth/tokens: code=%d body=%s", issue.Code, issue.Body.String())
			}
			issued := decodeTokenBody(t, issue)

			w := getToken(t, r, issue.Header().Get("X-Subject-Token"))
			if w.Code != http.StatusOK {
				t.Fatalf("GET /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
			}
			body := decodeValidationBody(t, w)

			// Mandatory envelope fields (rgw::keystone::TokenEnvelope::decode_json):
			// user.id/user.name, expires_at, roles[].name, project.id/project.name.
			if body.Token.User.ID != rgwCompatSeededAdminID {
				t.Errorf("token.user.id = %q, want canonical %q", body.Token.User.ID, rgwCompatSeededAdminID)
			}
			if body.Token.User.Name != "admin" {
				t.Errorf("token.user.name = %q, want %q", body.Token.User.Name, "admin")
			}
			if body.Token.User.Domain == nil || body.Token.User.Domain.ID != defaultDomainID {
				t.Errorf("token.user.domain = %+v, want domain id %q", body.Token.User.Domain, defaultDomainID)
			}
			if body.Token.ExpiresAt == "" {
				t.Error("token.expires_at missing (mandatory for Ceph RGW)")
			} else if _, err := time.Parse(time.RFC3339, body.Token.ExpiresAt); err != nil {
				t.Errorf("token.expires_at = %q, not RFC3339: %v", body.Token.ExpiresAt, err)
			}
			if body.Token.ExpiresAt != issued.Token.ExpiresAt {
				t.Errorf("token.expires_at = %q, want the issued token's %q", body.Token.ExpiresAt, issued.Token.ExpiresAt)
			}
			if body.Token.IssuedAt != issued.Token.IssuedAt {
				t.Errorf("token.issued_at = %q, want the issued token's %q", body.Token.IssuedAt, issued.Token.IssuedAt)
			}
			if body.Token.Project == nil {
				t.Fatal("validation response missing token.project")
			}
			if body.Token.Project.ID != tc.wantID {
				t.Errorf("token.project.id = %q, want %q", body.Token.Project.ID, tc.wantID)
			}
			if body.Token.Project.Name != rgwCompatProjectName {
				t.Errorf("token.project.name = %q, want %q", body.Token.Project.Name, rgwCompatProjectName)
			}
			if body.Token.Project.Domain.ID != defaultDomainID {
				t.Errorf("token.project.domain.id = %q, want canonical %q", body.Token.Project.Domain.ID, defaultDomainID)
			}
			if len(body.Token.Roles) == 0 {
				t.Fatal("validation response missing roles (mandatory for Ceph RGW)")
			}
			for _, role := range body.Token.Roles {
				if role.Name != "member" || role.ID != "member" {
					t.Errorf("token.roles entry = %+v, want {id: member, name: member}", role)
				}
			}

			// The catalog keeps the CANONICAL project id (template endpoint).
			var sawTemplate bool
			for _, svc := range body.Token.Catalog {
				if svc.Type != rgwCompatCatalogType {
					continue
				}
				sawTemplate = true
				for _, ep := range svc.Endpoints {
					if !strings.Contains(ep.URL, rgwCompatProjectID) {
						t.Errorf("catalog endpoint URL %q does not carry the canonical project id %q", ep.URL, rgwCompatProjectID)
					}
					if strings.Contains(ep.URL, rgwCompatProjectIDDashFree) {
						t.Errorf("catalog endpoint URL %q leaked the dash-free project id", ep.URL)
					}
				}
			}
			if !sawTemplate {
				t.Errorf("catalog missing the %s template service; catalog=%+v", rgwCompatCatalogType, body.Token.Catalog)
			}

			// The old flat shape must be gone: no project_id key, roles are
			// objects (not bare strings), and methods render from the claims
			// (["password"] here — the methods the token was issued with).
			var raw struct {
				Token map[string]any `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode raw validation response: %v", err)
			}
			if _, ok := raw.Token["project_id"]; ok {
				t.Error("validation response still carries the flat token.project_id key")
			}
			if body.Token.Methods == nil {
				t.Error("validation response omits token.methods; the claims now carry the issue methods")
			} else if len(body.Token.Methods) != 1 || body.Token.Methods[0] != "password" {
				t.Errorf("token.methods = %v, want [password]", body.Token.Methods)
			}
			if roles, ok := raw.Token["roles"].([]any); ok {
				for _, role := range roles {
					if _, ok := role.(string); ok {
						t.Errorf("validation response role %v is a bare string; Ceph RGW requires objects with a name", role)
					}
				}
			}
		})
	}
}

// TestTokenProjectIDRGWCompatValidationUnscoped: unscoped tokens carry no
// project, so the validation response omits token.project entirely — for
// either flag position.
func TestTokenProjectIDRGWCompatValidationUnscoped(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	for _, rgwCompat := range []bool{false, true} {
		r := newRGWCompatRouter(rgwCompat)

		issue := postTokens(t, r, passwordAuthBody(""))
		if issue.Code != http.StatusCreated {
			t.Fatalf("flag=%v, unscoped issue: code=%d body=%s", rgwCompat, issue.Code, issue.Body.String())
		}
		w := getToken(t, r, issue.Header().Get("X-Subject-Token"))
		if w.Code != http.StatusOK {
			t.Fatalf("flag=%v, unscoped validation: code=%d body=%s", rgwCompat, w.Code, w.Body.String())
		}
		if body := decodeValidationBody(t, w); body.Token.Project != nil {
			t.Errorf("flag=%v, unscoped validation: response carries a project: %+v", rgwCompat, *body.Token.Project)
		}

		var raw struct {
			Token map[string]any `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("flag=%v, decode raw validation response: %v", rgwCompat, err)
		}
		if _, ok := raw.Token["project"]; ok {
			t.Errorf("flag=%v, unscoped validation: response carries a project key", rgwCompat)
		}
	}
}

// mintTestToken signs a TokenClaims JWT with the router's secret, reaching
// states the HTTP issue paths never produce (missing or past expiry).
func mintTestToken(t *testing.T, claims *TokenClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(rgwCompatTestSecret))
	if err != nil {
		t.Fatalf("mint test token: %v", err)
	}
	return signed
}

// assertZTimestamp fails unless got is an RFC3339 string with a "Z" (UTC)
// tail that round-trips to the want instant. Go formats time.Time in the
// process-local zone; Ceph RGW's parse_iso8601 (extended format, the mode
// rgw_keystone.cc parses expires_at with) only accepts empty / "Z" /
// ".fractionZ" tails, so an offset tail breaks every RGW token validation.
func assertZTimestamp(t *testing.T, field, got string, want time.Time) {
	t.Helper()
	if !strings.HasSuffix(got, "Z") {
		t.Errorf("%s = %q, want a Z (UTC) suffix; offset tails are rejected by Ceph RGW's parse_iso8601", field, got)
	}
	// Real Keystone renders microsecond precision and /v3/s3tokens already
	// does (ksTime); Ceph's parse_iso8601 accepts the ".fractionZ" tail. Pin
	// the exact shape — 2006-01-02T15:04:05.000000Z — for byte-parity.
	if len(got) != 27 || got[19] != '.' || got[26] != 'Z' {
		t.Errorf("%s = %q, want the keystone microsecond form 2006-01-02T15:04:05.000000Z", field, got)
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Errorf("%s = %q, not RFC3339: %v", field, got, err)
		return
	}
	if !parsed.Equal(want) {
		t.Errorf("%s = %q parses to %s, want the claim instant %s", field, got, parsed.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestTokenTimestampsAreUTC: regression for non-UTC deployments. Go renders
// time.Time in the process-local zone, so with TZ != UTC the token responses
// used to carry "+HH:MM" tails that Ceph RGW's keystone decoder rejects. The
// test forces a non-UTC local zone and asserts every rendered timestamp —
// issue and validation, expires_at and issued_at — is Z-suffixed and carries
// the same instant as the JWT claims.
func TestTokenTimestampsAreUTC(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	origLocal := time.Local
	time.Local = time.FixedZone("regression-utc+2", 2*60*60)
	t.Cleanup(func() { time.Local = origLocal })

	r := newRGWCompatRouter(true)

	issue := postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
	if issue.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens: code=%d body=%s", issue.Code, issue.Body.String())
	}
	issued := decodeTokenBody(t, issue)
	tokenString := issue.Header().Get("X-Subject-Token")

	claims := &TokenClaims{}
	if _, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return []byte(rgwCompatTestSecret), nil
	}); err != nil {
		t.Fatalf("decode issued JWT: %v", err)
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatalf("issued JWT claims missing exp/iat: %+v", claims.RegisteredClaims)
	}

	assertZTimestamp(t, "issue token.expires_at", issued.Token.ExpiresAt, claims.ExpiresAt.Time)
	assertZTimestamp(t, "issue token.issued_at", issued.Token.IssuedAt, claims.IssuedAt.Time)

	w := getToken(t, r, tokenString)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
	}
	body := decodeValidationBody(t, w)
	assertZTimestamp(t, "validation token.expires_at", body.Token.ExpiresAt, claims.ExpiresAt.Time)
	assertZTimestamp(t, "validation token.issued_at", body.Token.IssuedAt, claims.IssuedAt.Time)
}

// TestValidateTokenRejectsMissingExpiry: Keystone tokens always carry an
// expiry; the JWT library only enforces exp when the claim is present, so a
// signed token without one must be rejected up front (401) instead of
// validating and rendering an empty expires_at — mandatory for Ceph RGW's
// TokenEnvelope decoder. Covers the GET validation and the token re-scope
// path (both route through AuthService.ValidateToken).
func TestValidateTokenRejectsMissingExpiry(t *testing.T) {
	r := newRGWCompatRouter(false)
	token := mintTestToken(t, &TokenClaims{
		UserID:   rgwCompatSeededAdminID,
		UserName: "admin",
	})

	w := getToken(t, r, token)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /v3/auth/tokens with exp-less token: code=%d body=%s, want %d", w.Code, w.Body.String(), http.StatusUnauthorized)
	}

	rescope := postTokens(t, r, tokenAuthBody(token, rgwCompatProjectID))
	if rescope.Code != http.StatusUnauthorized {
		t.Errorf("token re-scope with exp-less token: code=%d body=%s, want %d", rescope.Code, rescope.Body.String(), http.StatusUnauthorized)
	}
}

// TestValidateTokenRejectsExpired: expired tokens fail validation (401) on
// the GET validation and re-scope paths — jwt/v5 enforces exp when present.
func TestValidateTokenRejectsExpired(t *testing.T) {
	r := newRGWCompatRouter(false)
	token := mintTestToken(t, &TokenClaims{
		UserID:   rgwCompatSeededAdminID,
		UserName: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
	})

	w := getToken(t, r, token)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /v3/auth/tokens with expired token: code=%d body=%s, want %d", w.Code, w.Body.String(), http.StatusUnauthorized)
	}

	rescope := postTokens(t, r, tokenAuthBody(token, rgwCompatProjectID))
	if rescope.Code != http.StatusUnauthorized {
		t.Errorf("token re-scope with expired token: code=%d body=%s, want %d", rescope.Code, rescope.Body.String(), http.StatusUnauthorized)
	}
}

// TestValidateTokenDeadScopeOmitsProject: when the token's project row is
// gone, the validation envelope omits token.project but still renders the
// user and the roles from the claims. A scoped envelope without a project is
// rejected by Ceph RGW's parser — the correct outcome for a dead scope —
// instead of inventing project data the database no longer backs.
func TestValidateTokenDeadScopeOmitsProject(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	r := newRGWCompatRouter(false)

	issue := postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
	if issue.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens: code=%d body=%s", issue.Code, issue.Body.String())
	}
	tokenString := issue.Header().Get("X-Subject-Token")

	// Delete the project (role assignments first — the FK requires it),
	// leaving a realistically dead scope. seedRGWCompatFixtures' cleanup is
	// idempotent against the missing rows.
	if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE project_id = $1", rgwCompatProjectID); err != nil {
		t.Fatalf("delete role assignments: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM projects WHERE id = $1", rgwCompatProjectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	w := getToken(t, r, tokenString)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
	}
	body := decodeValidationBody(t, w)

	if body.Token.Project != nil {
		t.Errorf("dead scope: token.project = %+v, want the key omitted", *body.Token.Project)
	}
	var raw struct {
		Token map[string]any `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw validation response: %v", err)
	}
	if _, ok := raw.Token["project"]; ok {
		t.Error("dead scope: validation response carries a project key")
	}
	if body.Token.User.ID != rgwCompatSeededAdminID {
		t.Errorf("dead scope: token.user.id = %q, want the claim's %q", body.Token.User.ID, rgwCompatSeededAdminID)
	}
	if len(body.Token.Roles) != 0 {
		t.Errorf("dead scope: roles = %v, want none — the assignments died with the scope (uniform liveness, fail closed)", body.Token.Roles)
	}
	if _, ok := raw.Token["roles"]; ok {
		t.Error("dead scope: validation response carries a roles key")
	}
	if body.Token.ExpiresAt == "" {
		t.Error("dead scope: token.expires_at missing (mandatory for Ceph RGW)")
	}
}

// postJSON drives r with a JSON body and extra headers, returning the recorder.
func postJSON(t *testing.T, r *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestDashFreeProjectID: the RGW boundary uses Keystone's established
// dash-free form while internal project IDs remain canonical UUIDs.
func TestDashFreeProjectID(t *testing.T) {

	cases := []struct{ in, want string }{
		{rgwCompatProjectID, rgwCompatProjectIDDashFree},                         // canonical UUID becomes dash-free
		{rgwCompatProjectIDDashFree, rgwCompatProjectIDDashFree},                 // already dash-free
		{"32hexnodasheshere000000000000000", "32hexnodasheshere000000000000000"}, // hyphen-less: unchanged
		{"my_underscore_project", "my_underscore_project"},                       // non-UUID: unchanged
		{"", ""},
	}
	for _, tc := range cases {
		if got := canonicalID(tc.in); got != tc.want {
			t.Errorf("canonicalID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestScopeByAdvertisedProjectID: the project id exactly as the token
// envelope advertises it (dash-free while rgw_compat_tenant_ids is on)
// must round-trip through every client-input site that accepts a project
// id: the password scope, the token re-scope, and the EC2 credential
// tenant_id. PostgreSQL accepts the dash-free UUID input; the output encoding
// remains controlled solely by the flag.
func TestScopeByAdvertisedProjectID(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	t.Run("flag on: password scope by advertised id", func(t *testing.T) {
		r := newRGWCompatRouter(true)
		w := postTokens(t, r, passwordAuthBody(rgwCompatProjectIDDashFree))
		if w.Code != http.StatusCreated {
			t.Fatalf("POST /v3/auth/tokens with advertised id: code=%d body=%s", w.Code, w.Body.String())
		}
		body := decodeTokenBody(t, w)
		if body.Token.Project == nil || body.Token.Project.ID != rgwCompatProjectIDDashFree {
			t.Errorf("token.project = %+v, want the advertised dash-free id %q", body.Token.Project, rgwCompatProjectIDDashFree)
		}
	})

	t.Run("flag on: token re-scope by advertised id", func(t *testing.T) {
		r := newRGWCompatRouter(true)
		unscoped := postTokens(t, r, passwordAuthBody(""))
		if unscoped.Code != http.StatusCreated {
			t.Fatalf("unscoped auth: code=%d body=%s", unscoped.Code, unscoped.Body.String())
		}
		w := postTokens(t, r, tokenAuthBody(unscoped.Header().Get("X-Subject-Token"), rgwCompatProjectIDDashFree))
		if w.Code != http.StatusCreated {
			t.Fatalf("re-scope with advertised id: code=%d body=%s", w.Code, w.Body.String())
		}
		body := decodeTokenBody(t, w)
		if body.Token.Project == nil || body.Token.Project.ID != rgwCompatProjectIDDashFree {
			t.Errorf("token.project = %+v, want the advertised dash-free id %q", body.Token.Project, rgwCompatProjectIDDashFree)
		}
	})

	t.Run("flag on: EC2 mint with advertised tenant_id stores canonical", func(t *testing.T) {
		r := newRGWCompatRouter(true)
		w := postJSON(t, r, http.MethodPost,
			"/v3/users/"+rgwCompatSeededAdminID+"/credentials/OS-EC2",
			fmt.Sprintf(`{"tenant_id":%q}`, rgwCompatProjectIDDashFree),
			map[string]string{"X-Test-User": rgwCompatSeededAdminID, "X-Test-Roles": "admin"})
		if w.Code != http.StatusCreated {
			t.Fatalf("EC2 mint with advertised tenant_id: code=%d body=%s", w.Code, w.Body.String())
		}
		var cred struct {
			Credential struct {
				TenantID string `json:"tenant_id"`
			} `json:"credential"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &cred); err != nil {
			t.Fatalf("decode credential: %v (body=%s)", err, w.Body.String())
		}
		if cred.Credential.TenantID != rgwCompatProjectID {
			t.Errorf("credential.tenant_id = %q, want the canonical %q (canonicalized before storage)", cred.Credential.TenantID, rgwCompatProjectID)
		}
	})

	t.Run("flag on: EC2 mint with alternate UUID encodings stores canonical", func(t *testing.T) {
		// uuid.Parse (like Postgres) accepts valid UUIDs in alternate text
		// encodings; the blob retains the stored spelling and /v3/s3tokens
		// derives the S3 tenant from it, so a non-canonical mint would split
		// the S3 and Swift implicit tenants. The mint canonicalizes.
		for _, raw := range []string{
			strings.ReplaceAll(rgwCompatProjectID, "-", ""), // 32 hex, no dashes
			strings.ToUpper(rgwCompatProjectID),             // uppercase, hyphenated
			"{" + rgwCompatProjectID + "}",                  // braced
		} {
			r := newRGWCompatRouter(true)
			w := postJSON(t, r, http.MethodPost,
				"/v3/users/"+rgwCompatSeededAdminID+"/credentials/OS-EC2",
				fmt.Sprintf(`{"tenant_id":%q}`, raw),
				map[string]string{"X-Test-User": rgwCompatSeededAdminID, "X-Test-Roles": "admin"})
			if w.Code != http.StatusCreated {
				t.Fatalf("EC2 mint with alternate tenant_id %q: code=%d body=%s", raw, w.Code, w.Body.String())
			}
			var cred struct {
				Credential struct {
					TenantID string `json:"tenant_id"`
				} `json:"credential"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &cred); err != nil {
				t.Fatalf("decode credential: %v (body=%s)", err, w.Body.String())
			}
			if cred.Credential.TenantID != rgwCompatProjectID {
				t.Errorf("alternate tenant_id %q stored as %q, want the canonical %q", raw, cred.Credential.TenantID, rgwCompatProjectID)
			}
		}
	})

	t.Run("flag off: dash-free input remains accepted", func(t *testing.T) {
		r := newRGWCompatRouter(false)
		w := postTokens(t, r, passwordAuthBody(rgwCompatProjectIDDashFree))
		if w.Code != http.StatusCreated {
			t.Fatalf("flag off, scope by dash-free id: code=%d body=%s", w.Code, w.Body.String())
		}
		body := decodeTokenBody(t, w)
		if body.Token.Project == nil || body.Token.Project.ID != rgwCompatProjectID {
			t.Errorf("flag off: token.project = %+v, want canonical id %q", body.Token.Project, rgwCompatProjectID)
		}
	})
}

// TestRenderS3TokenTenantEncoding: /v3/s3tokens must derive its project.id
// (the RGW S3 implicit tenant) from the same transform as the Swift token
// payloads whenever rgw_compat_tenant_ids is on, so S3- and Swift-written
// objects for one project land in ONE implicit tenant. S3 already uses the
// historical dash-free keystone-canonical form. token.user.id is not
// used for tenant naming under implicit tenants and stays dash-free either way.
func TestRenderS3TokenTenantEncoding(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	dashFree := strings.ReplaceAll(rgwCompatProjectID, "-", "")
	for _, tc := range []struct {
		name      string
		rgwCompat bool
		want      string
	}{
		{"flag off: dash-free keystone-canonical form", false, dashFree},
		{"flag on: dash-free, matching the Swift token payload", true, dashFree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authSvc := NewAuthService(testPool, rgwCompatTestSecret, time.Hour, nil)
			authSvc.SetRGWCompatTenantIDs(tc.rgwCompat)
			svc := NewService(testPool, authSvc, nil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v3/s3tokens", nil)
			svc.renderS3Token(c, rgwCompatSeededAdminID, rgwCompatProjectID)

			if w.Code != http.StatusOK {
				t.Fatalf("renderS3Token: code=%d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Token struct {
					Project struct {
						ID string `json:"id"`
					} `json:"project"`
					User struct {
						ID string `json:"id"`
					} `json:"user"`
				} `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode s3tokens envelope: %v (body=%s)", err, w.Body.String())
			}
			if body.Token.Project.ID != tc.want {
				t.Errorf("token.project.id = %q, want %q", body.Token.Project.ID, tc.want)
			}
			if body.Token.User.ID != strings.ReplaceAll(rgwCompatSeededAdminID, "-", "") {
				t.Errorf("token.user.id = %q, want the dash-free form of the user id (unchanged in either mode)", body.Token.User.ID)
			}
		})
	}

	// Alternate valid UUID text encodings — accepted by uuid.Parse and by
	// Postgres on the way in (32 hex chars without dashes, uppercase hex,
	// braced) — must not split the S3 and Swift tenants. The credential mint
	// stores the canonical database UUID and the response renders dash-free.
	t.Run("flag on: alternate UUID encodings canonicalize before encoding", func(t *testing.T) {
		authSvc := NewAuthService(testPool, rgwCompatTestSecret, time.Hour, nil)
		authSvc.SetRGWCompatTenantIDs(true)
		svc := NewService(testPool, authSvc, nil)

		for _, raw := range []string{
			strings.ReplaceAll(rgwCompatProjectID, "-", ""), // 32 hex, no dashes
			strings.ToUpper(rgwCompatProjectID),             // uppercase, hyphenated
			"{" + rgwCompatProjectID + "}",                  // braced
		} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v3/s3tokens", nil)
			svc.renderS3Token(c, rgwCompatSeededAdminID, raw)

			if w.Code != http.StatusOK {
				t.Fatalf("renderS3Token(%q): code=%d body=%s", raw, w.Code, w.Body.String())
			}
			var body struct {
				Token struct {
					Project struct {
						ID string `json:"id"`
					} `json:"project"`
				} `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode s3tokens envelope: %v (body=%s)", err, w.Body.String())
			}
			if body.Token.Project.ID != dashFree {
				t.Errorf("raw %q rendered as %q, want the canonical dash-free %q (the SAME tenant as Swift)", raw, body.Token.Project.ID, dashFree)
			}
		}
	})
}

// TestValidateTokenDeadUserOmitsUser: a token whose user row is gone must
// omit the user key from the validation envelope — the exact mirror of the
// dead-scope handling. Ceph RGW's envelope parser requires a user, so the
// token is dead for it; the rendered identity never outlives the identity
// row. The JWT itself stays valid (signature + expiry), so the status is
// still 200 — only the envelope fails closed.
func TestValidateTokenDeadUserOmitsUser(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	r := newRGWCompatRouter(true)

	// A throwaway user (spec007- prefix: resetAdmin sweeps leftovers) with a
	// member role on the fixture project and a validly-signed scoped token.
	const deadUserID = "deadbeef-0000-4000-8000-00000000dead"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO users (id, name, password_hash, enabled, domain_id)
		VALUES ($1, 'spec007-deaduser', 'not-a-hash', true, $2)
		ON CONFLICT (id) DO NOTHING`, deadUserID, defaultDomainID); err != nil {
		t.Fatalf("insert throwaway user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		deadUserID, rgwCompatProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("insert role assignment: %v", err)
	}
	token := mintTestToken(t, &TokenClaims{
		UserID:    deadUserID,
		UserName:  "spec007-deaduser",
		ProjectID: rgwCompatProjectID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	// Positive control: while the user row exists, the envelope renders it.
	live := getToken(t, r, token)
	if live.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (live user): code=%d body=%s", live.Code, live.Body.String())
	}
	if b := decodeValidationBody(t, live); b.Token.User.ID != deadUserID {
		t.Errorf("live user: token.user.id = %q, want %q", b.Token.User.ID, deadUserID)
	}

	// Kill the user (assignments first — the FK requires it).
	if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE user_id = $1", deadUserID); err != nil {
		t.Fatalf("delete role assignments: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM users WHERE id = $1", deadUserID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	w := getToken(t, r, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (dead user): code=%d body=%s", w.Code, w.Body.String())
	}
	var raw struct {
		Token map[string]any `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw validation response: %v", err)
	}
	if _, ok := raw.Token["user"]; ok {
		t.Error("dead user: validation response carries a user key")
	}
	if _, ok := raw.Token["roles"]; ok {
		t.Error("dead user: validation response carries a roles key (the assignments died with the user)")
	}
	if _, ok := raw.Token["project"]; !ok {
		t.Error("dead user: validation response should still carry the (alive) project key")
	}
}

// TestValidateTokenDisabledUserOmitsUser: disabling a user takes effect on
// the validation envelope immediately — the issue paths reject disabled
// users with 401, so the live validation fails closed the same way as a
// deleted user (omit the user key; RGW's envelope parser requires one, so
// the token is dead for it). Unlike a deleted user, the role assignments
// survive a disable, so roles and the (alive) project keep rendering.
func TestValidateTokenDisabledUserOmitsUser(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	r := newRGWCompatRouter(true)

	// A throwaway user (spec007- prefix: resetAdmin sweeps leftovers) with a
	// member role on the fixture project and a validly-signed scoped token.
	const disabledUserID = "deadbeef-0000-4000-8000-0000d15ab1ed"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO users (id, name, password_hash, enabled, domain_id)
		VALUES ($1, 'spec007-disableduser', 'not-a-hash', true, $2)
		ON CONFLICT (id) DO NOTHING`, disabledUserID, defaultDomainID); err != nil {
		t.Fatalf("insert throwaway user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		disabledUserID, rgwCompatProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("insert role assignment: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE user_id = $1", disabledUserID); err != nil {
			t.Errorf("cleanup throwaway role assignments: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM users WHERE id = $1", disabledUserID); err != nil {
			t.Errorf("cleanup throwaway user: %v", err)
		}
	})
	token := mintTestToken(t, &TokenClaims{
		UserID:    disabledUserID,
		UserName:  "spec007-disableduser",
		ProjectID: rgwCompatProjectID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	// Positive control: while enabled, the envelope renders the user.
	live := getToken(t, r, token)
	if live.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (enabled user): code=%d body=%s", live.Code, live.Body.String())
	}
	if b := decodeValidationBody(t, live); b.Token.User.ID != disabledUserID {
		t.Errorf("enabled user: token.user.id = %q, want %q", b.Token.User.ID, disabledUserID)
	}

	// Disable the user (row and assignments stay).
	if _, err := testPool.Exec(ctx, "UPDATE users SET enabled = false WHERE id = $1", disabledUserID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	w := getToken(t, r, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (disabled user): code=%d body=%s", w.Code, w.Body.String())
	}
	var raw struct {
		Token map[string]any `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw validation response: %v", err)
	}
	if _, ok := raw.Token["user"]; ok {
		t.Error("disabled user: validation response carries a user key")
	}
	if _, ok := raw.Token["roles"]; !ok {
		t.Error("disabled user: validation response should still carry roles (assignments survive a disable)")
	}
	if _, ok := raw.Token["project"]; !ok {
		t.Error("disabled user: validation response should still carry the (alive) project key")
	}
}

// TestValidateTokenDisabledProjectOmitsProject: disabling the scope project
// takes effect on the validation envelope immediately — the issue paths
// reject disabled projects with 401, so the live validation fails closed the
// same way as a deleted project (omit the project key; RGW's parser rejects
// a scoped token without one). The user and its roles keep rendering.
func TestValidateTokenDisabledProjectOmitsProject(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	r := newRGWCompatRouter(true)

	const disabledProjectID = "cafebabe-0000-4000-8000-0000d15ab1ed"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO projects (id, name, description, enabled, domain_id)
		VALUES ($1, 'spec007-disabled-project', 'disabled scope fixture', true, $2)
		ON CONFLICT (id) DO NOTHING`, disabledProjectID, defaultDomainID); err != nil {
		t.Fatalf("insert throwaway project: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		rgwCompatSeededAdminID, disabledProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("insert role assignment: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE project_id = $1", disabledProjectID); err != nil {
			t.Errorf("cleanup throwaway role assignments: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM projects WHERE id = $1", disabledProjectID); err != nil {
			t.Errorf("cleanup throwaway project: %v", err)
		}
	})
	token := mintTestToken(t, &TokenClaims{
		UserID:    rgwCompatSeededAdminID,
		UserName:  "admin",
		ProjectID: disabledProjectID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	// Positive control: while enabled, the envelope renders the (dash-free,
	// flag on) project.
	live := getToken(t, r, token)
	if live.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (enabled project): code=%d body=%s", live.Code, live.Body.String())
	}
	if b := decodeValidationBody(t, live); b.Token.Project == nil || b.Token.Project.ID != canonicalID(disabledProjectID) {
		t.Errorf("enabled project: token.project.id = %+v, want the dash-free %q", b.Token.Project, canonicalID(disabledProjectID))
	}

	// Disable the project (row and assignments stay).
	if _, err := testPool.Exec(ctx, "UPDATE projects SET enabled = false WHERE id = $1", disabledProjectID); err != nil {
		t.Fatalf("disable project: %v", err)
	}

	w := getToken(t, r, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens (disabled project): code=%d body=%s", w.Code, w.Body.String())
	}
	var raw struct {
		Token map[string]any `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw validation response: %v", err)
	}
	if _, ok := raw.Token["project"]; ok {
		t.Error("disabled project: validation response carries a project key")
	}
	if _, ok := raw.Token["user"]; !ok {
		t.Error("disabled project: validation response should still carry the (alive) user key")
	}
}

// TestTokenMethodsAreServerRecorded: identity.methods in the auth request is
// caller input and is never validated against the credentials the handler
// authenticated — it must not be signed into the JWT. The password path
// records ["password"], and a re-scope inherits the methods of the token
// being re-scoped (Keystone's token plugin: "we want to maintain the
// values"), falling back to ["token"] for tokens minted before the claims
// carried methods. A spoofed ["totp"] in the request must not survive.
func TestTokenMethodsAreServerRecorded(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	r := newRGWCompatRouter(true)

	// Password path: the request claims totp, the handler authenticated a
	// password (dispatch is on the credential objects, not the methods).
	spoofed := `{"auth":{"identity":{"methods":["totp"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},"scope":{"project":{"id":"` + rgwCompatProjectID + `"}}}}`
	issue := postTokens(t, r, spoofed)
	if issue.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens (spoofed methods): code=%d body=%s", issue.Code, issue.Body.String())
	}
	if b := decodeTokenBody(t, issue); len(b.Token.Methods) != 1 || b.Token.Methods[0] != "password" {
		t.Errorf("issue token.methods = %v, want [password] (server-recorded)", b.Token.Methods)
	}
	tokenString := issue.Header().Get("X-Subject-Token")

	// The signed claims carry the recorded methods — the validation envelope
	// republishes those, not the request's ["totp"].
	w := getToken(t, r, tokenString)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
	}
	if b := decodeValidationBody(t, w); len(b.Token.Methods) != 1 || b.Token.Methods[0] != "password" {
		t.Errorf("validation token.methods = %v, want [password] (from the claims)", b.Token.Methods)
	}

	// Token path: inherit the re-scoped token's methods, not the request's.
	rescopeSpoofed := `{"auth":{"identity":{"methods":["totp"],"token":{"id":"` + tokenString + `"}},"scope":{"project":{"id":"` + rgwCompatProjectID + `"}}}}`
	rescope := postTokens(t, r, rescopeSpoofed)
	if rescope.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens (re-scope, spoofed methods): code=%d body=%s", rescope.Code, rescope.Body.String())
	}
	if b := decodeTokenBody(t, rescope); len(b.Token.Methods) != 1 || b.Token.Methods[0] != "password" {
		t.Errorf("re-scope token.methods = %v, want the inherited [password]", b.Token.Methods)
	}

	// Re-scope of a legacy token (claims without methods) falls back to
	// ["token"] — the method this path actually used.
	legacy := mintTestToken(t, &TokenClaims{
		UserID:    rgwCompatSeededAdminID,
		UserName:  "admin",
		ProjectID: rgwCompatProjectID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	legacyRescope := postTokens(t, r, tokenAuthBody(legacy, rgwCompatProjectID))
	if legacyRescope.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens (legacy re-scope): code=%d body=%s", legacyRescope.Code, legacyRescope.Body.String())
	}
	if b := decodeTokenBody(t, legacyRescope); len(b.Token.Methods) != 1 || b.Token.Methods[0] != "token" {
		t.Errorf("legacy re-scope token.methods = %v, want [token] (fallback for pre-methods tokens)", b.Token.Methods)
	}
}

// TestValidateTokenRoleRevocation: revoking the role assignment takes effect
// on the validation envelope immediately — roles are re-read live from the
// role assignments, not replayed from the JWT claims. Ceph RGW requires a
// non-empty roles list, so a fully revoked scoped token is dead for it, while
// the user and project keys stay rendered.
func TestValidateTokenRoleRevocation(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	r := newRGWCompatRouter(true)

	issue := postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
	if issue.Code != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens: code=%d body=%s", issue.Code, issue.Body.String())
	}
	tokenString := issue.Header().Get("X-Subject-Token")

	if b := decodeTokenBody(t, issue); len(b.Token.Roles) == 0 {
		t.Fatal("issue response should carry the member role")
	}

	// Revoke the only assignment.
	if _, err := testPool.Exec(ctx,
		"DELETE FROM role_assignments WHERE user_id = $1 AND project_id = $2",
		rgwCompatSeededAdminID, rgwCompatProjectID); err != nil {
		t.Fatalf("revoke role assignment: %v", err)
	}

	w := getToken(t, r, tokenString)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v3/auth/tokens: code=%d body=%s", w.Code, w.Body.String())
	}
	if body := decodeValidationBody(t, w); len(body.Token.Roles) != 0 {
		t.Errorf("revoked roles still render: %v, want none (live read, fail closed)", body.Token.Roles)
	}
	var raw struct {
		Token map[string]any `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw validation response: %v", err)
	}
	if _, ok := raw.Token["roles"]; ok {
		t.Error("revoked roles: validation response carries a roles key")
	}
	if _, ok := raw.Token["project"]; !ok {
		t.Error("revocation: the project key should stay (only the assignment died)")
	}
	if _, ok := raw.Token["user"]; !ok {
		t.Error("revocation: the user key should stay (the user is alive)")
	}
}
