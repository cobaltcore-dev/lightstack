package nova

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// SPEC-023 US1: the shared owner-or-administrator lookup for server operations.
//
// These encode contracts/compute-override.md C1, C2 and C6, and research R10's
// disclosure rule. The subtle one is C6: an administrator may address another
// project's instance by UUID but NOT by name, because Lightstack accepts a name where
// upstream accepts only a UUID, and two projects may own the same name. Left
// cross-tenant, `openstack server delete web-1` could destroy the wrong tenant's
// server.

const (
	authzProjectA = "00000000-0000-0000-0000-0000000000a1"
	authzProjectB = "00000000-0000-0000-0000-0000000000a2"
)

// seedAuthzFixtures creates two projects and one instance in each, both named
// the same, so the name-collision case in C6 is exercised rather than assumed.
// Returns the instance ids owned by A and by B.
func seedAuthzFixtures(t *testing.T) (idA, idB string) {
	t.Helper()
	ctx := context.Background()
	for id, name := range map[string]string{authzProjectA: "spec023-a", authzProjectB: "spec023-b"} {
		// domain_id is set deliberately: a project with a NULL domain makes
		// ListProjects fail its scan and answer 500, so fixtures that omit it
		// break unrelated identity tests.
		if _, err := testPool.Exec(ctx,
			`INSERT INTO projects (id, name, description, enabled, domain_id)
			 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))
			 ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
			t.Fatalf("seeding project %s: %v", name, err)
		}
	}
	// Same instance name in both projects — the collision C6 guards against.
	for _, p := range []string{authzProjectA, authzProjectB} {
		if _, err := testPool.Exec(ctx,
			`DELETE FROM instances WHERE project_id = $1 AND name = 'web-1'`, p); err != nil {
			t.Fatalf("clearing instances for %s: %v", p, err)
		}
	}
	// user_id/flavor_id must be set: the detail listing scans them into plain
	// strings, so a NULL makes the row silently dropped from the response.
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('web-1', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
		authzProjectA, seededUserID).Scan(&idA); err != nil {
		t.Fatalf("seeding instance in A: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('web-1', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
		authzProjectB, seededUserID).Scan(&idB); err != nil {
		t.Fatalf("seeding instance in B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM instances WHERE project_id = ANY($1)`, []string{authzProjectA, authzProjectB})
	})
	return idA, idB
}

// authzCtx builds a request context scoped to a project, with or without the
// administrative role.
func authzCtx(projectID string, admin bool) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v2.1/servers", nil)
	c.Set("project_id", projectID)
	if admin {
		c.Set("roles", []string{"admin"})
	} else {
		c.Set("roles", []string{"member"})
	}
	return c
}

// TestTargetInstance_AdminReachesAnyProjectByID: C1. The override's whole point.
func TestTargetInstance_AdminReachesAnyProjectByID(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)

	id, owner, err := targetInstance(authzCtx(authzProjectA, true), testPool, idB)
	if err != nil {
		t.Fatalf("admin in A resolving B's instance by id: %v; want success", err)
	}
	if id != idB {
		t.Errorf("id = %q, want %q", id, idB)
	}
	if owner != authzProjectB {
		t.Errorf("owner = %q, want %q (the OWNING project, not the caller's)", owner, authzProjectB)
	}
}

// TestTargetInstance_OwnerReachesOwn: the unprivileged happy path is unchanged.
func TestTargetInstance_OwnerReachesOwn(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)

	id, owner, err := targetInstance(authzCtx(authzProjectA, false), testPool, idA)
	if err != nil {
		t.Fatalf("member in A resolving A's own instance: %v; want success", err)
	}
	if id != idA || owner != authzProjectA {
		t.Errorf("got (%q, %q), want (%q, %q)", id, owner, idA, authzProjectA)
	}
}

// TestTargetInstance_NonAdminCannotReachForeign: C2. A member gets no-rows for
// another project's instance.
func TestTargetInstance_NonAdminCannotReachForeign(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)

	if _, _, err := targetInstance(authzCtx(authzProjectA, false), testPool, idB); !errors.Is(err, database.ErrNoRows) {
		t.Fatalf("member in A resolving B's instance: err = %v; want database.ErrNoRows", err)
	}
}

// TestTargetInstance_ForeignAndAbsentAreIndistinguishable: research R10 and
// FR-003. A caller must not be able to tell "someone else's" from "does not
// exist" — a distinguishable answer confirms the instance exists.
func TestTargetInstance_ForeignAndAbsentAreIndistinguishable(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)

	_, _, foreignErr := targetInstance(authzCtx(authzProjectA, false), testPool, idB)
	_, _, absentErr := targetInstance(authzCtx(authzProjectA, false), testPool,
		"11111111-1111-1111-1111-111111111111")

	if !errors.Is(foreignErr, database.ErrNoRows) || !errors.Is(absentErr, database.ErrNoRows) {
		t.Fatalf("foreign=%v absent=%v; both must be database.ErrNoRows", foreignErr, absentErr)
	}
}

// TestTargetInstance_AdminNameStaysInOwnProject: C6 / FR-005c — the case that
// could otherwise delete the wrong tenant's server. Both projects own an
// instance named "web-1"; an admin scoped to A addressing "web-1" must get A's,
// never B's and never an arbitrary choice.
func TestTargetInstance_AdminNameStaysInOwnProject(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)

	id, owner, err := targetInstance(authzCtx(authzProjectA, true), testPool, "web-1")
	if err != nil {
		t.Fatalf("admin in A resolving name 'web-1': %v; want A's own instance", err)
	}
	if id == idB {
		t.Fatal("admin addressing 'web-1' resolved to project B's instance — a name must never span projects")
	}
	if id != idA || owner != authzProjectA {
		t.Errorf("got (%q, %q), want A's (%q, %q)", id, owner, idA, authzProjectA)
	}
}

// TestTargetInstance_AdminNameNotInOwnProjectIsNotFound: FR-005d. A name that
// matches nothing in the caller's own project must NOT fall through to a
// cross-project search.
func TestTargetInstance_AdminNameNotInOwnProjectIsNotFound(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)
	ctx := context.Background()
	var onlyB string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, status) VALUES ('only-in-b', $1, 'ACTIVE') RETURNING id::text`,
		authzProjectB).Scan(&onlyB); err != nil {
		t.Fatalf("seeding only-in-b: %v", err)
	}

	if _, _, err := targetInstance(authzCtx(authzProjectA, true), testPool, "only-in-b"); !errors.Is(err, database.ErrNoRows) {
		t.Fatalf("admin in A resolving name 'only-in-b' (owned by B): err = %v; want database.ErrNoRows", err)
	}
}

// TestTargetInstance_MalformedRefIsNotFound: a reference that is neither a UUID
// nor an existing name must be a clean no-rows, not a Postgres cast error
// surfacing as a 500. This is the shape #515 fixed for identity ids.
func TestTargetInstance_MalformedRefIsNotFound(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)

	for _, ref := range []string{"not-a-uuid", "'; DROP TABLE instances; --", ""} {
		_, _, err := targetInstance(authzCtx(authzProjectA, true), testPool, ref)
		if !errors.Is(err, database.ErrNoRows) {
			t.Errorf("targetInstance(%q) = %v; want database.ErrNoRows", ref, err)
		}
	}
}

// TestTargetInstance_UnscopedCallerIsNotFound: an unscoped token names no
// project, so it owns no instance. The empty project used to reach the uuid
// column, where Postgres refused the cast, and every per-server route answered
// 500 instead of the 404 an absent server gets.
func TestTargetInstance_UnscopedCallerIsNotFound(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	for _, ref := range []string{idA, "web-1"} {
		if _, _, err := targetInstance(authzCtx("", false), testPool, ref); !errors.Is(err, database.ErrNoRows) {
			t.Errorf("%s: err = %v, want database.ErrNoRows", ref, err)
		}
	}
}

// TestUnscopedTokenOwnsNothing: the auth middleware accepts an unscoped token and
// leaves the project empty. Such a caller owns nothing — a server lookup is 404,
// as for an absent server and as upstream answers, and a listing is empty. Both
// used to bind the empty project to the uuid column and answer 500.
func TestUnscopedTokenOwnsNothing(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	unscoped := map[string]string{"X-Test-User": seededUserID} // no project, no roles

	if code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idA, "", unscoped); code != http.StatusNotFound {
		t.Errorf("GET /servers/{id}: code=%d, want 404. body: %s", code, body)
	}
	for _, path := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
		code, body := doReq(r, http.MethodGet, path, "", unscoped)
		var resp struct {
			Servers []json.RawMessage `json:"servers"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil || len(resp.Servers) != 0 {
			t.Errorf("GET %s: code=%d body=%s; want 200 with no servers", path, code, body)
		}
	}
}

// TestUnscopedTokenServerGroups: the same rule for server groups. An unscoped
// caller owns none, so the listing is empty and a lookup or delete is 404; with no
// project to create into, creation is refused with 403. All but the lookup bound
// the empty project to the uuid column and answered 500.
func TestUnscopedTokenServerGroups(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	unscoped := map[string]string{"X-Test-User": seededUserID} // no project, no roles
	const group = "/v2.1/os-server-groups/11111111-1111-1111-1111-111111111111"

	code, body := doReq(r, http.MethodGet, "/v2.1/os-server-groups", "", unscoped)
	var resp struct {
		Groups []json.RawMessage `json:"server_groups"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil || len(resp.Groups) != 0 {
		t.Errorf("list: code=%d body=%s; want 200 with no server groups", code, body)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, group, "", http.StatusNotFound},
		{http.MethodDelete, group, "", http.StatusNotFound},
		{http.MethodPost, "/v2.1/os-server-groups", `{"server_group":{"name":"g","policies":["affinity"]}}`,
			http.StatusForbidden},
	} {
		if code, body := doReq(r, tc.method, tc.path, tc.body, unscoped); code != tc.want {
			t.Errorf("%s %s: code=%d, want %d. body: %s", tc.method, tc.path, code, tc.want, body)
		}
	}
}

// TestAdminOnlyActionsReachOtherProjects: FR-008a / contract C4.
//
// Evacuate and reset-state are gated to administrators and then, before
// SPEC-023, scoped their lookup to the administrator's OWN project — so they
// answered 404 for exactly the instances they exist to handle. This is the
// RED→GREEN evidence for that pair.
func TestAdminOnlyActionsReachOtherProjects(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()

	adminInA := map[string]string{
		"X-Test-Project": authzProjectA,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "admin",
	}

	for _, tc := range []struct{ name, body string }{
		{"os-resetState", `{"os-resetState":{"state":"error"}}`},
		{"evacuate", `{"evacuate":{"host":"host-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/action", tc.body, adminInA)
			if code == http.StatusNotFound {
				t.Fatalf("admin in A acting on B's instance: 404 — the pre-SPEC-023 defect. body: %s", body)
			}
			if code >= 500 {
				t.Fatalf("status = %d; want a client-visible outcome, not a server error. body: %s", code, body)
			}
		})
	}
}

// TestResetStateAction_DatabaseErrorIs500: reset-state resolves its own target
// and answered 404 for EVERY lookup error, so a database failure read as "no
// such instance". Called directly: through the route, the dispatcher's own
// resolve would fail first and hide this one. A cancelled request context forces
// context.Canceled out of pgx without disturbing the shared pool.
func TestResetStateAction_DatabaseErrorIs500(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/v2.1/servers/"+idA+"/action", nil).WithContext(ctx)
	c.Params = gin.Params{{Key: "id", Value: idA}}
	c.Set("project_id", authzProjectA)
	c.Set("roles", []string{"admin"})
	c.Set("action_data", map[string]any{"state": "error"})

	NewService(testPool, "", "stub", nil).ResetStateAction(c, idA, authzProjectA)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — a database failure must not be answered as 404, "+
			"or an outage reads as an authorization result", w.Code)
	}
}

// TestAdminOnlyActionsStillRefuseNonAdmins: the gate itself must not have been
// loosened while fixing its scoping. A member gets 403 — not 404 — because the
// role check fires before any lookup, so there is nothing to disclose.
func TestAdminOnlyActionsStillRefuseNonAdmins(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	r := newNovaTestRouter()

	memberInA := map[string]string{
		"X-Test-Project": authzProjectA,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "member",
	}

	for _, tc := range []struct{ name, body string }{
		{"os-resetState", `{"os-resetState":{"state":"error"}}`},
		{"evacuate", `{"evacuate":{"host":"host-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action", tc.body, memberInA)
			if code != http.StatusForbidden {
				t.Errorf("member performing %s: code=%d, want 403. body: %s", tc.name, code, body)
			}
		})
	}
}

// TestOverrideAcrossServerOperations: contracts C1 and C3 at the HTTP level —
// an administrator scoped to A reaching project B's server, and a member of A
// still refused with the same answer they get for an absent server.
func TestOverrideAcrossServerOperations(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()

	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}
	memberInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	t.Run("admin shows another project's server", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idB, "", adminInA)
		if code != http.StatusOK {
			t.Fatalf("code=%d, want 200. body: %s", code, body)
		}
		// The response must name the OWNING project, not the caller's.
		if !strings.Contains(body, authzProjectB) {
			t.Errorf("body does not report the owning project %s: %s", authzProjectB, body)
		}
	})

	t.Run("member is refused, indistinguishably from absent", func(t *testing.T) {
		foreignCode, _ := doReq(r, http.MethodGet, "/v2.1/servers/"+idB, "", memberInA)
		absentCode, _ := doReq(r, http.MethodGet,
			"/v2.1/servers/11111111-1111-1111-1111-111111111111", "", memberInA)
		if foreignCode != http.StatusNotFound || absentCode != http.StatusNotFound {
			t.Errorf("foreign=%d absent=%d; both must be 404 so neither confirms existence",
				foreignCode, absentCode)
		}
	})

	t.Run("admin acts on another project's server", func(t *testing.T) {
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/action",
			`{"lock":null}`, adminInA)
		if code == http.StatusNotFound {
			t.Fatalf("admin locking B's server: 404 — the override did not apply. body: %s", body)
		}
	})

	t.Run("member cannot act on another project's server", func(t *testing.T) {
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/action",
			`{"lock":null}`, memberInA)
		if code != http.StatusNotFound {
			t.Errorf("member locking B's server: code=%d, want 404. body: %s", code, body)
		}
	})
}

// TestTargetInstance_AdminFallsBackToOwnProject: an administrator's UUID-shaped
// reference is tried as an id across projects first, and an id match wins. When
// no server has that id it may still be a name in the administrator's own
// project, as it is for everyone else — the admin path stopped at the id lookup,
// so a UUID-shaped name a member could address was not found for an
// administrator.
func TestTargetInstance_AdminFallsBackToOwnProject(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t) // its cleanup removes every instance in A
	ctx := context.Background()
	seedInA := func(name string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
			 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
			name, authzProjectA, seededUserID).Scan(&id); err != nil {
			t.Fatalf("seeding %q: %v", name, err)
		}
		return id
	}
	uuidName := uuid.NewString() // no server has this id
	namedInA := seedInA(uuidName)
	seedInA(idB) // an A server named after B's server's id
	admin := authzCtx(authzProjectA, true)

	if got, _, err := targetInstance(admin, testPool, uuidName); err != nil || got != namedInA {
		t.Errorf("admin addressing a UUID-shaped name in their own project: got %q (err %v), want %q",
			got, err, namedInA)
	}
	// An id still wins: B's server, not the A server named after it.
	if got, _, err := targetInstance(admin, testPool, idB); err != nil || got != idB {
		t.Errorf("admin addressing B's server by id: got %q (err %v), want %q", got, err, idB)
	}
	// uuid.Validate accepts the urn form, which Postgres refuses; it must still resolve.
	if got, _, err := targetInstance(admin, testPool, "urn:uuid:"+idB); err != nil || got != idB {
		t.Errorf("admin addressing B's server in urn form: got %q (err %v), want %q", got, err, idB)
	}
}

// TestTargetInstance_IDMatchWinsOverName: a reference matching one server's id
// and another's NAME resolves to the id. The lookup matched either and took the
// first row, so once the target's row had been updated — which moves it behind
// the other — its own id resolved to the server named after it.
func TestTargetInstance_IDMatchWinsOverName(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE')`,
		idA, authzProjectA, seededUserID); err != nil {
		t.Fatalf("seeding the server named after the target: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE instances SET updated_at = now() WHERE id = $1`, idA); err != nil {
		t.Fatalf("touching the target: %v", err)
	}
	got, _, err := targetInstance(authzCtx(authzProjectA, false), testPool, idA)
	if err != nil || got != idA {
		t.Errorf("resolved %q (err %v); want the target %q, not the server named after it", got, err, idA)
	}
}

// TestCrossTenantRefusalIsLoggedWithOwner: contract C8 / FR-007b. A refused
// cross-tenant lookup logs one WARN naming the caller's project, the target and
// the OWNING project; an absent server, a missed name and the caller's own
// server log nothing. The warning fired on every miss — an absent id read as a
// cross-tenant probe — and never named the owner.
func TestCrossTenantRefusalIsLoggedWithOwner(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	memberInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}
	type refusal struct {
		Level         string `json:"level"`
		Message       string `json:"message"`
		CallerProject string `json:"caller_project"`
		InstanceRef   string `json:"instance_ref"`
		OwnerProject  string `json:"owner_project"`
	}
	refusals := func(path string) (int, []refusal) {
		var buf bytes.Buffer
		restore := log.Logger
		log.Logger = zerolog.New(&buf)
		defer func() { log.Logger = restore }()
		code, _ := doReq(r, http.MethodGet, path, "", memberInA)
		var out []refusal
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var rec refusal
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Message == "refused cross-tenant compute access" {
				out = append(out, rec)
			}
		}
		return code, out
	}

	code, got := refusals("/v2.1/servers/" + idB)
	if code != http.StatusNotFound || len(got) != 1 {
		t.Fatalf("foreign server: code=%d with %d refusal line(s); want 404 and exactly one", code, len(got))
	}
	if got[0].Level != "warn" || got[0].CallerProject != authzProjectA ||
		got[0].InstanceRef != idB || got[0].OwnerProject != authzProjectB {
		t.Errorf("refusal = %+v; want a warn naming caller %s, target %s and owner %s",
			got[0], authzProjectA, idB, authzProjectB)
	}
	for _, path := range []string{
		"/v2.1/servers/11111111-1111-1111-1111-111111111111", // absent
		"/v2.1/servers/no-such-server",                       // a name, which never spans projects
		"/v2.1/servers/" + idA,                               // the caller's own
	} {
		if _, got := refusals(path); len(got) != 0 {
			t.Errorf("GET %s logged %d refusal(s); only a cross-tenant refusal is logged", path, len(got))
		}
	}
}

// TestResolvedActionsTouchOnlyTheirTarget: once a server is resolved to its id,
// what follows must address that id alone. The stub lifecycle updates and the
// delete still matched `id OR name`, so a second server in the project NAMED
// after the target's id was stopped, started, rebooted and deleted along with it
// — and the deleted one kept its ports and attachments.
func TestResolvedActionsTouchOnlyTheirTarget(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t) // its cleanup removes every instance in A, the twin included
	ctx := context.Background()
	var twin string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
		idA, authzProjectA, seededUserID).Scan(&twin); err != nil {
		t.Fatalf("seeding the server named after the target: %v", err)
	}
	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}
	status := func(id string) string {
		var s string
		if err := testPool.QueryRow(ctx, `SELECT status FROM instances WHERE id = $1`, id).Scan(&s); err != nil {
			return "GONE"
		}
		return s
	}
	setTwin := func(s string) {
		if _, err := testPool.Exec(ctx, `UPDATE instances SET status = $1 WHERE id = $2`, s, twin); err != nil {
			t.Fatalf("setting the second server's status: %v", err)
		}
	}
	act := func(body string) {
		if code, resp := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action", body, owner); code != http.StatusAccepted {
			t.Fatalf("%s on the target: code=%d, want 202. body: %s", body, code, resp)
		}
	}

	setTwin("ACTIVE")
	act(`{"os-stop":null}`)
	if got := status(twin); got != "ACTIVE" {
		t.Errorf("os-stop on the target changed the second server to %s", got)
	}

	setTwin("SHUTOFF")
	act(`{"os-start":null}`)
	if got := status(twin); got != "SHUTOFF" {
		t.Errorf("os-start on the target changed the second server to %s", got)
	}

	// A reboot updates twice — at once, and again a second later — so let the
	// target settle before looking at the second server.
	act(`{"reboot":{"type":"SOFT"}}`)
	for deadline := time.Now().Add(5 * time.Second); status(idA) != "ACTIVE" && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if got := status(twin); got != "SHUTOFF" {
		t.Errorf("reboot on the target changed the second server to %s", got)
	}

	if code, resp := doReq(r, http.MethodDelete, "/v2.1/servers/"+idA, "", owner); code != http.StatusNoContent {
		t.Fatalf("deleting the target: code=%d, want 204. body: %s", code, resp)
	}
	if status(twin) == "GONE" {
		t.Error("deleting the target deleted the second server too")
	}
}

// TestServerMetadataFollowsTheOverride: FR-004 / contracts C1 and C3 for
// /servers/{id}/metadata. The three metadata handlers checked only that the
// instance existed — no project, no role — so any tenant could read, merge into
// or replace another tenant's server metadata by UUID.
func TestServerMetadataFollowsTheOverride(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO instance_metadata (instance_id, meta_key, meta_value) VALUES ($1, 'owner', 'b')`,
		idB); err != nil {
		t.Fatalf("seeding B's metadata: %v", err)
	}

	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}
	memberInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPost, `{"metadata":{"owner":"a"}}`},
		{http.MethodPut, `{"metadata":{"owner":"a"}}`},
	} {
		t.Run("member "+tc.method+" on another project's server is refused", func(t *testing.T) {
			code, body := doReq(r, tc.method, "/v2.1/servers/"+idB+"/metadata", tc.body, memberInA)
			if code != http.StatusNotFound {
				t.Errorf("code=%d, want 404, as for an absent server. body: %s", code, body)
			}
		})
	}

	t.Run("refused writes leave the owner's metadata alone", func(t *testing.T) {
		var v string
		if err := testPool.QueryRow(context.Background(),
			`SELECT meta_value FROM instance_metadata WHERE instance_id = $1 AND meta_key = 'owner'`,
			idB).Scan(&v); err != nil || v != "b" {
			t.Errorf("B's metadata after the refused writes: %q (err %v), want \"b\"", v, err)
		}
	})

	t.Run("admin reads another project's server metadata", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idB+"/metadata", "", adminInA)
		if code != http.StatusOK || !strings.Contains(body, `"owner":"b"`) {
			t.Errorf("code=%d body=%s; want 200 carrying B's metadata", code, body)
		}
	})

	t.Run("owner reads their own", func(t *testing.T) {
		if code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idA+"/metadata", "", memberInA); code != http.StatusOK {
			t.Errorf("code=%d, want 200. body: %s", code, body)
		}
	})
}

// recordedAs returns the project and user of the latest action row with this name
// on an instance, failing the test when there is none.
func recordedAs(t *testing.T, instanceID, action string) (project, user string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT project_id::text, COALESCE(user_id::text,'') FROM instance_actions
		 WHERE instance_id = $1 AND action = $2 ORDER BY start_time DESC LIMIT 1`,
		instanceID, action).Scan(&project, &user); err != nil {
		t.Fatalf("no %q action recorded on %s: %v", action, instanceID, err)
	}
	return project, user
}

// TestInterfaceAttachmentsRecordedAgainstOwner: FR-007a / research R6. The
// interface routes are separate from ServerAction, so the dispatcher's recording
// never reached them: an administrator could attach or detach an interface on a
// tenant's server and the tenant's history showed nothing. Upstream records both
// (nova/compute/instance_actions.py ATTACH_INTERFACE, DETACH_INTERFACE).
func TestInterfaceAttachmentsRecordedAgainstOwner(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	ctx := context.Background()
	var netID, portID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id) VALUES ('spec023-net-b', $1) RETURNING id::text`,
		authzProjectB).Scan(&netID); err != nil {
		t.Fatalf("seeding B's network: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, netID) })
	if err := testPool.QueryRow(ctx,
		`INSERT INTO ports (network_id, project_id, mac_address, fixed_ips)
		 VALUES ($1, $2, 'fa:16:3e:00:02:3b', '[]'::jsonb) RETURNING id::text`,
		netID, authzProjectB).Scan(&portID); err != nil {
		t.Fatalf("seeding B's port: %v", err)
	}
	r := newNovaTestRouter()
	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/os-interface",
		`{"interfaceAttachment":{"port_id":"`+portID+`"}}`, adminInA); code != http.StatusOK {
		t.Fatalf("admin attaching to B's server: code=%d, want 200. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodDelete,
		"/v2.1/servers/"+idB+"/os-interface/"+portID, "", adminInA); code != http.StatusAccepted {
		t.Fatalf("admin detaching from B's server: code=%d, want 202. body: %s", code, body)
	}
	for _, action := range []string{"attach_interface", "detach_interface"} {
		if project, user := recordedAs(t, idB, action); project != authzProjectB || user != seededUserID {
			t.Errorf("%s recorded against project %s, user %q; want the owner %s and the acting "+
				"administrator %q", action, project, user, authzProjectB, seededUserID)
		}
	}
}

// TestVolumeAttachmentsRecordedAgainstOwner: the same gap on the volume routes.
// Upstream records both (nova/compute/instance_actions.py ATTACH_VOLUME,
// DETACH_VOLUME).
func TestVolumeAttachmentsRecordedAgainstOwner(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	var volID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO volumes (size_gb, status, project_id) VALUES (1, 'available', $1) RETURNING id::text`,
		authzProjectB).Scan(&volID); err != nil {
		t.Fatalf("seeding B's volume: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volID) })
	r := newNovaTestRouter()
	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/os-volume_attachments",
		`{"volumeAttachment":{"volumeId":"`+volID+`"}}`, adminInA); code != http.StatusOK {
		t.Fatalf("admin attaching to B's server: code=%d, want 200. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodDelete,
		"/v2.1/servers/"+idB+"/os-volume_attachments/"+volID, "", adminInA); code != http.StatusAccepted {
		t.Fatalf("admin detaching from B's server: code=%d, want 202. body: %s", code, body)
	}
	for _, action := range []string{"attach_volume", "detach_volume"} {
		if project, user := recordedAs(t, idB, action); project != authzProjectB || user != seededUserID {
			t.Errorf("%s recorded against project %s, user %q; want the owner %s and the acting "+
				"administrator %q", action, project, user, authzProjectB, seededUserID)
		}
	}
}

// TestMigrationActionsRecordedAgainstOwner: the per-server migration routes are
// separate from ServerAction too. Upstream records both operations
// (LIVE_MIGRATION_CANCEL for an abort, LIVE_MIGRATION_FORCE_COMPLETE); a server
// update, its metadata and its tags it records under no action name at all, so
// neither does Lightstack (FR-007a).
func TestMigrationActionsRecordedAgainstOwner(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	seedMigration := func() string {
		var id string
		if err := testPool.QueryRow(context.Background(),
			`INSERT INTO server_migrations (server_uuid, status, migration_type)
			 VALUES ($1, 'running', 'live-migration') RETURNING id::text`, idB).Scan(&id); err != nil {
			t.Fatalf("seeding a migration of B's server: %v", err)
		}
		return id
	}
	toComplete, toAbort := seedMigration(), seedMigration()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM server_migrations WHERE server_uuid = $1`, idB)
	})
	r := newNovaTestRouter()
	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/migrations/"+toComplete+"/action",
		`{"force_complete":null}`, adminInA); code != http.StatusAccepted {
		t.Fatalf("admin force-completing B's migration: code=%d, want 202. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodDelete,
		"/v2.1/servers/"+idB+"/migrations/"+toAbort, "", adminInA); code != http.StatusNoContent {
		t.Fatalf("admin aborting B's migration: code=%d, want 204. body: %s", code, body)
	}
	for _, action := range []string{"live_migration_force_complete", "live_migration_cancel"} {
		if project, user := recordedAs(t, idB, action); project != authzProjectB || user != seededUserID {
			t.Errorf("%s recorded against project %s, user %q; want the owner %s and the acting "+
				"administrator %q", action, project, user, authzProjectB, seededUserID)
		}
	}
}

// TestActionsRecordedAgainstOwner: FR-007 / FR-007a / contract C7. After an
// administrator acts on another project's server, the OWNING tenant must be able
// to see it in their own server's history, with the acting user identified.
func TestActionsRecordedAgainstOwner(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)
	r := newNovaTestRouter()

	adminInA := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin",
	}
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idB+"/action",
		`{"lock":null}`, adminInA); code == http.StatusNotFound {
		t.Fatalf("admin action on B's server was refused: %s", body)
	}

	// Recorded against B, not against the administrator's project A.
	var project, user string
	if err := testPool.QueryRow(context.Background(),
		`SELECT project_id::text, COALESCE(user_id::text,'') FROM instance_actions
		 WHERE instance_id = $1 AND action = 'lock' ORDER BY start_time DESC LIMIT 1`,
		idB).Scan(&project, &user); err != nil {
		t.Fatalf("no action row written for the admin's lock — FR-007 would be vacuous: %v", err)
	}
	if project != authzProjectB {
		t.Errorf("recorded project = %s, want the OWNER %s (recording the caller's project "+
			"leaves the tenant's own history empty)", project, authzProjectB)
	}
	if user != seededUserID {
		t.Errorf("recorded user = %q, want the acting administrator %q", user, seededUserID)
	}

	// And the owner can read it back through the API.
	ownerB := map[string]string{
		"X-Test-Project": authzProjectB, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+idB+"/os-instance-actions", "", ownerB)
	if code != http.StatusOK || !strings.Contains(body, "lock") {
		t.Errorf("owner reading their server's history: code=%d body=%s; want the lock action", code, body)
	}
}

// TestServerActionRequiresExactlyOneAction: SRC-2. A body naming two actions was
// previously answered 202 after performing whichever the dispatch ladder reached
// first and silently discarding the rest — a contradictory request accepted,
// partially executed, and reported as success.
//
// It also made the recorded action name nondeterministic: the ladder picks the
// first action in its fixed order, actionName picked an arbitrary map key, so the
// history could name an action that never ran (FR-007).
func TestServerActionRequiresExactlyOneAction(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	for _, tc := range []struct{ name, body string }{
		{"two actions", `{"lock":null,"unlock":null}`},
		{"no action", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action", tc.body, owner)
			if code != http.StatusBadRequest {
				t.Errorf("code=%d, want 400. body: %s", code, body)
			}
		})
	}

	// A single action is unaffected.
	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action", `{"lock":null}`, owner)
	if code != http.StatusAccepted {
		t.Errorf("single action: code=%d, want 202. body: %s", code, body)
	}
}

// TestUnknownActionRecordsNothing: an action this API does not know is refused
// before the target is resolved, so it never reaches the action history — a
// request that performed nothing must not appear in the server's record
// (FR-007a). TestUnknownActionKeepsConsoleTokens pins the console half of the
// same ordering.
func TestUnknownActionRecordsNothing(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	member := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action", `{"no_such_action":{}}`, member)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400. body: %s", code, body)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM instance_actions WHERE instance_id = $1`, idA).Scan(&n); err != nil {
		t.Fatalf("counting action records: %v", err)
	}
	if n != 0 {
		t.Errorf("%d action record(s) written for a refused action; want 0", n)
	}
}

// TestInstanceActionNamesFollowUpstream: FR-007a / research R6. Every action the
// dispatcher records is named in upstream's vocabulary
// (nova/compute/instance_actions.py), so os-instance-actions reads as it does
// against Nova. Trimming "os-" got two wrong: a live migration is
// `live-migration`, and a force delete is recorded as `delete`. Reset-state
// really is `resetState` upstream (RESET_STATE).
//
// Every dispatched action must be listed, so a new one has to choose its name.
func TestInstanceActionNamesFollowUpstream(t *testing.T) {
	want := map[string]string{
		"reboot": "reboot", "os-stop": "stop", "os-start": "start",
		"suspend": "suspend", "resume": "resume", "pause": "pause", "unpause": "unpause",
		"lock": "lock", "unlock": "unlock", "rescue": "rescue", "rebuild": "rebuild",
		"shelve": "shelve", "shelveOffload": "shelveOffload", "unshelve": "unshelve",
		"resize": "resize", "confirmResize": "confirmResize", "revertResize": "revertResize",
		"migrate": "migrate", "os-migrateLive": "live-migration", "evacuate": "evacuate",
		"restore": "restore", "forceDelete": "delete", "os-resetState": "resetState",
		"changePassword": "changePassword", "createImage": "createImage", "createBackup": "createBackup",
		// No upstream record exists for these; they keep their own names.
		"addSecurityGroup": "addSecurityGroup", "removeSecurityGroup": "removeSecurityGroup",
		"os-resetNetwork": "resetNetwork",
	}
	dispatched := []string{}
	for action := range serverActions {
		dispatched = append(dispatched, action)
	}
	for action := range lifecycleActions {
		dispatched = append(dispatched, action)
	}
	for _, action := range dispatched {
		name, listed := want[action]
		if !listed {
			t.Errorf("dispatched action %q has no expected record name here", action)
			continue
		}
		if got := instanceActionName(action); got != name {
			t.Errorf("instanceActionName(%q) = %q, want %q", action, got, name)
		}
	}
}

// TestActionRecordNamesThePerformedAction: with the arity check in place, the
// recorded name always matches the action the ladder dispatched.
func TestActionRecordNamesThePerformedAction(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)
	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action",
		`{"unlock":null}`, owner); code != http.StatusAccepted {
		t.Fatalf("unlock: code=%d body=%s", code, body)
	}

	var action string
	if err := testPool.QueryRow(context.Background(),
		`SELECT action FROM instance_actions WHERE instance_id = $1
		 ORDER BY start_time DESC LIMIT 1`, idA).Scan(&action); err != nil {
		t.Fatalf("reading the action record: %v", err)
	}
	if action != "unlock" {
		t.Errorf("recorded action = %q, want \"unlock\" — the record must name the action performed", action)
	}
}

// TestActionRecordCarriesTheRequestID: SRC-5. request_id is client-facing —
// os-instance-actions returns it and GET .../os-instance-actions/{request_id}
// routes on it. Both insert sites used to invent their own value, in two
// different formats, so the column's shape was unreliable and no record could be
// correlated with the request log that produced it.
func TestActionRecordCarriesTheRequestID(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)

	// The real logging middleware sets request_id; the bare test router does not,
	// so this also exercises the fallback.
	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+idA+"/action",
		`{"lock":null}`, owner); code != http.StatusAccepted {
		t.Fatalf("lock: code=%d body=%s", code, body)
	}

	var got string
	if err := testPool.QueryRow(context.Background(),
		`SELECT request_id FROM instance_actions WHERE instance_id = $1
		 ORDER BY start_time DESC LIMIT 1`, idA).Scan(&got); err != nil {
		t.Fatalf("reading request_id: %v", err)
	}
	if !strings.HasPrefix(got, "req-") {
		t.Errorf("request_id = %q; want the upstream req-<uuid> shape that "+
			"os-instance-actions exposes and routes on", got)
	}
}

// TestRequestIDPrefersTheContext: with the middleware's id present, the record
// carries exactly it — that string equality is what makes correlation possible.
func TestRequestIDPrefersTheContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("request_id", "req-from-middleware")
	if got := requestID(c); got != "req-from-middleware" {
		t.Errorf("requestID = %q, want the context's value verbatim", got)
	}

	bare, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := requestID(bare); !strings.HasPrefix(got, "req-") {
		t.Errorf("fallback = %q, want a req- prefixed id", got)
	}
}

// TestResolveServerOr404_DatabaseErrorIs500: SRC-6. The function deliberately
// separates database.ErrNoRows from every other error, and that distinction is
// security-relevant in both directions:
//
//   - no-rows must be 404, because 403 would confirm a foreign instance exists;
//   - any other error must be 500, because answering a transient database failure
//     as "not found" would let an outage read as an authorization result — an
//     operator would chase a permissions bug that isn't there.
//
// Nothing exercised the second branch. A cancelled request context forces
// context.Canceled out of pgx without disturbing the shared pool.
func TestResolveServerOr404_DatabaseErrorIs500(t *testing.T) {
	requireDB(t)
	idA, _ := seedAuthzFixtures(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("GET", "/v2.1/servers/"+idA, nil).WithContext(ctx)
	c.Set("project_id", authzProjectA)
	c.Set("roles", []string{"member"})

	id, owner, ok := resolveServerOr404(c, testPool, idA)
	if ok {
		t.Fatal("resolve succeeded against a cancelled context; want failure")
	}
	if id != "" || owner != "" {
		t.Errorf("got (%q, %q); want empty on failure", id, owner)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — a database failure must not be answered as 404, "+
			"or an outage reads as an authorization result", w.Code)
	}
}
