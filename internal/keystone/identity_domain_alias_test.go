package keystone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// doJSONReq is doReq's body-carrying sibling — the identity surface tests only ever
// needed GETs and DELETEs, and this file is all about what creation requests accept.
func doJSONReq(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	if rdr != nil {
		req = httptest.NewRequest(method, path, rdr)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// dropProjectByName removes a project the test created, so reruns are not blocked by
// the very uniqueness constraint FR-004 makes visible.
func dropProjectByName(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		// context.Background(), not t.Context(): the test context is cancelled just
		// before Cleanup-registered functions run (testing.T.Context).
		if _, err := testPool.Exec(context.Background(), `DELETE FROM projects WHERE name = $1`, name); err != nil {
			t.Logf("cleanup project %q: %v", name, err)
		}
	})
}

func defaultDomainIDForTest(t *testing.T) string {
	t.Helper()
	id, outcome := resolveDomainRef(t.Context(), testPool, "", enabledDomainsOnly)
	if outcome != domainResolved {
		t.Fatalf("cannot resolve the default domain: outcome=%v", outcome)
	}
	return id
}

// TestCreateProjectDomainReference is the core of B5/P1 (FR-001, FR-002, FR-003,
// FR-007): every way a client may name the default domain must work, an unknown or
// disabled domain must be a client error naming the field, and the response must keep
// reporting the stored UUID rather than the alias.
func TestCreateProjectDomainReference(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	wantDomain := defaultDomainIDForTest(t)
	disabled := insertTestDomain(t, "spec022-proj-disabled", false)

	tests := []struct {
		name       string
		domainPart string
		wantCode   int
		wantDomain string // checked only on 201
	}{
		{"alias", `,"domain_id":"default"`, http.StatusCreated, wantDomain},
		{"omitted", ``, http.StatusCreated, wantDomain},
		{"empty string", `,"domain_id":""`, http.StatusCreated, wantDomain},
		{"stored uuid", fmt.Sprintf(`,"domain_id":%q`, wantDomain), http.StatusCreated, wantDomain},
		{"unknown uuid", `,"domain_id":"11111111-1111-1111-1111-111111111111"`, http.StatusBadRequest, ""},
		{"disabled domain", fmt.Sprintf(`,"domain_id":%q`, disabled), http.StatusBadRequest, ""},
		{"non-identifier", `,"domain_id":"not-a-domain"`, http.StatusBadRequest, ""},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("spec022-proj-%d", i)
			dropProjectByName(t, name)
			body := fmt.Sprintf(`{"project":{"name":%q%s}}`, name, tc.domainPart)
			code, resp := doJSONReq(r, http.MethodPost, "/v3/projects", body, adminHdr)
			if code != tc.wantCode {
				t.Fatalf("POST /v3/projects (%s): code=%d body=%s; want %d", tc.name, code, resp, tc.wantCode)
			}
			if tc.wantCode == http.StatusBadRequest {
				// FR-002: the message must name the domain field, so an operator can
				// tell which input was wrong rather than guessing.
				if !strings.Contains(strings.ToLower(resp), "domain") {
					t.Fatalf("POST /v3/projects (%s): 400 body %s does not name the domain field", tc.name, resp)
				}
				assertNoProject(t, name)
				return
			}
			var got struct {
				Project struct {
					DomainID string `json:"domain_id"`
				} `json:"project"`
			}
			if err := json.Unmarshal([]byte(resp), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", resp, err)
			}
			// FR-007 / A2: the alias is accepted on input, never echoed back.
			if got.Project.DomainID != tc.wantDomain {
				t.Fatalf("POST /v3/projects (%s): domain_id=%q, want the stored uuid %q", tc.name, got.Project.DomainID, tc.wantDomain)
			}
		})
	}
}

// TestCreateProjectDuplicateNameIsConflict — FR-004. projects.name is UNIQUE, and the
// violation surfaces as a generic 500 today. CreateUser already reports the same
// collision as a 409, so this only brings projects into line.
func TestCreateProjectDuplicateNameIsConflict(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	name := "spec022-dup"
	dropProjectByName(t, name)
	body := fmt.Sprintf(`{"project":{"name":%q,"domain_id":"default"}}`, name)

	if code, resp := doJSONReq(r, http.MethodPost, "/v3/projects", body, adminHdr); code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s; want 201", code, resp)
	}
	code, resp := doJSONReq(r, http.MethodPost, "/v3/projects", body, adminHdr)
	if code != http.StatusConflict {
		t.Fatalf("duplicate create: code=%d body=%s; want 409", code, resp)
	}
	if !strings.Contains(resp, name) {
		t.Fatalf("duplicate create: 409 body %s does not name the duplicate %q", resp, name)
	}
}

// TestCreateProjectNoServerFaults — SC-002. The criterion was narrowed from an
// unbounded "any well-formed request" claim to exactly this enumerated set, so the
// test and the success criterion say the same thing.
func TestCreateProjectNoServerFaults(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	disabled := insertTestDomain(t, "spec022-nofault-disabled", false)
	taken := "spec022-nofault-taken"
	dropProjectByName(t, taken)
	if code, resp := doJSONReq(r, http.MethodPost, "/v3/projects",
		fmt.Sprintf(`{"project":{"name":%q,"domain_id":"default"}}`, taken), adminHdr); code != http.StatusCreated {
		t.Fatalf("seeding the taken name: code=%d body=%s; want 201", code, resp)
	}

	payloads := map[string]string{
		"unknown domain":         `{"project":{"name":"spec022-nf-1","domain_id":"11111111-1111-1111-1111-111111111111"}}`,
		"disabled domain":        fmt.Sprintf(`{"project":{"name":"spec022-nf-2","domain_id":%q}}`, disabled),
		"non-identifier domain":  `{"project":{"name":"spec022-nf-3","domain_id":"not-a-domain"}}`,
		"duplicate name":         fmt.Sprintf(`{"project":{"name":%q,"domain_id":"default"}}`, taken),
		"missing required name":  `{"project":{"domain_id":"default"}}`,
		"malformed json":         `{"project":{`,
		"enabled has wrong type": `{"project":{"name":"spec022-nf-4","enabled":"yes"}}`,
	}
	for name, body := range payloads {
		t.Run(name, func(t *testing.T) {
			code, resp := doJSONReq(r, http.MethodPost, "/v3/projects", body, adminHdr)
			if code >= 500 {
				t.Fatalf("POST /v3/projects (%s): code=%d body=%s; a rejected payload must never be a server fault", name, code, resp)
			}
			if code < 400 {
				t.Fatalf("POST /v3/projects (%s): code=%d body=%s; want a 4xx", name, code, resp)
			}
		})
	}
}

// TestCreateUserAcceptsDomainAlias — FR-005. The alias is the only change; the admin
// gate must still precede any domain work, and the existing 400 message text for a
// genuinely unknown domain is preserved so nothing matching on it breaks.
func TestCreateUserAcceptsDomainAlias(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	cleanupUser := func(name string) {
		t.Cleanup(func() {
			if _, err := testPool.Exec(context.Background(), `DELETE FROM users WHERE name = $1`, name); err != nil {
				t.Logf("cleanup user %q: %v", name, err)
			}
		})
	}

	cleanupUser("spec022-user-alias")
	code, resp := doJSONReq(r, http.MethodPost, "/v3/users",
		`{"user":{"name":"spec022-user-alias","password":"pw","domain_id":"default"}}`, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("POST /v3/users with the alias: code=%d body=%s; want 201", code, resp)
	}

	// The admin gate runs first: a non-admin must get 403, not a domain error, so the
	// alias work cannot have moved authorization later in the handler.
	code, resp = doJSONReq(r, http.MethodPost, "/v3/users",
		`{"user":{"name":"spec022-user-nonadmin","password":"pw","domain_id":"default"}}`,
		map[string]string{"X-Test-Roles": "member"})
	if code != http.StatusForbidden {
		t.Fatalf("POST /v3/users as non-admin: code=%d body=%s; want 403 (authz precedes domain resolution)", code, resp)
	}

	code, resp = doJSONReq(r, http.MethodPost, "/v3/users",
		`{"user":{"name":"spec022-user-baddomain","password":"pw","domain_id":"11111111-1111-1111-1111-111111111111"}}`, adminHdr)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /v3/users with an unknown domain: code=%d body=%s; want 400", code, resp)
	}
	if !strings.Contains(resp, "specified domain not found or disabled") {
		t.Fatalf("POST /v3/users with an unknown domain: body %s lost the existing message text", resp)
	}
}

// TestCreateUserDuplicateNameIsConflict: a second user with the same name answers 409,
// not 500. The duplicate used to be detected by PostgreSQL's error text, which SQLite
// does not produce; it now goes through database.IsUniqueViolation, so both backends 409.
func TestCreateUserDuplicateNameIsConflict(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM users WHERE name = $1`, "spec028-dup"); err != nil {
			t.Logf("cleanup user: %v", err)
		}
	})

	body := `{"user":{"name":"spec028-dup","password":"pw","domain_id":"default"}}`
	if code, resp := doJSONReq(r, http.MethodPost, "/v3/users", body, adminHdr); code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s; want 201", code, resp)
	}
	if code, resp := doJSONReq(r, http.MethodPost, "/v3/users", body, adminHdr); code != http.StatusConflict {
		t.Fatalf("duplicate create: code=%d body=%s; want 409", code, resp)
	}
}

// TestCreateGroupAcceptsDomainAlias — FR-006. CreateGroup used to store the reference
// verbatim (groups.domain_id was a VARCHAR with no foreign key until migration 072), so
// `default` was kept as the string and any value was accepted. It now resolves like the
// other creation paths.
func TestCreateGroupAcceptsDomainAlias(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	wantDomain := defaultDomainIDForTest(t)
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM groups WHERE name LIKE 'spec022-group-%'`); err != nil {
			t.Logf("cleanup groups: %v", err)
		}
	})

	code, resp := doJSONReq(r, http.MethodPost, "/v3/groups",
		`{"group":{"name":"spec022-group-alias","domain_id":"default"}}`, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("POST /v3/groups with the alias: code=%d body=%s; want 201", code, resp)
	}
	if !strings.Contains(resp, wantDomain) {
		t.Fatalf("POST /v3/groups: body %s does not report the stored domain uuid %s", resp, wantDomain)
	}

	code, resp = doJSONReq(r, http.MethodPost, "/v3/groups",
		`{"group":{"name":"spec022-group-bad","domain_id":"11111111-1111-1111-1111-111111111111"}}`, adminHdr)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /v3/groups with an unknown domain: code=%d body=%s; want 400", code, resp)
	}
}

// TestCreateGroupDuplicateNameIsConflict — the group half of FR-004. groups carries
// UNIQUE(name, domain_id) (migration 010) and the violation surfaced as a generic 500,
// so a duplicate group name answered differently from a duplicate project name in the
// same service. Found in the SPEC-022 real-mode run; closed here rather than split out,
// since the constant and the pattern were already in the package.
func TestCreateGroupDuplicateNameIsConflict(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	name := "spec022-group-dup"
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM groups WHERE name = $1`, name); err != nil {
			t.Logf("cleanup group %s: %v", name, err)
		}
	})
	body := fmt.Sprintf(`{"group":{"name":%q,"domain_id":"default"}}`, name)

	if code, resp := doJSONReq(r, http.MethodPost, "/v3/groups", body, adminHdr); code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s; want 201", code, resp)
	}
	code, resp := doJSONReq(r, http.MethodPost, "/v3/groups", body, adminHdr)
	if code != http.StatusConflict {
		t.Fatalf("duplicate create: code=%d body=%s; want 409", code, resp)
	}
	if !strings.Contains(resp, name) {
		t.Fatalf("duplicate create: 409 body %s does not name the duplicate %q", resp, name)
	}
}

// TestDomainLookupFailureIsServerError — FR-002, from the caller's side. When the domain
// lookup itself fails, each of the resolver's four callers answers 500, never the
// 400/404 it gives a domain that does not exist: an outage must not tell the client
// its request was wrong. No database needed — the service's pool cannot connect. The
// message is pinned too, and is the same for all four: the resolver logs the real
// error, so the body only has to say what failed — not which domain, since the
// failure is the same whether the caller named the alias or an id.
func TestDomainLookupFailureIsServerError(t *testing.T) {
	const wantMsg = "failed to resolve domain"
	r := newIdentityTestRouterOn(unreachablePool(t))
	for _, tc := range []struct{ name, method, path, body string }{
		{"create project", http.MethodPost, "/v3/projects", `{"project":{"name":"spec022-qf","domain_id":"default"}}`},
		{"create user", http.MethodPost, "/v3/users", `{"user":{"name":"spec022-qf","domain_id":"default"}}`},
		{"create group", http.MethodPost, "/v3/groups", `{"group":{"name":"spec022-qf","domain_id":"default"}}`},
		{"get domain", http.MethodGet, "/v3/domains/default", ""},
	} {
		code, resp := doJSONReq(r, tc.method, tc.path, tc.body, adminHdr)
		if code != http.StatusInternalServerError {
			t.Errorf("%s with the database unreachable: code=%d body=%s; want 500", tc.name, code, resp)
			continue
		}
		if !strings.Contains(resp, `"message":"`+wantMsg+`"`) {
			t.Errorf("%s with the database unreachable: body=%s; want message %q", tc.name, resp, wantMsg)
		}
	}
}

// TestGetDomainByAlias — FR-001a. Standard clients resolve a --domain argument by
// retrieving the domain first, so the alias has to work here too; and every other
// non-identifier value must still get #515's verbatim 404, now from the handler
// rather than the middleware.
func TestGetDomainByAlias(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	wantDomain := defaultDomainIDForTest(t)

	code, resp := doReq(r, http.MethodGet, "/v3/domains/default", adminHdr)
	if code != http.StatusOK {
		t.Fatalf("GET /v3/domains/default: code=%d body=%s; want 200", code, resp)
	}
	if !strings.Contains(resp, wantDomain) {
		t.Fatalf("GET /v3/domains/default: body %s is not the default domain %s", resp, wantDomain)
	}

	code, resp = doReq(r, http.MethodGet, "/v3/domains/"+wantDomain, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("GET /v3/domains/<uuid>: code=%d body=%s; want 200", code, resp)
	}

	// A *disabled* domain must still be retrievable. Retrieval and creation ask
	// different questions: "does this domain exist" versus "is this a valid domain to
	// put a new project in". GetDomain's query has never filtered on enabled, and its
	// response carries the flag, so resolving the alias must not silently start
	// hiding disabled domains behind a 404.
	disabled := insertTestDomain(t, "spec022-get-disabled", false)
	code, resp = doReq(r, http.MethodGet, "/v3/domains/"+disabled, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("GET /v3/domains/<disabled uuid>: code=%d body=%s; want 200 — retrieval is not a creation-target check", code, resp)
	}
	if !strings.Contains(resp, `"enabled":false`) {
		t.Fatalf("GET /v3/domains/<disabled uuid>: body %s should report enabled:false", resp)
	}

	// #515's contract holds for everything except the one alias.
	for _, id := range []string{"not-a-uuid", "DEFAULT", "11111111-1111-1111-1111-111111111111"} {
		code, resp = doReq(r, http.MethodGet, "/v3/domains/"+id, adminHdr)
		if code != http.StatusNotFound {
			t.Fatalf("GET /v3/domains/%s: code=%d body=%s; want 404", id, code, resp)
		}
		if want := fmt.Sprintf("Could not find domain: %s.", id); !strings.Contains(resp, want) {
			t.Fatalf("GET /v3/domains/%s: body %s lost the Keystone-verbatim message %q", id, resp, want)
		}
	}
}

// TestListDomainsNameFilter — FR-001a. The handler issues an unfiltered SELECT today
// and ignores every query parameter, so a client's name lookup silently receives all
// domains instead of the one it asked for.
func TestListDomainsNameFilter(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	r := newIdentityTestRouter()
	insertTestDomain(t, "Spec022Filter", true)

	count := func(path string) int {
		t.Helper()
		code, resp := doReq(r, http.MethodGet, path, adminHdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", path, code, resp)
		}
		var got struct {
			Domains []json.RawMessage `json:"domains"`
		}
		if err := json.Unmarshal([]byte(resp), &got); err != nil {
			t.Fatalf("GET %s: unmarshal %s: %v", path, resp, err)
		}
		return len(got.Domains)
	}

	all := count("/v3/domains")
	if all < 2 {
		t.Fatalf("expected at least the default domain and the fixture, got %d", all)
	}
	if n := count("/v3/domains?name=Spec022Filter"); n != 1 {
		t.Fatalf("GET /v3/domains?name=Spec022Filter returned %d domains, want 1", n)
	}
	if n := count("/v3/domains?name=Default"); n != 1 {
		t.Fatalf("GET /v3/domains?name=Default returned %d domains, want 1", n)
	}
	// Names stay case-sensitive, as upstream treats them (FR-001a).
	if n := count("/v3/domains?name=spec022filter"); n != 0 {
		t.Fatalf("GET /v3/domains?name=spec022filter returned %d domains, want 0 (names are case-sensitive)", n)
	}
	// The discriminator that proves filtering happens at all: a name that cannot
	// exist must return nothing, not everything.
	if n := count("/v3/domains?name=spec022-no-such-domain"); n != 0 {
		t.Fatalf("GET /v3/domains?name=<nonexistent> returned %d domains, want 0 — the filter is being ignored", n)
	}
	// Unfiltered still means unfiltered.
	if n := count("/v3/domains"); n != all {
		t.Fatalf("GET /v3/domains returned %d domains, want %d", n, all)
	}
}

func assertNoProject(t *testing.T, name string) {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM projects WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("counting projects named %q: %v", name, err)
	}
	if n != 0 {
		t.Fatalf("a refused request left %d project(s) named %q behind", n, name)
	}
}
