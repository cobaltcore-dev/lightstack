package nova

import (
	"net/http"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
)

func hasName(names []string, want string) bool {
	return slices.Contains(names, want)
}

// US2 (FR-004..FR-007, FR-005a, FR-006a): security_groups are persisted at create,
// read back on show + list-detail, default-synthesized when empty, orphans dropped,
// unknown non-default names rejected, and the project default ensured on demand.

func TestServerDetail_SecurityGroups_RoundTrip(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sg1","flavorRef":"m1.small","security_groups":[{"name":"default"}]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); !hasName(names, "default") {
		t.Errorf("show security_groups=%v; want to contain default", names)
	}

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/detail", "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s", code, body)
	}
	found := false
	for _, s := range serversBody(t, body) {
		if s["id"] == id {
			found = true
			if names := sgNamesFromResponse(t, s["security_groups"]); !hasName(names, "default") {
				t.Errorf("detail security_groups=%v; want default", names)
			}
		}
	}
	if !found {
		t.Errorf("detail: server %s not found in list", id)
	}
}

func TestServerDetail_SecurityGroups_DefaultWhenNone(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgnone","flavorRef":"m1.small"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); len(names) != 1 || names[0] != "default" {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

func TestServerDetail_SecurityGroups_Multiple(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	createTestSecurityGroup(t, "spec017-sg2", seededProjectID)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgmulti","flavorRef":"m1.small","security_groups":[{"name":"default"},{"name":"spec017-sg2"}]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); !hasName(names, "default") || !hasName(names, "spec017-sg2") {
		t.Errorf("show security_groups=%v; want default + spec017-sg2", names)
	}
}

func TestServerDetail_SecurityGroups_OrphanDropped(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	sgID := createTestSecurityGroup(t, "spec017-sgorphan", seededProjectID)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgorph","flavorRef":"m1.small","security_groups":[{"name":"spec017-sgorphan"}]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)

	// Delete the SG row directly → the server_security_groups row is now orphaned
	// (no FK on security_group_id); the INNER JOIN read-back must drop it.
	deleteSecurityGroupByID(t, sgID)

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"])
	if hasName(names, "spec017-sgorphan") {
		t.Errorf("show security_groups=%v; orphaned group should be dropped", names)
	}
	if len(names) != 1 || names[0] != "default" {
		t.Errorf("show security_groups=%v; want [default] after orphan drop", names)
	}
}

// FR-005a: unknown non-default SG rejected with 400, no instance created.
func TestCreateServer_UnknownSecurityGroup_400(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgbad","flavorRef":"m1.small","security_groups":[{"name":"spec017-nope"}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create with unknown SG: code=%d body=%s; want 400", code, body)
	}
	if n := instanceCount(t, "spec017-sgbad", seededProjectID); n != 0 {
		t.Errorf("instance created despite 400: count=%d", n)
	}
}

// FR-006a: explicit ["default"] succeeds in a project with no seeded default SG —
// the default is ensured (auto-created) on demand.
func TestCreateServer_EnsureDefault_FreshProject(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	createTestProject(t, freshProjectID, "spec017-proj")
	if securityGroupExists(t, "default", freshProjectID) {
		t.Fatalf("precondition failed: fresh project already has a default SG")
	}

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, freshProjectID, seededUserID,
		`{"server":{"name":"spec017-fresh","flavorRef":"m1.small","security_groups":[{"name":"default"}]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create in fresh project: code=%d body=%s; want 202 (ensure default)", code, body)
	}
	if !securityGroupExists(t, "default", freshProjectID) {
		t.Errorf("default SG was not ensured for the fresh project")
	}
	id, _ := serverBody(t, body)["id"].(string)
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(freshProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); len(names) != 1 || names[0] != "default" {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

// FR-006 / FR-006a on the implicit path: a create with NO security_groups in a fresh
// project must ensure the project's default group and persist the association, so
// the port allocated at boot can attach it and the read-back is a real association
// rather than a synthesized one.
func TestCreateServer_ImplicitDefault_FreshProject(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	createTestProject(t, freshProjectID, "spec017-proj")
	if securityGroupExists(t, "default", freshProjectID) {
		t.Fatalf("precondition failed: fresh project already has a default SG")
	}

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, freshProjectID, seededUserID,
		`{"server":{"name":"spec017-implicit","flavorRef":"m1.small"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("implicit create in fresh project: code=%d body=%s; want 202", code, body)
	}
	if !securityGroupExists(t, "default", freshProjectID) {
		t.Errorf("default SG was not ensured on the implicit path")
	}
	id, _ := serverBody(t, body)["id"].(string)
	if n := serverSecurityGroupCount(t, id); n != 1 {
		t.Errorf("server_security_groups rows=%d; want 1 (default persisted, not synthesized)", n)
	}
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(freshProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); len(names) != 1 || names[0] != "default" {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

// Upstream parity for degenerate items: {"name":""} fails schema validation (400,
// no instance); {} carries no name and is ignored, so the server gets `default`.
func TestCreateServer_EmptySecurityGroupName_400(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgempty","flavorRef":"m1.small","security_groups":[{"name":""}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create with empty SG name: code=%d body=%s; want 400", code, body)
	}
	if n := instanceCount(t, "spec017-sgempty", seededProjectID); n != 0 {
		t.Errorf("instance created despite 400: count=%d", n)
	}
}

func TestCreateServer_NamelessSecurityGroupItem_Ignored(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-sgnameless","flavorRef":"m1.small","security_groups":[{}]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create with nameless SG item: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if names := sgNamesFromResponse(t, serverBody(t, body)["security_groups"]); len(names) != 1 || names[0] != "default" {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

// neutron.EnsureDefaultSecurityGroup seeds rules only for a group it created; an
// existing `default` — even one with no rules — is left exactly as found.
func TestEnsureDefaultSecurityGroup_SeedsOnlyWhenCreated(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	ctx := t.Context()

	createTestProject(t, freshProjectID, "spec017-proj")
	id, err := neutron.EnsureDefaultSecurityGroup(ctx, freshProjectID)
	if err != nil {
		t.Fatalf("ensure (fresh): %v", err)
	}
	if n := ruleCountForSG(t, id); n != 3 {
		t.Errorf("fresh default: rules=%d; want 3 seeded", n)
	}

	// Second call on the now-existing group: still 3, and the same id.
	id2, err := neutron.EnsureDefaultSecurityGroup(ctx, freshProjectID)
	if err != nil || id2 != id {
		t.Fatalf("ensure (again): id=%s err=%v; want same id %s", id2, err, id)
	}
	if n := ruleCountForSG(t, id); n != 3 {
		t.Errorf("re-ensure: rules=%d; want 3 (unchanged)", n)
	}

	// Operator empties the group: ensure must NOT restore the rules.
	if _, err := database.DB.Exec(ctx, "DELETE FROM security_group_rules WHERE security_group_id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := neutron.EnsureDefaultSecurityGroup(ctx, freshProjectID); err != nil {
		t.Fatalf("ensure (emptied): %v", err)
	}
	if n := ruleCountForSG(t, id); n != 0 {
		t.Errorf("emptied default was re-seeded: rules=%d; want 0", n)
	}
}
