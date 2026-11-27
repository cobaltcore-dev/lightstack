package nova

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T022 (FR-013; contracts/compute-security-groups.md, interface attach): a port attached on
// a network gets the project's default group, created if missing, and the server's other groups are
// not copied, as upstream. A port attached by id keeps its own groups. Detaching a port leaves its
// groups on it, and the server stops reporting them.

// freePortWithGroups inserts a port on networkID in projectID that no device uses, carrying groupIDs.
func freePortWithGroups(t *testing.T, networkID, projectID string, groupIDs ...string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, $2, $3, $4, 'fa:16:3e:33:22:01', 'DOWN', '[]'::jsonb)`,
		id, "spec033-free-"+id[:8], networkID, projectID); err != nil {
		t.Fatalf("seed a free port: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM ports WHERE id = $1", id) })
	setGroups(t, id, groupIDs...)
	return id
}

// attachInterface attaches an interface and returns the attached port's id.
func attachInterface(t *testing.T, r *gin.Engine, hdr map[string]string, serverID, body string) string {
	t.Helper()
	code, resp := doReq(r, http.MethodPost, "/v2.1/servers/"+serverID+"/os-interface", body, hdr)
	if code != http.StatusOK {
		t.Fatalf("attach interface %s: code=%d body=%s; want 200", body, code, resp)
	}
	var out struct {
		InterfaceAttachment struct {
			PortID string `json:"port_id"`
		} `json:"interfaceAttachment"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.InterfaceAttachment.PortID == "" {
		t.Fatalf("attach interface: no port id in %s (%v)", resp, err)
	}
	return out.InterfaceAttachment.PortID
}

func TestInterfaceAttach_SecurityGroups(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t) // no default group yet: the attach on a network creates it
	hdr := tenantHdr(tenant)
	web := createTestGroup(t, tenant, "spec033-web-"+uuid.NewString()[:6])
	db := createTestGroup(t, tenant, "spec033-db-"+uuid.NewString()[:6])
	// Created with web: a create without groups would ensure default itself (FR-006a).
	net1, _ := createTestNetwork(t, tenant)
	server := createTestServer(t, r, tenant,
		`{"server":{"name":"spec033-attach","flavorRef":"m1.small","security_groups":[{"name":"`+groupName(t, web)+`"}]}}`)
	first := allocateTestPort(t, rec.Service, net1, tenant, server, []string{web})
	if securityGroupExists(t, "default", tenant) {
		t.Fatal("precondition: the tenant already has a default group")
	}

	// On a second network: the new port gets default only, created for the project.
	net2, _ := createTestNetwork(t, tenant)
	attached := attachInterface(t, r, hdr, server, `{"interfaceAttachment":{"net_id":"`+net2+`"}}`)
	def := defaultGroup(t, tenant)
	if got := groupsOf(t, attached); !slices.Equal(got, []string{def}) {
		t.Errorf("the port attached on a network: %v; want default only (%s)", got, def)
	}
	if names, _ := reportOf(t, r, tenant, server); !slices.Equal(names, wantNames(groupName(t, web), "default")) {
		t.Errorf("report: %v; want web and default", names)
	}

	// By port id: the port keeps its own groups.
	free := freePortWithGroups(t, net2, tenant, db)
	if got := attachInterface(t, r, hdr, server, `{"interfaceAttachment":{"port_id":"`+free+`"}}`); got != free {
		t.Fatalf("attached port %s; want %s", got, free)
	}
	if got := groupsOf(t, free); !slices.Equal(got, []string{db}) {
		t.Errorf("the port attached by id: %v; want db, unchanged", got)
	}
	if names, _ := reportOf(t, r, tenant, server); !hasName(names, groupName(t, db)) {
		t.Errorf("report: %v; want it to include db", names)
	}

	// Removing web changes only the port that carried it.
	code, body := sgAction(t, r, hdr, server, "removeSecurityGroup", groupName(t, web))
	wantStatus(t, "remove web", code, body, http.StatusAccepted)
	if got := groupsOf(t, first); len(got) != 0 {
		t.Errorf("the first port: %v; want none", got)
	}
	if got := groupsOf(t, attached); !slices.Equal(got, []string{def}) {
		t.Errorf("the port attached on a network: %v; want default, unchanged", got)
	}
	if got := groupsOf(t, free); !slices.Equal(got, []string{db}) {
		t.Errorf("the port attached by id: %v; want db, unchanged", got)
	}
	if calls := rec.recorded(); len(calls) != 1 || !slices.Equal(calls[0], []string{first}) {
		t.Errorf("RefreshPortFilters calls: %v; want one with the first port only", calls)
	}

	// Detaching the port leaves its groups on it; the server stops reporting them.
	code, body = doReq(r, http.MethodDelete, "/v2.1/servers/"+server+"/os-interface/"+free, "", hdr)
	wantStatus(t, "detach the port attached by id", code, body, http.StatusAccepted)
	if names, _ := reportOf(t, r, tenant, server); hasName(names, groupName(t, db)) {
		t.Errorf("report after the detach: %v; want no db", names)
	}
	if got := groupsOf(t, free); !slices.Equal(got, []string{db}) {
		t.Errorf("the detached port: %v; want db kept", got)
	}
}
