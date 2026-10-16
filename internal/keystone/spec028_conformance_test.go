package keystone

// SPEC-028 conformance fixes for the identity surface, found by the CLI
// conformance suites (issue #105).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// doReqBody is doReq with a request body, for the POST paths these tests drive.
func doReqBody(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// spec028Router wires the real routes over the test pool with a real
// AuthService, for the paths (token revoke) that need one.
func spec028Router() (*gin.Engine, *AuthService) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authSvc := NewAuthService(testPool, "spec028-test-secret", time.Hour, nil)
	NewService(testPool, authSvc, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) {
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			c.Set("user_id", uid)
		}
		c.Next()
	})
	return r, authSvc
}

// TestUserShowCarriesEmailAndDescription — the users table has both columns,
// create/set accept both, but the show and list queries selected neither, so
// `openstack user show` printed email: null for a user created with one
// (SPEC-028).
func TestUserShowCarriesEmailAndDescription(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()

	userID := uuid.NewString()
	name := "spec028-email-" + userID[:8]
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO users (id, name, password_hash, domain_id, email, description)
		 VALUES ($1,$2,'x',$3,'spec028@example.com','spec028 description')`,
		userID, name, defaultDomainID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM users WHERE id=$1", userID) })

	code, body := doReq(r, http.MethodGet, "/v3/users/"+userID, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("show user: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"email":"spec028@example.com"`) {
		t.Errorf("show response lacks the persisted email: %s", body)
	}
	if !strings.Contains(body, `"description":"spec028 description"`) {
		t.Errorf("show response lacks the persisted description: %s", body)
	}

	_, body = doReq(r, http.MethodGet, "/v3/users?name="+name, adminHdr)
	if !strings.Contains(body, `"email":"spec028@example.com"`) {
		t.Errorf("list response lacks the persisted email: %s", body)
	}
}

// TestRoleCreateDuplicateIsConflict — roles.name is UNIQUE; a duplicate name
// was surfaced as 500 instead of upstream's 409 (SPEC-028).
func TestRoleCreateDuplicateIsConflict(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()

	name := "spec028-dup-role-" + uuid.NewString()[:8]
	body := `{"role":{"name":"` + name + `"}}`
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM roles WHERE name=$1", name) })

	code, resp := doReqBody(r, http.MethodPost, "/v3/roles", body, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s", code, resp)
	}
	code, resp = doReqBody(r, http.MethodPost, "/v3/roles", body, adminHdr)
	if code != http.StatusConflict {
		t.Errorf("duplicate create: code=%d body=%s; want 409", code, resp)
	}
}

// TestRoleAssignmentIncludeNames — `GET /v3/role_assignments` ignored
// include_names=true and returned bare ids; openstacksdk's consumer dies with
// KeyError 'name', so `role assignment list --names` exited 1 (SPEC-028).
func TestRoleAssignmentIncludeNames(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()
	ctx := context.Background()
	domID := defaultDomainID

	userID, projectID := uuid.NewString(), uuid.NewString()
	roleID := uuid.NewString()
	roleName := "spec028-ra-role-" + roleID[:8]
	for _, ins := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, name, password_hash, domain_id) VALUES ($1,$2,'x',$3)`,
			[]any{userID, "spec028-ra-user-" + userID[:8], domID}},
		{`INSERT INTO projects (id, name, domain_id) VALUES ($1,$2,$3)`,
			[]any{projectID, "spec028-ra-proj-" + projectID[:8], domID}},
		{`INSERT INTO roles (id, name) VALUES ($1,$2)`, []any{roleID, roleName}},
		{`INSERT INTO role_assignments (user_id, project_id, role_id) VALUES ($1,$2,$3)`,
			[]any{userID, projectID, roleID}},
	} {
		if _, err := testPool.Exec(ctx, ins.sql, ins.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM role_assignments WHERE user_id=$1", userID)
		testPool.Exec(ctx, "DELETE FROM roles WHERE id=$1", roleID)
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", projectID)
		testPool.Exec(ctx, "DELETE FROM users WHERE id=$1", userID)
	})

	// Filtered to the seeded assignment so the shared database's other rows
	// cannot make the assertions below flaky.
	code, body := doReq(r, http.MethodGet, "/v3/role_assignments?include_names=true&user.id="+userID, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", code, body)
	}
	var resp struct {
		RoleAssignments []struct {
			User struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Domain struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"domain"`
			} `json:"user"`
			Scope struct {
				Project struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Domain struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"domain"`
				} `json:"project"`
			} `json:"scope"`
			Role struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"role"`
		} `json:"role_assignments"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if len(resp.RoleAssignments) != 1 {
		t.Fatalf("want exactly the seeded assignment, got %d: %s", len(resp.RoleAssignments), body)
	}
	a := resp.RoleAssignments[0]
	if a.User.Name == "" || a.Scope.Project.Name == "" || a.Role.Name != roleName ||
		a.User.Domain.Name != "Default" || a.Scope.Project.Domain.Name != "Default" {
		t.Errorf("include_names did not embed names: user=%+v scope=%+v role=%+v", a.User, a.Scope, a.Role)
	}
}

// TestDomainResponsesCarryLinks — POST and GET /v3/domains omitted `links`;
// the pinned OSC 7 formatter aborts with KeyError 'links' (SPEC-028).
func TestDomainResponsesCarryLinks(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()

	name := "spec028-links-" + uuid.NewString()[:8]
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM domains WHERE name=$1", name) })

	code, body := doReqBody(r, http.MethodPost, "/v3/domains",
		`{"domain":{"name":"`+name+`","description":"spec028 links"}}`, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("create domain: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"links":{"self":`) {
		t.Errorf("create response lacks links.self: %s", body)
	}
	var id string
	if err := testPool.QueryRow(context.Background(),
		"SELECT id::text FROM domains WHERE name=$1", name).Scan(&id); err != nil {
		t.Fatalf("resolve domain: %v", err)
	}

	code, body = doReq(r, http.MethodGet, "/v3/domains/"+id, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("show domain: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"links":{"self":`) {
		t.Errorf("show response lacks links.self: %s", body)
	}

	// The list is how the CLI resolves a domain BY NAME (show and delete fall
	// back to ?name= after the by-id GET 404s), so every item needs links
	// too — the plain list answered without them, and 'domain show <name>'
	// and 'domain delete <name>' aborted with KeyError 'links'.
	code, body = doReq(r, http.MethodGet, "/v3/domains", adminHdr)
	if code != http.StatusOK {
		t.Fatalf("list domains: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"links":{"self":`) {
		t.Errorf("domain list items lack links.self: %s", body)
	}
	code, body = doReq(r, http.MethodGet, "/v3/domains?name="+name, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("list domains by name: code=%d body=%s", code, body)
	}
	var filtered struct {
		Domains []struct {
			Links map[string]string `json:"links"`
		} `json:"domains"`
	}
	if err := json.Unmarshal([]byte(body), &filtered); err != nil {
		t.Fatalf("unmarshal name-filtered list: %v (body=%s)", err, body)
	}
	if len(filtered.Domains) != 1 || filtered.Domains[0].Links["self"] == "" {
		t.Errorf("name-filtered list item lacks links.self: %s", body)
	}
}

// TestApplicationCredentialConformance — three defects in one surface
// (SPEC-028):
//   - a role given by NAME ONLY panicked on a nil type assertion (recovered
//     by gin as 500, after the credential row was already committed);
//   - a duplicate (user_id, name) answered 500 instead of 409;
//   - list/show omitted the `roles` key entirely when the roles query failed
//     — which it always did, because no migration ever created
//     application_credential_roles — and the pinned OSC 7 formatter crashes
//     on the missing key.
func TestApplicationCredentialConformance(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()
	ctx := context.Background()

	userID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO users (id, name, password_hash, domain_id) VALUES ($1,$2,'x',$3)`,
		userID, "spec028-ac-user-"+userID[:8], defaultDomainID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	roleID := uuid.NewString()
	roleName := "spec028-ac-role-" + roleID[:8]
	if _, err := testPool.Exec(ctx, "INSERT INTO roles (id, name) VALUES ($1,$2)", roleID, roleName); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM application_credentials WHERE user_id=$1", userID)
		testPool.Exec(ctx, "DELETE FROM users WHERE id=$1", userID)
		testPool.Exec(ctx, "DELETE FROM roles WHERE id=$1", roleID)
	})

	// Role by name only: 201 (was a panic → 500), with the role resolved to
	// its id and name, and actually persisted.
	code, body := doReqBody(r, http.MethodPost, "/v3/users/"+userID+"/application_credentials",
		`{"application_credential":{"name":"spec028-ac-byname","roles":[{"name":"`+roleName+`"}]}}`, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("create with role by name: code=%d body=%s; want 201 (was a panic/500)", code, body)
	}
	if !strings.Contains(body, `"roles":[{"id":"`+roleID+`","name":"`+roleName+`"}]`) {
		t.Errorf("create response did not resolve the role: %s", body)
	}
	var n int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM application_credential_roles WHERE role_id=$1", roleID).Scan(&n); err != nil {
		t.Fatalf("count role rows: %v", err)
	}
	if n != 1 {
		t.Errorf("role association persisted: count=%d; want 1", n)
	}

	// Duplicate name for the same user: 409 (was 500).
	code, body = doReqBody(r, http.MethodPost, "/v3/users/"+userID+"/application_credentials",
		`{"application_credential":{"name":"spec028-ac-byname"}}`, adminHdr)
	if code != http.StatusConflict {
		t.Errorf("duplicate create: code=%d body=%s; want 409", code, body)
	}

	// List and show always carry roles (empty array when none).
	code, body = doReq(r, http.MethodGet, "/v3/users/"+userID+"/application_credentials", adminHdr)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"roles":[`) {
		t.Errorf("list response lacks the roles key: %s", body)
	}
	var credID string
	if err := testPool.QueryRow(ctx,
		"SELECT id::text FROM application_credentials WHERE user_id=$1 AND name='spec028-ac-byname'", userID).
		Scan(&credID); err != nil {
		t.Fatalf("resolve credential: %v", err)
	}
	code, body = doReq(r, http.MethodGet, "/v3/users/"+userID+"/application_credentials/"+credID, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"roles":[{"id":"`+roleID+`","name":"`+roleName+`"}]`) {
		t.Errorf("show response must carry the persisted role: %s", body)
	}
}

// TestTokenRevokeUnknownToken — DELETE /v3/auth/tokens answered 204
// unconditionally; upstream validates the subject token and answers 404 when
// it names nothing revocable, so typos and expired ids were invisible to
// clients (SPEC-028).
func TestTokenRevokeUnknownToken(t *testing.T) {
	requireDB(t)
	r, authSvc := spec028Router()

	// Garbage subject token: 404, not 204.
	code, body := doReq(r, http.MethodDelete, "/v3/auth/tokens",
		map[string]string{"X-Subject-Token": "not-a-token"})
	if code != http.StatusNotFound {
		t.Errorf("revoke garbage token: code=%d body=%s; want 404", code, body)
	}

	// Missing subject token: 400.
	code, _ = doReq(r, http.MethodDelete, "/v3/auth/tokens", map[string]string{})
	if code != http.StatusBadRequest {
		t.Errorf("revoke without subject token: code=%d; want 400", code)
	}

	// A well-formed, validly signed, unexpired token: revoked with 204.
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, &TokenClaims{
		UserID: "00000000-0000-0000-0000-00000000a0a0",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte("spec028-test-secret"))
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if _, err := authSvc.ValidateToken(tok); err != nil {
		t.Fatalf("minted token must validate: %v", err)
	}
	code, body = doReq(r, http.MethodDelete, "/v3/auth/tokens",
		map[string]string{"X-Subject-Token": tok})
	if code != http.StatusNoContent {
		t.Errorf("revoke valid token: code=%d body=%s; want 204", code, body)
	}
}

// TestIdentityErrorEnvelopeIsKeystoneShaped — keystoneauth1's from_response
// parses only {"error": {...}} (or the API-SIG {"errors": [...]}) bodies;
// every other JSON shape degrades to "Unrecognized schema in response body.",
// throwing away the reason. Upstream Keystone renders code/message/title, so
// 'domain delete <enabled>' must say why it refused (SPEC-028).
func TestIdentityErrorEnvelopeIsKeystoneShaped(t *testing.T) {
	requireDB(t)
	r, _ := spec028Router()

	name := "spec028-envelope-" + uuid.NewString()[:8]
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM domains WHERE name=$1", name) })

	code, body := doReqBody(r, http.MethodPost, "/v3/domains",
		`{"domain":{"name":"`+name+`"}}`, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("create domain: code=%d body=%s", code, body)
	}

	// Deleting an ENABLED domain is upstream's 403 — in upstream's envelope.
	var id string
	if err := testPool.QueryRow(context.Background(),
		"SELECT id::text FROM domains WHERE name=$1", name).Scan(&id); err != nil {
		t.Fatalf("resolve domain: %v", err)
	}
	code, body = doReq(r, http.MethodDelete, "/v3/domains/"+id, adminHdr)
	if code != http.StatusForbidden {
		t.Fatalf("delete enabled domain: code=%d body=%s; want 403", code, body)
	}
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Title   string `json:"title"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error body is not keystone-shaped: %v (body=%s)", err, body)
	}
	if env.Error.Code != http.StatusForbidden || env.Error.Title != "Forbidden" {
		t.Errorf("envelope code/title = %d/%q; want 403/Forbidden (body=%s)", env.Error.Code, env.Error.Title, body)
	}
	if env.Error.Message == "" {
		t.Errorf("envelope message is empty; the refusal reason must reach keystoneauth clients (body=%s)", body)
	}

	// And the 404 shape, the one every 'show <missing>' hits.
	code, body = doReq(r, http.MethodGet, "/v3/domains/"+uuid.NewString(), adminHdr)
	if code != http.StatusNotFound {
		t.Fatalf("show missing domain: code=%d body=%s; want 404", code, body)
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("404 body is not keystone-shaped: %v (body=%s)", err, body)
	}
	if env.Error.Code != http.StatusNotFound || env.Error.Title != "Not Found" || env.Error.Message == "" {
		t.Errorf("404 envelope = %+v; want code 404, title Not Found, non-empty message", env.Error)
	}
}
