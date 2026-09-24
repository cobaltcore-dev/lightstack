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
)

// newRGWCompatRouter wires the real RegisterRoutes behind a real AuthService
// carrying the rgw_compat_tenant_ids flag, plus the same stub auth middleware
// as newIdentityTestRouter (roles / user_id from X-Test-* headers) so the
// management routes can be exercised without minting JWTs for them.
func newRGWCompatRouter(rgwCompat bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authSvc := NewAuthService("rgw-compat-test-secret", time.Hour, nil)
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
		Project *struct {
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
