package neutron

import (
	"context"
	"net/http"
	"testing"
)

// SPEC-033 T257 (FR-070): router:external cannot be unset while a router's gateway port is on the network,
// as upstream's _process_l3_update (409 ExternalNetworkInUse). Routers kept a gateway, and floating IPs an
// address, on a network that was no longer external. Without a gateway port the flag may go.
func TestExternalCannotBeUnsetUnderAGateway(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	unset := func() (int, string) {
		return doJSONReq(rr.r, http.MethodPut, "/v2.0/networks/"+rr.external, `{"network":{"router:external":false}}`, rr.admin)
	}
	external := func() bool {
		var ext bool
		if err := testPool.QueryRow(context.Background(), "SELECT external FROM networks WHERE id = $1", rr.external).Scan(&ext); err != nil {
			t.Fatal(err)
		}
		return ext
	}
	code, body, router := rr.createRouter(t, rr.admin, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create the router: code=%d body=%s", code, body)
	}
	want := "External network " + rr.external + " cannot be updated to be made non-external, since it has existing gateway ports."
	if code, body := unset(); code != http.StatusConflict || neutronErrorType(t, body) != "ExternalNetworkInUse" ||
		neutronErrorMessage(t, body) != want {
		t.Errorf("unset under a gateway: code=%d body=%s; want 409 ExternalNetworkInUse %q", code, body, want)
	}
	if !external() {
		t.Error("the network is no longer external after the refused update")
	}
	if code, body := rr.updateGateway(t, rr.admin, router, "null"); code != http.StatusOK {
		t.Fatalf("clear the gateway: code=%d body=%s", code, body)
	}
	if code, body := unset(); code != http.StatusOK || external() {
		t.Errorf("unset with no gateway: code=%d body=%s; want 200 and the flag gone", code, body)
	}
}
