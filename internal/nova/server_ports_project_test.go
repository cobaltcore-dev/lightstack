package nova

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// Branch review SRC-8 (2026-11-23-review.md): a server's ports were the ports naming it in device_id,
// whichever project made them, and device_id is any port creator's to set. Another project's port naming
// the server was changed by the server's group actions, could veto them, and showed in the server's
// report. A server's ports are those of its project, as the address map and the self-heal read them.
func TestServerGroupsIgnoreAnotherProjectsPortNamingTheServer(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "src8-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)

	// Another project's port, on its own network, naming the server; its group is stateless, so a
	// stateful one added beside it is a conflict (FR-051).
	other := tenantProject(t)
	foreignGroup := createTestGroup(t, other, "src8-foreign-"+uuid.NewString()[:6])
	if _, err := testPool.Exec(t.Context(), "UPDATE security_groups SET stateful = false WHERE id = $1", foreignGroup); err != nil {
		t.Fatal(err)
	}
	otherNetwork, _ := createTestNetwork(t, other)
	foreign := allocateTestPort(t, rec.Service, otherNetwork, other, server, []string{foreignGroup})

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
	wantStatus(t, "add", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(def, web)) {
		t.Errorf("the server's port: %v; want default and web", got)
	}
	if got := groupsOf(t, foreign); !slices.Equal(got, ids(foreignGroup)) {
		t.Errorf("another project's port naming the server: %v; want its own group only", got)
	}
	if names, _ := reportOf(t, r, seededProjectID, server); !slices.Equal(names, wantNames("default", groupName(t, web))) {
		t.Errorf("report: %v; want default and web, not the other project's group", names)
	}

	code, body = sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
	wantStatus(t, "remove", code, body, http.StatusAccepted)
	if got := groupsOf(t, foreign); !slices.Equal(got, ids(foreignGroup)) {
		t.Errorf("another project's port after the remove: %v; want its own group only", got)
	}
}
