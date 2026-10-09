package keystone

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TestProjectReadsSurviveNullableColumns: projects.description and projects.domain_id are
// NULLABLE, and a project row with neither is legitimate (another service's fixtures create
// exactly that). ListProjects already read them as nullable; four other reads scanned them
// into plain strings, so such a project answered 500 — and so did every listing it appeared
// in, since one failed scan aborts the whole result set. Each must render the zero value.
func TestProjectReadsSurviveNullableColumns(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const adminUserID = "00000000-0000-0000-0000-000000000001"

	projectID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, description, domain_id, enabled, created_at, updated_at)
		 VALUES ($1, $2, NULL, NULL, true, now(), now())`, projectID, "spec025-null-columns-"+projectID[:8]); err != nil {
		t.Fatalf("seeding the project: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO role_assignments (id, user_id, project_id, role_id, created_at)
		 VALUES ($1, $2, $3, (SELECT id FROM roles WHERE name = 'member'), now())`,
		uuid.NewString(), adminUserID, projectID); err != nil {
		t.Fatalf("seeding the role assignment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM role_assignments WHERE project_id = $1", projectID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", projectID)
	})

	// GET /v3/auth/projects reads the caller from its own token, so the service gets a real
	// token issuer and the test a real token.
	resetAdmin(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(testPool, NewAuthService(testPool, "null-columns-test-secret", time.Hour, nil), nil)
	svc.RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
		c.Set("user_id", adminUserID)
		c.Set("roles", []string{"admin"})
		c.Next()
	})
	issued := postTokens(t, r, passwordAuthBody("00000000-0000-0000-0000-000000000002"))
	token := issued.Header().Get("X-Subject-Token")
	if token == "" {
		t.Fatalf("no token issued: %d %s", issued.Code, issued.Body.String())
	}
	serve := func(method, path, body string) (int, string) {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Auth-Token", token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	for _, rd := range []struct{ name, method, path, body string }{
		{"show", http.MethodGet, "/v3/projects/" + projectID, ""},
		{"update", http.MethodPatch, "/v3/projects/" + projectID, `{"project":{"enabled":true}}`},
		{"user's projects", http.MethodGet, "/v3/users/" + adminUserID + "/projects", ""},
		{"auth projects", http.MethodGet, "/v3/auth/projects", ""},
		{"list", http.MethodGet, "/v3/projects", ""},
	} {
		code, body := serve(rd.method, rd.path, rd.body)
		if code != http.StatusOK {
			t.Errorf("%s with a project lacking description and domain: %d; want 200. body: %.200s", rd.name, code, body)
			continue
		}
		if !strings.Contains(body, projectID) {
			t.Errorf("%s does not carry the project; body: %.300s", rd.name, body)
		}
		var anyBody map[string]any
		if err := json.Unmarshal([]byte(body), &anyBody); err != nil {
			t.Errorf("%s: body is not JSON: %v", rd.name, err)
		}
	}
}
