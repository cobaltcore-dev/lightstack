package nova

import (
	"net/http"
	"slices"
	"testing"
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
	for _, s := range serversBody(t, body) {
		if s["id"] == id {
			if names := sgNamesFromResponse(t, s["security_groups"]); !hasName(names, "default") {
				t.Errorf("detail security_groups=%v; want default", names)
			}
		}
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
