package keystone

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
)

// seededDefaultProjectID is the `default` project from migration 002_seed_data.
const seededDefaultProjectID = "00000000-0000-0000-0000-000000000002"

// TestServiceRunsOnItsOwnPool is ADR-001's payoff (SPEC-027 FR-008): a Keystone
// service built on a pool this test opened itself. It never reads testPool and
// needs no package-level setup, so it passes when run on its own:
//
//	go test -run TestServiceRunsOnItsOwnPool ./internal/keystone/
//
// Both pool holders are exercised: the AuthService authenticates a password,
// the Service reads a project.
func TestServiceRunsOnItsOwnPool(t *testing.T) {
	dbURL := dbtest.URL()
	if dbURL == "" {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run the pool-injection demonstrator")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, dbURL, database.Options{Pool: database.PoolConfig{
		MaxConns:          2,
		MinConns:          1,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
	}})
	if err != nil {
		t.Fatalf("connect to LIGHTSTACK_TEST_DB_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	// Earlier tests in this package can leave the seeded admin with another
	// password; restore it through this test's own pool.
	if _, err := pool.Exec(ctx,
		"UPDATE users SET password_hash = $1, enabled = true WHERE name = 'admin'", seededSecretHash); err != nil {
		t.Fatalf("restore the seeded admin: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := NewAuthService(pool, "pool-injection-test-secret", time.Hour, nil)
	NewService(pool, auth, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
		c.Next()
	})

	w := postTokens(t, r, passwordAuthBody(seededDefaultProjectID))
	if w.Code != http.StatusCreated || w.Header().Get("X-Subject-Token") == "" {
		t.Fatalf("POST /v3/auth/tokens through the injected pool: code=%d token=%q body=%s; want 201 and a token",
			w.Code, w.Header().Get("X-Subject-Token"), w.Body.String())
	}

	code, body := doReq(r, http.MethodGet, "/v3/projects/"+seededDefaultProjectID, map[string]string{"X-Test-Roles": "admin"})
	if code != http.StatusOK || !strings.Contains(body, `"name":"default"`) {
		t.Fatalf("GET /v3/projects/%s through the injected pool: code=%d body=%s; want 200 naming project default",
			seededDefaultProjectID, code, body)
	}
}
