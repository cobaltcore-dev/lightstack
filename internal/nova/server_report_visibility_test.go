package nova

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T262 (FR-014): a server reports the groups of its ports that its viewer can see. An administrator
// may put any project's group on a tenant's server by id (FR-004); the tenant's report leaves that group out,
// as upstream's get_instances_security_groups_bindings skips a port's group the caller cannot list. The
// administrator's report names it.
func TestServerReportNamesOnlyGroupsTheViewerCanSee(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	private := "spec033-t262-" + uuid.NewString()[:6]
	adminOwn := createTestGroup(t, seededProjectID, private)
	server, _ := serverWithPorts(t, r, rec, tenant, 1)
	if code, body := sgAction(t, r, testAuthHdr(seededProjectID, seededUserID), server, "addSecurityGroup", adminOwn); code != http.StatusAccepted {
		t.Fatalf("the administrator's add by id: code=%d body=%s", code, body)
	}
	report := func(hdr map[string]string) []string {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+server, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("show: code=%d body=%s", code, body)
		}
		raw, present := serverBody(t, body)["security_groups"]
		if !present {
			return nil
		}
		return sgNamesFromResponse(t, raw)
	}
	if names := report(tenantHdr(tenant)); hasName(names, private) || !hasName(names, "default") {
		t.Errorf("the tenant's report %v; want default and not the administrator project's group %q", names, private)
	}
	if names := report(testAuthHdr(tenant, seededUserID)); !hasName(names, private) {
		t.Errorf("the administrator's report %v; want it to name %q", names, private)
	}
}

// SPEC-033 T260: a remove the node cannot apply puts nothing back when its group was deleted meanwhile:
// the undo finds no group to restore and leaves the ports as the deletion left them, answering 500 for the
// change it could not apply.
func TestUndoOfARemoveWhoseGroupWasDeleted(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	group := createTestGroup(t, tenant, "spec033-t260-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, tenant, 1)
	if code, body := sgAction(t, r, tenantHdr(tenant), server, "addSecurityGroup", group); code != http.StatusAccepted {
		t.Fatalf("add: code=%d body=%s", code, body)
	}
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
	rec.before = func() { mustExecNova(t, "DELETE FROM security_groups WHERE id = $1", group) }
	rec.failFirst = errors.New("the node could not converge the ports")
	if code, body := sgAction(t, r, tenantHdr(tenant), server, "removeSecurityGroup", group); code != http.StatusInternalServerError {
		t.Fatalf("remove: code=%d body=%s; want 500", code, body)
	}
	var n int
	if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM port_security_groups WHERE port_id = $1 AND security_group_id = $2", ports[0], group).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d port rows for the deleted group (%v); want none", n, err)
	}
}
