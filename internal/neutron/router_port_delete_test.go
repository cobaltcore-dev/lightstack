package neutron

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T184 (FR-065): a router's interface port and its gateway port go with the
// router's own requests, never through the port API. DELETE /v2.0/ports/{id} answers 409
// ServicePortInUse, administrators included, as upstream's prevent_l3_port_deletion, and nothing
// changes. A port is a router's by the router's records (SPEC-033 T170): one that only names a
// router as its owner and device, or the gateway port of a router that is gone, is deleted as any port.

func TestRouterPortsAreNotDeletedThroughThePortAPI(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rr := newRouterRig(t)
	code, body, router := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create router: code=%d body=%s", code, body)
	}
	gw := "10.95.0.1"
	internal := seedNetwork(t, "spec033-src12-"+uuid.NewString()[:6])
	subnet := seedSubnet(t, internal, "spec033-src12", "10.95.0.0/24", &gw, 4, false)
	if code, body := doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+router+"/add_router_interface",
		`{"subnet_id":"`+subnet+`"}`, rr.member); code != http.StatusOK {
		t.Fatalf("add interface: code=%d body=%s", code, body)
	}
	var iface string
	if err := testPool.QueryRow(ctx, "SELECT port_id::text FROM router_interfaces WHERE router_id = $1", router).Scan(&iface); err != nil {
		t.Fatal(err)
	}
	gateways := gatewayPorts(t, router)
	if len(gateways) != 1 {
		t.Fatalf("gateway ports %+v; want one", gateways)
	}
	gateway := gateways[0].id

	for _, tc := range []struct {
		name, port, owner string
		hdr               map[string]string
	}{
		{"a member, the interface's", iface, ownerRouterInterface, rr.member},
		{"an administrator, the interface's", iface, ownerRouterInterface, rr.admin},
		{"an administrator, the gateway's", gateway, ownerRouterGateway, rr.admin},
	} {
		code, body := doReq(rr.r, http.MethodDelete, "/v2.0/ports/"+tc.port, tc.hdr)
		if code != http.StatusConflict || neutronErrorType(t, body) != "ServicePortInUse" || !strings.Contains(body, "has device owner "+tc.owner) {
			t.Errorf("%s: code=%d body=%s; want 409 ServicePortInUse naming %s", tc.name, code, body, tc.owner)
		}
	}
	if code, body := doReq(rr.r, http.MethodDelete, "/v2.0/ports/"+gateway, rr.member); code != http.StatusNotFound {
		t.Errorf("a member, the gateway's: code=%d body=%s; want 404, a port of no project", code, body)
	}
	var interfaces int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM router_interfaces WHERE router_id = $1 AND port_id::text = $2", router, iface).Scan(&interfaces); err != nil || interfaces != 1 {
		t.Errorf("%d interface row(s) after the refusals (%v); want the one", interfaces, err)
	}
	if after := gatewayPorts(t, router); len(after) != 1 || after[0].id != gateway {
		t.Errorf("gateway ports after the refusals %+v; want %s", after, gateway)
	}

	// Ports the router's records do not hold are ordinary ports.
	claimed, claimedGateway, stale := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, c := range []struct{ id, owner, mac string }{
		{claimed, ownerRouterInterface, "fa:16:3e:12:00:01"},
		{claimedGateway, ownerRouterGateway, "fa:16:3e:12:00:03"},
	} {
		mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
			VALUES ($1, 'spec033-src12-claim', $2, $3, $4, $5, $6, 'DOWN', '[]'::jsonb)`,
			c.id, internal, seededProjectID, router, c.owner, c.mac)
	}
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, '', $2, NULL, $3, 'network:router_gateway', 'fa:16:3e:12:00:02', 'DOWN', '[]'::jsonb)`,
		stale, rr.external, uuid.NewString())
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM ports WHERE id::text = ANY($1)", []string{claimed, claimedGateway, stale})
	})
	for _, port := range []string{claimed, claimedGateway} {
		if code, body := doReq(rr.r, http.MethodDelete, "/v2.0/ports/"+port, rr.member); code != http.StatusNoContent {
			t.Errorf("a member's port naming the router without its record: code=%d body=%s; want 204", code, body)
		}
	}
	if code, body := doReq(rr.r, http.MethodDelete, "/v2.0/ports/"+stale, rr.admin); code != http.StatusNoContent {
		t.Errorf("the gateway port of a router that is gone: code=%d body=%s; want 204", code, body)
	}
}
