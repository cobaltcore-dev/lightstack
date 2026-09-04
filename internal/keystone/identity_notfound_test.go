package keystone

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
)

// TestIdentityNotFound_ReadPaths (US1) covers the ticket's core resources: a
// malformed OR a valid-but-unknown id returns the identical Keystone-verbatim
// 404 ("Could not find <resource>: <id>."), while an existing id still 200s.
// DB-backed (requireDB); routes are wired via the real RegisterRoutes.
func TestIdentityNotFound_ReadPaths(t *testing.T) {
	requireDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(nil, nil)
	svc.RegisterRoutes(&r.RouterGroup, func(c *gin.Context) { c.Next() })

	serve := func(path string) (int, string) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code, w.Body.String()
	}

	const unknown = "00000000-0000-0000-0000-0000000000ff"
	for _, res := range []struct{ noun, base, table string }{
		{"project", "/v3/projects/", "projects"},
		{"user", "/v3/users/", "users"},
		{"role", "/v3/roles/", "roles"},
	} {
		t.Run(res.noun, func(t *testing.T) {
			var existing string
			if err := database.DB.QueryRow(context.Background(),
				"SELECT id FROM "+res.table+" LIMIT 1").Scan(&existing); err != nil {
				t.Fatalf("no seeded %s row to test the 200 case: %v", res.noun, err)
			}

			// malformed id -> verbatim 404 (via middleware, before the DB)
			if code, body := serve(res.base + "not-a-uuid"); code != http.StatusNotFound ||
				!strings.Contains(body, "Could not find "+res.noun+": not-a-uuid.") {
				t.Errorf("malformed: code=%d body=%s", code, body)
			}
			// valid-but-unknown id -> identical verbatim 404 (via the handler)
			if code, body := serve(res.base + unknown); code != http.StatusNotFound ||
				!strings.Contains(body, "Could not find "+res.noun+": "+unknown+".") {
				t.Errorf("unknown: code=%d body=%s", code, body)
			}
			// existing id -> unchanged success
			if code, _ := serve(res.base + existing); code != http.StatusOK {
				t.Errorf("existing %s (%s): code=%d, want 200", res.noun, existing, code)
			}
		})
	}
}
