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

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Fixtures for the rgw_compat_tenant_ids tests: a dedicated project with a
// fixed hyphenated UUID (so the canonical and underscored forms are obvious),
// the member role for the seeded admin (RGW's rgw_keystone_accepted_roles
// default), and a catalog service whose endpoint URL carries a {project_id}
// template — proving BuildServiceCatalog keeps receiving the canonical id.
const (
	rgwCompatProjectID            = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	rgwCompatProjectIDUnderscored = "1a2b3c4d_5e6f_4a7b_8c9d_0e1f2a3b4c5d"
	rgwCompatProjectName          = "rgw-compat-test-project"
	rgwCompatServiceID            = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	rgwCompatCatalogType          = "rgw-compat-test"
	rgwCompatSeededAdminID        = "00000000-0000-0000-0000-000000000001"
	rgwCompatMemberRoleID         = "00000000-0000-0000-0000-000000000004"

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
	authSvc := NewAuthService(rgwCompatTestSecret, time.Hour, nil)
	authSvc.SetRGWCompatTenantIDs(rgwCompat)
	NewService(authSvc, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
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

	if _, err := database.DB.Exec(ctx, `
		INSERT INTO projects (id, name, description, enabled, domain_id)
		VALUES ($1, $2, 'rgw_compat_tenant_ids test fixture', true, $3)
		ON CONFLICT (id) DO NOTHING`,
		rgwCompatProjectID, rgwCompatProjectName, defaultDomainID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, project_id, role_id) DO NOTHING`,
		rgwCompatSeededAdminID, rgwCompatProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("seed role assignment: %v", err)
	}
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO services (id, type, name, description, enabled)
		VALUES ($1, $2, 'rgw-compat-test', 'rgw_compat_tenant_ids test fixture', true)
		ON CONFLICT (id) DO NOTHING`,
		rgwCompatServiceID, rgwCompatCatalogType); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO endpoints (service_id, interface, url, region, enabled)
		VALUES ($1, 'public', 'http://lightstack:9999/v2.1/{project_id}', 'RegionOne', true)
		ON CONFLICT DO NOTHING`,
		rgwCompatServiceID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	t.Cleanup(func() {
		if _, err := database.DB.Exec(ctx, "DELETE FROM endpoints WHERE service_id = $1", rgwCompatServiceID); err != nil {
			t.Errorf("cleanup fixture endpoints: %v", err)
		}
		if _, err := database.DB.Exec(ctx, "DELETE FROM services WHERE id = $1", rgwCompatServiceID); err != nil {
			t.Errorf("cleanup fixture service: %v", err)
		}
		if _, err := database.DB.Exec(ctx, "DELETE FROM role_assignments WHERE project_id = $1", rgwCompatProjectID); err != nil {
			t.Errorf("cleanup fixture role assignments: %v", err)
		}
		if _, err := database.DB.Exec(ctx, "DELETE FROM projects WHERE id = $1", rgwCompatProjectID); err != nil {
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
		ExpiresAt string `json:"expires_at"`
		IssuedAt  string `json:"issued_at"`
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

// TestRGWCompatTenantID: the pure transform is exactly '-' → '_' and nothing
// else — hyphenless ids (real Keystone's 32-hex form) pass through unchanged.
func TestRGWCompatTenantID(t *testing.T) {
	cases := []struct{ in, want string }{
		{rgwCompatProjectID, rgwCompatProjectIDUnderscored},
		{"00000000-0000-0000-0000-000000000002", "00000000_0000_0000_0000_000000000002"},
		{"0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"}, // no hyphens: unchanged
		{"a-b-c", "a_b_c"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := rgwCompatTenantID(tc.in); got != tc.want {
			t.Errorf("rgwCompatTenantID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTokenProjectIDGating: the flag gates the transform; the default is off
// (canonical hyphenated UUIDs).
func TestTokenProjectIDGating(t *testing.T) {
	s := NewAuthService("k", time.Hour, nil)
	if got := s.tokenProjectID(rgwCompatProjectID); got != rgwCompatProjectID {
		t.Errorf("flag off (default): tokenProjectID = %q, want canonical %q", got, rgwCompatProjectID)
	}
	s.SetRGWCompatTenantIDs(true)
	if got := s.tokenProjectID(rgwCompatProjectID); got != rgwCompatProjectIDUnderscored {
		t.Errorf("flag on: tokenProjectID = %q, want %q", got, rgwCompatProjectIDUnderscored)
	}
	if got := s.tokenProjectID("0123456789abcdef0123456789abcdef"); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("flag on, hyphenless id: tokenProjectID = %q, want it unchanged", got)
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
		{"password issue, flag on", true, false, rgwCompatProjectIDUnderscored},
		{"token re-scope, flag off", false, true, rgwCompatProjectID},
		{"token re-scope, flag on", true, true, rgwCompatProjectIDUnderscored},
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
					if strings.Contains(ep.URL, rgwCompatProjectIDUnderscored) {
						t.Errorf("catalog endpoint URL %q leaked the underscored project id", ep.URL)
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
		{"flag on", true, rgwCompatProjectIDUnderscored},
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
					if strings.Contains(ep.URL, rgwCompatProjectIDUnderscored) {
						t.Errorf("catalog endpoint URL %q leaked the underscored project id", ep.URL)
					}
				}
			}
			if !sawTemplate {
				t.Errorf("catalog missing the %s template service; catalog=%+v", rgwCompatCatalogType, body.Token.Catalog)
			}

			// The old flat shape must be gone: no project_id key, roles are
			// objects (not bare strings), and methods — not carried by the
			// JWT claims — are omitted.
			var raw struct {
				Token map[string]any `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode raw validation response: %v", err)
			}
			if _, ok := raw.Token["project_id"]; ok {
				t.Error("validation response still carries the flat token.project_id key")
			}
			if _, ok := raw.Token["methods"]; ok {
				t.Error("validation response carries token.methods; the JWT claims do not carry the identity methods")
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
