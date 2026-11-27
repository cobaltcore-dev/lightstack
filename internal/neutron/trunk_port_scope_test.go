package neutron

import (
	"net/http"
	"testing"
)

// SPEC-033 T258 (FR-073): a trunk's parent port and its subports are ports the caller can see — their
// project's, any project's for an administrator — as upstream's trunk validator reads them in the caller's
// context (404 otherwise). Any port id was stored: a member took another project's port as a trunk's parent,
// the owner then got 500 making it one of their own, and the answers told a real id from a random one.
func TestTrunkTakesOnlyPortsTheCallerCanSee(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	theirNet := seedBareNetwork(t, otherProjectID)
	theirPort := seedBarePort(t, theirNet, otherProjectID)
	ourNet := seedBareNetwork(t, seededProjectID)
	ourPort := seedBarePort(t, ourNet, seededProjectID)
	t.Cleanup(func() { mustExec(t, "DELETE FROM trunks WHERE port_id IN ($1, $2)", theirPort, ourPort) })

	if code, body := doJSONReq(r, http.MethodPost, "/v2.0/trunks", `{"trunk":{"port_id":"`+theirPort+`"}}`, member); code != http.StatusNotFound {
		t.Errorf("a trunk on another project's port: code=%d body=%s; want 404", code, body)
	}
	code, body := doJSONReq(r, http.MethodPost, "/v2.0/trunks", `{"trunk":{"port_id":"`+ourPort+`"}}`, member)
	if code != http.StatusCreated {
		t.Fatalf("a trunk on the caller's port: code=%d body=%s; want 201", code, body)
	}
	trunk := createdID(t, body, "trunk")
	for subport, want := range map[string]int{theirPort: http.StatusNotFound, "not-a-uuid": http.StatusBadRequest} {
		if code, body := doJSONReq(r, http.MethodPut, "/v2.0/trunks/"+trunk+"/add_subports",
			`{"sub_ports":[{"port_id":"`+subport+`","segmentation_type":"vlan","segmentation_id":100}]}`, member); code != want {
			t.Errorf("subport %s: code=%d body=%s; want %d", subport, code, body, want)
		}
	}
}
