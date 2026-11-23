package neutron

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"testing"
)

// SPEC-033 T192 (FR-049): a port forwarding goes on a floating IP no port uses, as upstream's
// (neutron's pf_plugin): its router is found from the internal port's subnet and the floating network, as
// a floating IP's is (get_router_for_floatingip), and recorded on the floating IP in the same
// transaction; the last forwarding's deletion, or its internal port's, frees it again. A floating IP a
// port uses is refused (409 FloatingIPPortAlreadyAssociated), as is an internal port with a floating IP
// of its own (409 PortHasBindingFloatingIP), and a port no router joins to the floating network (400). A
// floating IP its forwardings use cannot be given a port (409 FipInUseByPortForwarding), and a null
// port_id leaves it bound.

func forwardingBody(port, address string, external int) string {
	return `{"port_forwarding":{"internal_port_id":"` + port + `","internal_ip_address":"` + address +
		`","external_port":` + strconv.Itoa(external) + `,"internal_port":22,"protocol":"tcp"}}`
}

// floatingRouter reads the router a floating IP records, "" for none, and its status.
func floatingRouter(t *testing.T, fip string) (string, string) {
	t.Helper()
	var router sql.NullString
	var status string
	if err := testPool.QueryRow(context.Background(), "SELECT router_id::text, COALESCE(status, '') FROM floating_ips WHERE id = $1", fip).Scan(&router, &status); err != nil {
		t.Fatal(err)
	}
	return router.String, status
}

func TestPortForwardingFindsItsRouter(t *testing.T) {
	requireDB(t)

	t.Run("on a floating IP no port uses, through the internal port's router", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, x := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2230))
		if router, status := floatingRouter(t, fip); router != routerID || status != "ACTIVE" {
			t.Errorf("the floating IP's router and status: %q %q; want %s ACTIVE", router, status, routerID)
		}
		if s := rig.held(t, routerID); len(s.PortForwardings) != 1 {
			t.Errorf("the router holds %+v; want the port forwarding", s.PortForwardings)
		}

		// A second forwarding on it keeps the router; deleting one keeps it, the last frees it.
		other, y := rig.port(t, network)
		second := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(other, y, 2231)), "port_forwarding")
		var first string
		if err := testPool.QueryRow(context.Background(), "SELECT id::text FROM port_forwardings WHERE floatingip_id = $1 AND id::text <> $2", fip, second).Scan(&first); err != nil {
			t.Fatal(err)
		}
		rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+fip+"/port_forwardings/"+first, "")
		if router, _ := floatingRouter(t, fip); router != routerID {
			t.Errorf("after one of two forwardings went: router %q; want %s kept", router, routerID)
		}
		rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+fip+"/port_forwardings/"+second, "")
		if router, status := floatingRouter(t, fip); router != "" || status != "DOWN" {
			t.Errorf("after the last forwarding went: router %q, status %q; want none, DOWN", router, status)
		}
	})

	t.Run("the internal port's deletion frees the floating IP", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, x := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2232))
		rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/ports/"+p, "")
		if router, status := floatingRouter(t, fip); router != "" || status != "DOWN" {
			t.Errorf("after the internal port went: router %q, status %q; want none, DOWN", router, status)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, x := rig.port(t, network)
		associated, _ := rig.floatingIP(t, p)
		q, y := rig.port(t, network)
		free, _ := rig.floatingIP(t, "")
		unreachable := seedNetwork(t, "spec033-src20-"+rig.externalPrefix)
		gw := "10.76.0.1"
		seedSubnet(t, unreachable, "spec033-src20", "10.76.0.0/24", &gw, 4, false)
		far, z := rig.port(t, unreachable)
		for _, tc := range []struct {
			name, fip, body string
			status          int
			kind            string
		}{
			{"a floating IP a port uses", associated, forwardingBody(q, y, 2233), http.StatusConflict, "FloatingIPPortAlreadyAssociated"},
			{"an internal port with a floating IP of its own", free, forwardingBody(p, x, 2234), http.StatusConflict, "PortHasBindingFloatingIP"},
			{"an internal port no router joins to the floating network", free, forwardingBody(far, z, 2235), http.StatusBadRequest, "BadRequest"},
		} {
			code, body := rig.do(t, http.MethodPost, "/v2.0/floatingips/"+tc.fip+"/port_forwardings", tc.body)
			if code != tc.status || neutronErrorType(t, body) != tc.kind {
				t.Errorf("%s: code=%d body=%s; want %d %s", tc.name, code, body, tc.status, tc.kind)
			}
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM port_forwardings WHERE floatingip_id::text IN ($1, $2)", associated, free).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d port forwarding(s) after the refusals (%v); want none", n, err)
		}
		if router, _ := floatingRouter(t, free); router != "" {
			t.Errorf("the free floating IP's router after the refusals: %q; want none", router)
		}
	})

	t.Run("a floating IP its forwardings use keeps them", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, x := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2236))
		q, _ := rig.port(t, network)
		code, body := rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":"`+q+`"}}`)
		if code != http.StatusConflict || neutronErrorType(t, body) != "FipInUseByPortForwarding" {
			t.Errorf("associate it with a port: code=%d body=%s; want 409 FipInUseByPortForwarding", code, body)
		}
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":null}}`)
		if router, _ := floatingRouter(t, fip); router != routerID {
			t.Errorf("after a null port_id: router %q; want %s kept", router, routerID)
		}
		if s := rig.held(t, routerID); len(s.PortForwardings) != 1 {
			t.Errorf("the router holds %+v; want the port forwarding kept", s.PortForwardings)
		}
	})
}
