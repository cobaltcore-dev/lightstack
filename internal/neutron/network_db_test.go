package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the services the tests construct (SPEC-027).
var testPool database.DB

func TestMain(m *testing.M) {
	// Skip DB-backed tests only when no test DB is configured. A configured database
	// that cannot be opened or migrated fails the run instead (dbtest.Open), so a broken
	// test database in CI fails the job rather than quietly hiding the DB-backed tests.
	testPool, testDBAvailable = dbtest.Open("../../migrations")
	code := m.Run()
	dbtest.Close(testPool)
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed neutron tests")
	}
}

const seededProjectID = "00000000-0000-0000-0000-000000000002"

func newNeutronTestRouter() *gin.Engine {
	r, _ := newNeutronTestRouterWithService()
	return r
}

// newNeutronTestRouterWithService also hands back the Service the routes are bound to, for the tests
// that assert on state the handlers keep outside the response — DHCP bookkeeping, for instance.
func newNeutronTestRouterWithService() (*gin.Engine, *Service) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Mirror production: the envelope marker goes on the engine, first, so error bodies in these
	// tests are the ones a client actually receives (SPEC-024 US2). See error_envelope_test.go for
	// a router that also wires NoRoute/NoMethod, which this one deliberately does not.
	r.Use(ErrorEnvelope())
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc := NewService(testPool, "stub", nil)
	svc.RegisterRoutes(&r.RouterGroup)
	return r, svc
}

func doReq(r *gin.Engine, method, path string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func networksFromBody(t *testing.T, body string) []map[string]any {
	t.Helper()
	var resp struct {
		Networks []map[string]any `json:"networks"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal networks: %v (body=%s)", err, body)
	}
	return resp.Networks
}

func hasNet(nets []map[string]any, name string) bool {
	for _, n := range nets {
		if n["name"] == name {
			return true
		}
	}
	return false
}

// TestListNetworksFilters_DB exercises the F4 wiring against Postgres: with one
// project-owned non-shared network and one shared network, the `shared` filter must
// return disjoint sets, and every network must carry `router:external`. Catches a
// regression in the query the pure filter test cannot (e.g. the WHERE never reaching
// the DB, or the attribute dropped from the row).
//
// The router:external half was rewritten by SPEC-024 F11: it asserted an empty result,
// which was the defect, and now asserts the two filters are complements.
func TestListNetworksFilters_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	ownID, shdID := uuid.NewString(), uuid.NewString()
	ownName := "dbnet-own-" + ownID[:8]
	shdName := "dbnet-shared-" + shdID[:8]
	if _, err := testPool.Exec(ctx, `INSERT INTO networks (id, name, project_id, shared) VALUES ($1,$2,$3,false)`, ownID, ownName, seededProjectID); err != nil {
		t.Fatalf("insert own net: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO networks (id, name, project_id, shared) VALUES ($1,$2,$3,true)`, shdID, shdName, seededProjectID); err != nil {
		t.Fatalf("insert shared net: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM networks WHERE id = ANY($1)", []string{ownID, shdID}) })

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	// shared=true → the shared net, not the owned non-shared one.
	_, body := doReq(r, http.MethodGet, "/v2.0/networks?shared=true", hdr)
	nets := networksFromBody(t, body)
	if !hasNet(nets, shdName) || hasNet(nets, ownName) {
		t.Errorf("shared=true returned own=%v shared=%v; want shared only", hasNet(nets, ownName), hasNet(nets, shdName))
	}
	for _, n := range nets {
		if _, ok := n["router:external"]; !ok {
			t.Errorf("network %v is missing the router:external key", n["name"])
		}
	}

	// shared=false → the owned net, not the shared one.
	_, body = doReq(r, http.MethodGet, "/v2.0/networks?shared=false", hdr)
	nets = networksFromBody(t, body)
	if !hasNet(nets, ownName) || hasNet(nets, shdName) {
		t.Errorf("shared=false returned own=%v shared=%v; want own only", hasNet(nets, ownName), hasNet(nets, shdName))
	}

	// SPEC-024 F11: router:external is a real filter now. It used to force an empty result because
	// Lightstack modelled no external networks — the defect that left Horizon's Set Gateway dropdown empty.
	// Asserting a global count of 0 was also fragile: it depended on no other test or fixture ever
	// creating one. The property that actually matters is that the two filters are complements over
	// networks this test controls.
	extName := "spec024-db-ext-" + uuid.New().String()[:8]
	if _, err := testPool.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, external, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, true, 'ACTIVE', false, true, 'flat', 1500, now(), now())
	`, uuid.New().String(), extName, seededProjectID); err != nil {
		t.Fatalf("seed external network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE name = $1", extName) //nolint:errcheck // best-effort cleanup
	})

	_, body = doReq(r, http.MethodGet, "/v2.0/networks?router:external=true", hdr)
	nets = networksFromBody(t, body)
	if !hasNet(nets, extName) {
		t.Error("router:external=true did not return the external network — F11's empty dropdown")
	}
	if hasNet(nets, ownName) {
		t.Error("router:external=true returned a non-external network")
	}

	_, body = doReq(r, http.MethodGet, "/v2.0/networks?router:external=false", hdr)
	nets = networksFromBody(t, body)
	if hasNet(nets, extName) {
		t.Error("the two router:external filters are not complements")
	}
}
