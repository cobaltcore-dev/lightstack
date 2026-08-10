package nova

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// SPEC-023 US1: the shared owner-or-administrator lookup for server operations.
//
// These encode contracts/compute-override.md C1, C2 and C6, and research R10's
// disclosure rule. The subtle one is C6: an administrator may address another
// project's instance by UUID but NOT by name, because O3K accepts a name where
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
		if _, err := database.DB.Exec(ctx,
			`INSERT INTO projects (id, name, description, enabled, domain_id)
			 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))
			 ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
			t.Fatalf("seeding project %s: %v", name, err)
		}
	}
	// Same instance name in both projects — the collision C6 guards against.
	for _, p := range []string{authzProjectA, authzProjectB} {
		if _, err := database.DB.Exec(ctx,
			`DELETE FROM instances WHERE project_id = $1 AND name = 'web-1'`, p); err != nil {
			t.Fatalf("clearing instances for %s: %v", p, err)
		}
	}
	// user_id/flavor_id must be set: the detail listing scans them into plain
	// strings, so a NULL makes the row silently dropped from the response.
	if err := database.DB.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('web-1', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
		authzProjectA, seededUserID).Scan(&idA); err != nil {
		t.Fatalf("seeding instance in A: %v", err)
	}
	if err := database.DB.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('web-1', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE') RETURNING id::text`,
		authzProjectB, seededUserID).Scan(&idB); err != nil {
		t.Fatalf("seeding instance in B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.DB.Exec(context.Background(),
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

	id, owner, err := targetInstance(authzCtx(authzProjectA, true), idB)
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

	id, owner, err := targetInstance(authzCtx(authzProjectA, false), idA)
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

	if _, _, err := targetInstance(authzCtx(authzProjectA, false), idB); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("member in A resolving B's instance: err = %v; want pgx.ErrNoRows", err)
	}
}

// TestTargetInstance_ForeignAndAbsentAreIndistinguishable: research R10 and
// FR-003. A caller must not be able to tell "someone else's" from "does not
// exist" — a distinguishable answer confirms the instance exists.
func TestTargetInstance_ForeignAndAbsentAreIndistinguishable(t *testing.T) {
	requireDB(t)
	_, idB := seedAuthzFixtures(t)

	_, _, foreignErr := targetInstance(authzCtx(authzProjectA, false), idB)
	_, _, absentErr := targetInstance(authzCtx(authzProjectA, false),
		"11111111-1111-1111-1111-111111111111")

	if !errors.Is(foreignErr, pgx.ErrNoRows) || !errors.Is(absentErr, pgx.ErrNoRows) {
		t.Fatalf("foreign=%v absent=%v; both must be pgx.ErrNoRows", foreignErr, absentErr)
	}
}

// TestTargetInstance_AdminNameStaysInOwnProject: C6 / FR-005c — the case that
// could otherwise delete the wrong tenant's server. Both projects own an
// instance named "web-1"; an admin scoped to A addressing "web-1" must get A's,
// never B's and never an arbitrary choice.
func TestTargetInstance_AdminNameStaysInOwnProject(t *testing.T) {
	requireDB(t)
	idA, idB := seedAuthzFixtures(t)

	id, owner, err := targetInstance(authzCtx(authzProjectA, true), "web-1")
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
	if err := database.DB.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, status) VALUES ('only-in-b', $1, 'ACTIVE') RETURNING id::text`,
		authzProjectB).Scan(&onlyB); err != nil {
		t.Fatalf("seeding only-in-b: %v", err)
	}

	if _, _, err := targetInstance(authzCtx(authzProjectA, true), "only-in-b"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("admin in A resolving name 'only-in-b' (owned by B): err = %v; want pgx.ErrNoRows", err)
	}
}

// TestTargetInstance_MalformedRefIsNotFound: a reference that is neither a UUID
// nor an existing name must be a clean no-rows, not a Postgres cast error
// surfacing as a 500. This is the shape #515 fixed for identity ids.
func TestTargetInstance_MalformedRefIsNotFound(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t)

	for _, ref := range []string{"not-a-uuid", "'; DROP TABLE instances; --", ""} {
		_, _, err := targetInstance(authzCtx(authzProjectA, true), ref)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("targetInstance(%q) = %v; want pgx.ErrNoRows", ref, err)
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
	if err := database.DB.QueryRow(context.Background(),
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
	if err := database.DB.QueryRow(context.Background(),
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
	if err := database.DB.QueryRow(context.Background(),
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
// separates pgx.ErrNoRows from every other error, and that distinction is
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

	id, owner, ok := resolveServerOr404(c, idA)
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
