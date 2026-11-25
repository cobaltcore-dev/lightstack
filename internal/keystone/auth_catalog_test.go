package keystone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
)

func setSchemeEnv(t *testing.T, scheme string) {
	t.Helper()
	for _, k := range []string{"LIGHTSTACK_ENDPOINT_SCHEME", "LIGHTSTACK_ENDPOINT_HOST"} {
		v, had := os.LookupEnv(k)
		k := k
		t.Cleanup(func() {
			if had {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	os.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.example.com")
	if scheme == "" {
		os.Unsetenv("LIGHTSTACK_ENDPOINT_SCHEME")
	} else {
		os.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", scheme)
	}
}

// TestHardcodedCatalogScheme: the fallback catalog honors the scheme on every
// URL, and the structure is identical between http and https except the scheme
// (FR-010/011/013, SC-006). No DB required.
func TestHardcodedCatalogScheme(t *testing.T) {
	setSchemeEnv(t, "")
	httpCat := buildHardcodedCatalog("proj1", catalogHost(""), "")
	setSchemeEnv(t, "https")
	httpsCat := buildHardcodedCatalog("proj1", catalogHost(""), "")

	for _, e := range httpsCat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "https://") {
				t.Errorf("https config: %s/%s URL not https: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
	for _, e := range httpCat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "http://") {
				t.Errorf("default config: %s/%s URL not http: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}

	// Structure identical except the URL scheme.
	if len(httpCat) != len(httpsCat) {
		t.Fatalf("service count differs: http=%d https=%d", len(httpCat), len(httpsCat))
	}
	for i := range httpCat {
		if httpCat[i].Type != httpsCat[i].Type || httpCat[i].Name != httpsCat[i].Name {
			t.Fatalf("service[%d] differs: %+v vs %+v", i, httpCat[i], httpsCat[i])
		}
		if len(httpCat[i].Endpoints) != len(httpsCat[i].Endpoints) {
			t.Fatalf("endpoint count differs for %s", httpCat[i].Type)
		}
		for j := range httpCat[i].Endpoints {
			a, b := httpCat[i].Endpoints[j], httpsCat[i].Endpoints[j]
			if a.Interface != b.Interface || a.Region != b.Region {
				t.Errorf("endpoint meta differs: %+v vs %+v", a, b)
			}
			// URLs must match once the scheme is normalized away.
			if common.ForceScheme(a.URL, "http") != common.ForceScheme(b.URL, "http") {
				t.Errorf("endpoint URL differs beyond scheme: %s vs %s", a.URL, b.URL)
			}
		}
	}
}

// TestBuildServiceCatalogScheme: the DB-backed catalog applies the scheme to
// every advertised endpoint URL (FR-010). DB required.
func TestBuildServiceCatalogScheme(t *testing.T) {
	requireDB(t)
	setSchemeEnv(t, "https")
	cat := BuildServiceCatalog(testPool, "proj1", "", "")
	if len(cat) == 0 {
		t.Fatal("empty catalog")
	}
	for _, e := range cat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "https://") {
				t.Errorf("%s/%s URL not https: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
}

// TestBuildServiceCatalogS3EndpointFromConfig: the DB-backed catalog must advertise
// the resolved RGW endpoint (cfg.Glance.S3Endpoint, wired via SetGlanceS3Endpoint —
// the Helm chart populates it from YAML, NOT the env), never migration 062's seeded
// http://localhost:7480 placeholder (PR #73 r3558646680). DB required — the harness
// migrates 062, which seeds the s3 service+endpoint.
func TestBuildServiceCatalogS3EndpointFromConfig(t *testing.T) {
	requireDB(t)
	const rgw = "http://rgw.example.test:8080"
	t.Setenv("LIGHTSTACK_GLANCE_S3_ENDPOINT", "") // Helm-style: value comes from config, not env

	var got string
	for _, e := range BuildServiceCatalog(testPool, "proj-s3", "", rgw) { // rgw = cfg value
		if e.Type != "s3" {
			continue
		}
		for _, ep := range e.Endpoints {
			if ep.Interface == "public" {
				got = ep.URL
			}
		}
	}
	if got != rgw {
		t.Fatalf("s3 catalog endpoint = %q, want %q (seeded localhost:7480 must be overridden by the config value)", got, rgw)
	}
}

// TestBuildServiceCatalogOmitsS3WhenUnconfigured: with RGW unconfigured, the
// DB-backed catalog must omit the `s3` service entirely — migration 062's seeded
// placeholder (http://localhost:7480) must never reach a client, on any service
// (PR #73 review r3710751586). DB required: the harness migrates 062, which seeds it.
func TestBuildServiceCatalogOmitsS3WhenUnconfigured(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_GLANCE_S3_ENDPOINT", "") // unconfigured deployment

	cat := BuildServiceCatalog(testPool, "proj-unconf", "", "") // "" = cfg.Glance.S3Endpoint unset
	if len(cat) == 0 {
		t.Fatal("empty catalog")
	}
	for _, e := range cat {
		if e.Type == "s3" {
			t.Errorf("unconfigured: catalog must omit the s3 service, got %+v", e)
		}
		for _, ep := range e.Endpoints {
			if strings.Contains(ep.URL, "localhost:7480") {
				t.Errorf("%s/%s advertises migration 062's placeholder: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
}

// TestHardcodedCatalogS3FromConfig: the fallback catalog advertises `s3` only when
// the resolved config endpoint is set, and with that URL (no DB, no env).
func TestHardcodedCatalogS3FromConfig(t *testing.T) {
	s3URL := func(cat []CatalogEntry) (string, bool) {
		for _, e := range cat {
			if e.Type == "s3" && len(e.Endpoints) > 0 {
				return e.Endpoints[0].URL, true
			}
		}
		return "", false
	}

	if url, ok := s3URL(buildHardcodedCatalog("p", catalogHost(""), "http://rgw.example.test:8080")); !ok || url != "http://rgw.example.test:8080" {
		t.Fatalf("configured: fallback s3 endpoint = %q ok=%v; want the RGW URL", url, ok)
	}
	if _, ok := s3URL(buildHardcodedCatalog("p", catalogHost(""), "")); ok {
		t.Fatal("unconfigured: fallback must not advertise an s3 endpoint")
	}
}

// TestSubstituteURLTemplatesRewritesLocalhost: with a configured endpoint host,
// substitution rewrites a localhost-seeded URL (the cinder `volume` alias …015,
// migration 054) to that host — not just the `http://lightstack:` prefix (F5, FR-006/007).
// No DB required — substituteURLTemplates is pure.
func TestSubstituteURLTemplatesRewritesLocalhost(t *testing.T) {
	setSchemeEnv(t, "") // LIGHTSTACK_ENDPOINT_HOST=lightstack.example.com, scheme unset (http)

	got := substituteURLTemplates("http://localhost:8776/v3", "proj1", catalogHost(""))
	if strings.Contains(got, "localhost") {
		t.Errorf("localhost not rewritten: %q", got)
	}
	if !strings.Contains(got, "lightstack.example.com:8776") {
		t.Errorf("expected configured host, got %q", got)
	}

	// The existing http://lightstack: rewrite still works (no regression).
	if got := substituteURLTemplates("http://lightstack:9696", "p", catalogHost("")); !strings.Contains(got, "lightstack.example.com:9696") {
		t.Errorf("lightstack prefix rewrite regressed: %q", got)
	}
}

// TestSubstituteURLTemplatesLocalhostUnsetNoop: with no endpoint host configured
// (dev/CI default), a localhost URL stays localhost — the fix must not break the
// local case (F5, FR edge case).
func TestSubstituteURLTemplatesLocalhostUnsetNoop(t *testing.T) {
	for _, k := range []string{"LIGHTSTACK_ENDPOINT_HOST", "LIGHTSTACK_ENDPOINT_SCHEME"} {
		v, had := os.LookupEnv(k)
		k := k
		t.Cleanup(func() {
			if had {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		})
		os.Unsetenv(k)
	}

	got := substituteURLTemplates("http://localhost:8776/v3", "p", catalogHost(""))
	if !strings.Contains(got, "localhost:8776") {
		t.Errorf("localhost must be preserved when host unset, got %q", got)
	}
}

// catalogServices lists a catalog's services as sorted "type/name" pairs.
func catalogServices(cat []CatalogEntry) []string {
	out := make([]string, 0, len(cat))
	for _, e := range cat {
		out = append(out, e.Type+"/"+e.Name)
	}
	slices.Sort(out)
	return out
}

// Placement is served on :8778 and the chart's notes send clients to the catalog for it,
// but no migration seeded it, so only the fallback catalog listed it. Horizon's
// Hypervisors → Resource Provider tab looks it up there and showed "Unable to retrieve
// providers information." without it.
func TestBuildServiceCatalogListsPlacement(t *testing.T) {
	requireDB(t)
	setSchemeEnv(t, "")
	for _, e := range BuildServiceCatalog(testPool, "proj-placement", "", "") {
		if e.Type != "placement" {
			continue
		}
		if len(e.Endpoints) != 1 || e.Endpoints[0].URL != "http://lightstack.example.com:8778" {
			t.Errorf("placement endpoints = %+v; want one, at http://lightstack.example.com:8778", e.Endpoints)
		}
		return
	}
	t.Error("the catalog lists no placement service")
}

// The fallback is what clients get when the catalog table cannot be read, so it must
// advertise the same services. The two had drifted both ways: placement only in the
// fallback, cinder's `volume` alias (migration 054) only in the table.
func TestHardcodedCatalogMatchesTheDatabase(t *testing.T) {
	requireDB(t)
	setSchemeEnv(t, "")
	db := catalogServices(BuildServiceCatalog(testPool, "proj-drift", "", ""))
	fallback := catalogServices(buildHardcodedCatalog("proj-drift", catalogHost(""), ""))
	if !slices.Equal(db, fallback) {
		t.Errorf("the database catalog lists %v, the fallback %v; they must list the same services", db, fallback)
	}
}

// partialReadDB answers the catalog query with one good row and then an iteration error: a read
// that fails part-way. unreachableDB fails at Query instead, which reaches a different fallback,
// and a read that yields no rows would fall back as an empty catalog whatever rows.Err said.
type partialReadDB struct{ unreachableDB }

func (partialReadDB) Query(context.Context, string, ...any) (database.Rows, error) {
	return &partialRows{}, nil
}

type partialRows struct{ served bool }

func (r *partialRows) Next() bool {
	more := !r.served
	r.served = true
	return more
}

func (r *partialRows) Scan(dest ...any) error {
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*string) = "partial-svc", "partial-read-probe", "partial"
	for i, v := range []string{"partial-ep", "public", "http://lightstack:1/partial", "RegionOne"} {
		*dest[3+i].(**string) = &v
	}
	return nil
}

func (*partialRows) Err() error { return errUnreachable }
func (*partialRows) Close()     {}

// A read that fails part-way must not become a token's catalog: it would hand the token a catalog
// with endpoints missing. The complete hardcoded catalog is served instead.
func TestBuildServiceCatalogDiscardsAPartialRead(t *testing.T) {
	setSchemeEnv(t, "")
	got := catalogServices(BuildServiceCatalog(partialReadDB{}, "p", "", ""))
	want := catalogServices(buildHardcodedCatalog("p", catalogHost(""), ""))
	if !slices.Equal(got, want) {
		t.Errorf("a read that failed part-way served %v; want the complete hardcoded catalog %v", got, want)
	}
}

// catalogAdminHeaders are the stub-middleware headers that make a management call an admin's.
var catalogAdminHeaders = map[string]string{"X-Test-Roles": "admin", "X-Test-User": rgwCompatSeededAdminID}

// createdID posts body to path as an admin, asserts 201 and returns <key>.id.
func createdID(t *testing.T, r *gin.Engine, path, key, body string) string {
	t.Helper()
	w := postJSON(t, r, http.MethodPost, path, body, catalogAdminHeaders)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
	var got map[string]struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got[key].ID == "" {
		t.Fatalf("POST %s: no %s.id in %s", path, key, w.Body.String())
	}
	return got[key].ID
}

// createProbeEndpoint creates a service of a type nothing else uses, and one public endpoint for
// it, through the Identity API. Both rows are removed on cleanup, the endpoint first (cleanups run
// last-in first-out), so a failing run leaves nothing for the parallel packages that read the
// seeded catalog.
func createProbeEndpoint(t *testing.T, r *gin.Engine, svcType, url string) (serviceID, endpointID string) {
	t.Helper()
	serviceID = createdID(t, r, "/v3/services", "service",
		fmt.Sprintf(`{"service":{"type":%q,"name":%q}}`, svcType, svcType))
	cleanupIdentityRow(t, "services", serviceID)
	endpointID = createdID(t, r, "/v3/endpoints", "endpoint",
		fmt.Sprintf(`{"endpoint":{"service_id":%q,"interface":"public","region":"RegionOne","url":%q}}`, serviceID, url))
	cleanupIdentityRow(t, "endpoints", endpointID)
	return serviceID, endpointID
}

// tokenCatalog is a token's catalog as tokenResponseBody and validationResponseBody decode it.
type tokenCatalog []struct {
	Type      string `json:"type"`
	Endpoints []struct {
		URL string `json:"url"`
	} `json:"endpoints"`
}

// catalogURLs lists the endpoint URLs a token catalog holds for one service type.
func catalogURLs(cat tokenCatalog, svcType string) []string {
	var out []string
	for _, e := range cat {
		if e.Type == svcType {
			for _, ep := range e.Endpoints {
				out = append(out, ep.URL)
			}
		}
	}
	return out
}

// The catalog freshness tests (SPEC-034 FR-001 to FR-004, contract C1, C2, C4) each issue a token
// first and then change one thing: a catalog kept between requests would serve the first token's
// catalog to the second. Before SPEC-034 a Redis-backed copy did exactly that.

// catalogFreshnessEnv seeds the project and admin the tests authenticate as, and pins the
// advertised scheme (empty is http) and host (httptest addresses example.com).
func catalogFreshnessEnv(t *testing.T) {
	t.Helper()
	seedRGWCompatFixtures(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "")
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "example.com")
}

// issueCatalogToken issues a password token for the fixture project and returns it with its body.
func issueCatalogToken(t *testing.T, r *gin.Engine) (token string, body tokenResponseBody) {
	t.Helper()
	w := postTokens(t, r, passwordAuthBody(rgwCompatProjectID))
	if w.Code != http.StatusCreated {
		t.Fatalf("issue token: %d %s", w.Code, w.Body.String())
	}
	return w.Header().Get("X-Subject-Token"), decodeTokenBody(t, w)
}

// newCatalogProbe names a service type nothing else uses, the endpoint URL to create for it, and
// the URL the catalog must advertise for it.
func newCatalogProbe() (svcType, url, want string) {
	svcType = "catalog-freshness-" + uuid.NewString()[:8]
	return svcType, "http://lightstack:8999/" + svcType, "http://example.com:8999/" + svcType
}

func TestCatalogListsEndpointCreatedAfterIssue(t *testing.T) {
	catalogFreshnessEnv(t)
	r := newRGWCompatRouter(false)
	first, _ := issueCatalogToken(t, r)
	svcType, url, want := newCatalogProbe()
	createProbeEndpoint(t, r, svcType, url)

	if _, body := issueCatalogToken(t, r); !slices.Contains(catalogURLs(body.Token.Catalog, svcType), want) {
		t.Errorf("issued token's catalog lists %v for %s; want %s (stale catalog)",
			catalogURLs(body.Token.Catalog, svcType), svcType, want)
	}
	w := getToken(t, r, first)
	if w.Code != http.StatusOK {
		t.Fatalf("validate token: %d %s", w.Code, w.Body.String())
	}
	if got := catalogURLs(decodeValidationBody(t, w).Token.Catalog, svcType); !slices.Contains(got, want) {
		t.Errorf("validated token's catalog lists %v for %s; want %s (stale catalog)", got, svcType, want)
	}
}

func TestCatalogDropsDeletedEndpoint(t *testing.T) {
	catalogFreshnessEnv(t)
	r := newRGWCompatRouter(false)
	svcType, url, want := newCatalogProbe()
	_, endpointID := createProbeEndpoint(t, r, svcType, url)
	// Issued after the create, so a kept catalog would hold the endpoint the delete removes.
	if _, body := issueCatalogToken(t, r); !slices.Contains(catalogURLs(body.Token.Catalog, svcType), want) {
		t.Fatalf("new endpoint missing from a fresh catalog; the test cannot run")
	}
	if w := postJSON(t, r, http.MethodDelete, "/v3/endpoints/"+endpointID, "", catalogAdminHeaders); w.Code != http.StatusNoContent {
		t.Fatalf("delete endpoint: %d %s", w.Code, w.Body.String())
	}
	_, body := issueCatalogToken(t, r)
	if got := catalogURLs(body.Token.Catalog, svcType); len(got) != 0 {
		t.Errorf("deleted endpoint still in the catalog: %v (stale catalog)", got)
	}
}

// The scheme is read on every call, so changing it here stands in for a restart with
// LIGHTSTACK_ENDPOINT_SCHEME=https.
func TestCatalogFollowsSchemeChange(t *testing.T) {
	catalogFreshnessEnv(t)
	r := newRGWCompatRouter(false)
	issueCatalogToken(t, r)
	t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "https")
	_, body := issueCatalogToken(t, r)
	for _, e := range body.Token.Catalog {
		if e.Type == "s3" {
			continue // the configured RGW URL, served as-is
		}
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "https://") {
				t.Errorf("%s endpoint %s is not https after the scheme change (stale catalog)", e.Type, ep.URL)
			}
		}
	}
}
