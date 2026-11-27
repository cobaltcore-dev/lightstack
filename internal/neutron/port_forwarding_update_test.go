package neutron

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T256 (FR-049): a port forwarding's internal port passes the same checks on an update that moves
// it as on its create, as upstream's pf_plugin: no floating IP of its own (409 PortHasBindingFloatingIP),
// and reached from the floating IP through the router the floating IP records (400). The update checked
// only the port's project and address, and the create took the recorded router without asking whether it
// reaches the new internal port, so the network node DNATed into a network it has no leg on.
func TestPortForwardingInternalPortIsCheckedOnEveryChange(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	_, network := rig.router(t)
	p, x := rig.port(t, network)
	fip, _ := rig.floatingIP(t, "")
	pf := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings",
		forwardingBody(p, x, 2290)), "port_forwarding")
	unreachable := seedNetwork(t, "spec033-t256-"+uuid.NewString()[:6])
	gw := "10.92.0.1"
	seedSubnet(t, unreachable, "spec033-t256", "10.92.0.0/24", &gw, 4, false)
	far, z := rig.port(t, unreachable)
	move := func(port, address string) (int, string) {
		return rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip+"/port_forwardings/"+pf,
			`{"port_forwarding":{"internal_port_id":"`+port+`","internal_ip_address":"`+address+`"}}`)
	}

	t.Run("an update to a port the floating IP's router does not reach", func(t *testing.T) {
		if code, body := move(far, z); code != http.StatusBadRequest {
			t.Errorf("code=%d body=%s; want 400", code, body)
		}
	})
	t.Run("a create to a port the floating IP's router does not reach", func(t *testing.T) {
		code, body := rig.do(t, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(far, z, 2291))
		if code != http.StatusBadRequest {
			t.Errorf("code=%d body=%s; want 400", code, body)
		}
	})
	t.Run("an update to a port with a floating IP of its own", func(t *testing.T) {
		q, y := rig.port(t, network)
		rig.floatingIP(t, q)
		if code, body := move(q, y); code != http.StatusConflict || neutronErrorType(t, body) != "PortHasBindingFloatingIP" {
			t.Errorf("code=%d body=%s; want 409 PortHasBindingFloatingIP", code, body)
		}
	})
}
