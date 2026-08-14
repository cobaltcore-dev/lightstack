package neutron

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// Router writes used to act before anything confirmed the caller owns the router, and two of them
// never confirmed it at all. UpdateRouter configured the gateway's egress SNAT ahead of an owner-scoped
// UPDATE whose row count nobody read; RemoveRouterInterface looked up, detached and deleted the
// interface with no owner predicate anywhere; DeleteRouter removed the namespace before its
// owner-scoped DELETE and answered 204 whatever that matched. A project that knew another's router id
// could give that router's subnets egress, take its interfaces away, or delete its namespace.
//
// And egress SNAT, once installed, was never withdrawn: not when the gateway was cleared — which the
// API could not even express — not when SNAT was turned off, and not when an interface went.

// routerWithInterface seeds a router owned by project with an interface on a fresh subnet of range
// cidr: the rows AddRouterInterface writes. It returns the router, subnet and port ids.
func routerWithInterface(t *testing.T, project, cidr string) (routerID, subnetID, portID string) {
	t.Helper()
	routerID = seedRouter(t, "spec024-router-"+uuid.NewString()[:8], project)
	netID := seedExternalNetwork(t, "spec024-router-net-"+uuid.NewString()[:8], project, false, false)
	subnetID, portID = uuid.NewString(), uuid.NewString()
	mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, 'spec024-router-sub', $2, $3, $4, NULL, 4, false, now(), now())`, subnetID, netID, project, cidr)
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		VALUES ($1, 'router-interface', $2, $3, $4, 'network:router_interface', 'fa:16:3e:00:00:06', '[]'::jsonb)`,
		portID, netID, project, routerID)
	mustExec(t, `INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`, routerID, portID, subnetID)
	return routerID, subnetID, portID
}

// TestRouterWritesByAnotherProjectAreNotFound — every write to another project's router is a 404 and
// changes nothing, as for any resource the caller cannot see.
func TestRouterWritesByAnotherProjectAreNotFound(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newEnvelopeRouter() // the caller is the seeded project
	routerID, subnetID, portID := routerWithInterface(t, otherProjectID, "10.86.0.0/24")
	ext := seedExternalNetwork(t, "spec024-router-ext-"+uuid.NewString()[:8], seededProjectID, false, true)

	for _, tc := range []struct{ name, method, path, body string }{
		{"rename", http.MethodPut, "/v2.0/routers/" + routerID, `{"router":{"name":"spec024-hijacked"}}`},
		{"set a gateway", http.MethodPut, "/v2.0/routers/" + routerID, fmt.Sprintf(`{"router":{"external_gateway_info":{"network_id":%q}}}`, ext)},
		{"clear the gateway", http.MethodPut, "/v2.0/routers/" + routerID, `{"router":{"external_gateway_info":{}}}`},
		{"remove an interface", http.MethodPut, "/v2.0/routers/" + routerID + "/remove_router_interface", fmt.Sprintf(`{"subnet_id":%q}`, subnetID)},
		{"delete", http.MethodDelete, "/v2.0/routers/" + routerID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReqBody(r, tc.method, tc.path, tc.body, nil)
			if code != http.StatusNotFound {
				t.Fatalf("code=%d body=%s, want 404", code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "RouterNotFound")
		})
	}

	var name string
	var gateway *string
	if err := testPool.QueryRow(context.Background(),
		"SELECT name, external_gateway_info::text FROM routers WHERE id = $1", routerID).Scan(&name, &gateway); err != nil {
		t.Fatalf("read router: %v", err)
	}
	if name == "spec024-hijacked" || gateway != nil {
		t.Errorf("another project changed the router: name=%q gateway=%v", name, derefOr(gateway, "null"))
	}
	var interfaces, ports int
	if err := testPool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM router_interfaces WHERE router_id = $1), (SELECT count(*) FROM ports WHERE id = $2)`,
		routerID, portID).Scan(&interfaces, &ports); err != nil {
		t.Fatalf("read interface: %v", err)
	}
	if interfaces != 1 || ports != 1 {
		t.Errorf("another project changed the interface: %d interface row(s), %d port(s), want 1 and 1", interfaces, ports)
	}
}

// TestRouterWritesWithAnIDThatIsNotAUUIDAreNotFound — such an id matches no router; it used to reach a
// uuid comparison and come back as a 500, or as a panic on the device-name slice.
func TestRouterWritesWithAnIDThatIsNotAUUIDAreNotFound(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	for _, tc := range []struct{ name, method, path, body string }{
		{"update", http.MethodPut, "/v2.0/routers/not-a-uuid", `{"router":{"name":"x"}}`},
		{"remove an interface", http.MethodPut, "/v2.0/routers/not-a-uuid/remove_router_interface", fmt.Sprintf(`{"subnet_id":%q}`, absentID)},
		{"delete", http.MethodDelete, "/v2.0/routers/not-a-uuid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := doReqBody(r, tc.method, tc.path, tc.body, nil); code != http.StatusNotFound {
				t.Errorf("code=%d body=%s, want 404", code, body)
			}
		})
	}
}

// TestRemoveRouterInterfaceNeedsASubnetOrAPort — upstream's "Either subnet_id or port_id must be
// specified". With neither, the handler used to slice an empty port id and panic.
func TestRemoveRouterInterfaceNeedsASubnetOrAPort(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	routerID := seedRouter(t, "spec024-router-noargs-"+uuid.NewString()[:8], seededProjectID)
	if code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("code=%d body=%s, want 400", code, body)
	}
}

// TestRemoveRouterInterfaceTakesTheInterface — the owner's removal, by subnet and by port, deletes the
// interface's port and with it the router_interfaces row.
func TestRemoveRouterInterfaceTakesTheInterface(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	for _, by := range []string{"subnet_id", "port_id"} {
		t.Run("by "+by, func(t *testing.T) {
			routerID, subnetID, portID := routerWithInterface(t, seededProjectID, "10.85.0.0/24")
			ref := map[string]string{"subnet_id": subnetID, "port_id": portID}[by]
			code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface",
				fmt.Sprintf(`{%q:%q}`, by, ref), nil)
			if code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want 200", code, body)
			}
			var interfaces, ports int
			if err := testPool.QueryRow(context.Background(), `
				SELECT (SELECT count(*) FROM router_interfaces WHERE router_id = $1), (SELECT count(*) FROM ports WHERE id = $2)`,
				routerID, portID).Scan(&interfaces, &ports); err != nil {
				t.Fatalf("read interface: %v", err)
			}
			if interfaces != 0 || ports != 0 {
				t.Errorf("after removal: %d interface row(s), %d port(s), want none", interfaces, ports)
			}
		})
	}
}

// TestDeletingARouterWithInterfacesIsRouterInUse — upstream's class for the refusal.
func TestDeletingARouterWithInterfacesIsRouterInUse(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	routerID, _, _ := routerWithInterface(t, seededProjectID, "10.84.0.0/24")
	code, body := doReqBody(r, http.MethodDelete, "/v2.0/routers/"+routerID, "", nil)
	if code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", code, body)
	}
	assertNeutronShape(t, neutronError(t, body), "RouterInUse")
}

// TestRouterGatewayCanBeCleared — `external_gateway_info` set to an empty object or to null clears the
// gateway, as upstream's dict_or_nodata accepts both; `openstack router unset --external-gateway` sends
// the former. It used to be refused (the empty object) or ignored (null).
func TestRouterGatewayCanBeCleared(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	ext := seedExternalNetwork(t, "spec024-router-clear-ext-"+uuid.NewString()[:8], seededProjectID, false, true)
	routerID := seedRouter(t, "spec024-router-clear-"+uuid.NewString()[:8], seededProjectID)
	setGateway := fmt.Sprintf(`{"router":{"external_gateway_info":{"network_id":%q}}}`, ext)

	for _, clear := range []string{`{}`, `null`} {
		t.Run(clear, func(t *testing.T) {
			if code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID, setGateway, hdr); code != http.StatusOK {
				t.Fatalf("set gateway: code=%d body=%s", code, body)
			}
			code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID,
				`{"router":{"external_gateway_info":`+clear+`}}`, hdr)
			if code != http.StatusOK {
				t.Fatalf("clear gateway: code=%d body=%s, want 200", code, body)
			}
			var gateway *string
			if err := testPool.QueryRow(context.Background(),
				"SELECT external_gateway_info::text FROM routers WHERE id = $1", routerID).Scan(&gateway); err != nil {
				t.Fatalf("read router: %v", err)
			}
			if gateway != nil {
				t.Errorf("stored gateway after clearing = %s, want NULL", *gateway)
			}
		})
	}
}

// TestRouterGatewayEnableSNATMustBeABoolean — it is stored as given and read back as a SNAT decision,
// so a value that is not a boolean is refused, as upstream's validator does.
func TestRouterGatewayEnableSNATMustBeABoolean(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	ext := seedExternalNetwork(t, "spec024-router-snat-ext-"+uuid.NewString()[:8], seededProjectID, false, true)
	routerID := seedRouter(t, "spec024-router-snat-"+uuid.NewString()[:8], seededProjectID)
	code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID,
		fmt.Sprintf(`{"router":{"external_gateway_info":{"network_id":%q,"enable_snat":"no"}}}`, ext), hdr)
	if code != http.StatusBadRequest {
		t.Errorf("code=%d body=%s, want 400", code, body)
	}
}

// TestEgressSNATIsWithdrawnOnlyWhenNoRouterStillNeedsIt — the MASQUERADE rule is per range, not per
// router, and two projects may well both use the same range. A range is withdrawn only once no router
// with SNAT on still has an interface on a subnet of that range.
func TestEgressSNATIsWithdrawnOnlyWhenNoRouterStillNeedsIt(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := &Service{pool: testPool, mode: "iptables"}
	ext := seedExternalNetwork(t, "spec024-snat-ext-"+uuid.NewString()[:8], seededProjectID, false, true)
	gateway := func(routerID, info string) {
		mustExec(t, `UPDATE routers SET external_gateway_info = $2::jsonb WHERE id = $1`, routerID, info)
	}
	on := fmt.Sprintf(`{"network_id":%q}`, ext)
	off := fmt.Sprintf(`{"network_id":%q,"enable_snat":false}`, ext)

	a, _, _ := routerWithInterface(t, seededProjectID, "10.87.0.0/24")
	b, _, _ := routerWithInterface(t, seededProjectID, "10.87.0.0/24")
	gateway(a, on)
	gateway(b, on)
	stale := func() []string {
		got, err := svc.egressSNATNoLongerNeeded(ctx, []string{"10.87.0.0/24"})
		if err != nil {
			t.Fatalf("egressSNATNoLongerNeeded: %v", err)
		}
		return got
	}

	if got := stale(); len(got) != 0 {
		t.Errorf("both routers egress, yet %v would be withdrawn", got)
	}
	mustExec(t, `UPDATE routers SET external_gateway_info = NULL WHERE id = $1`, a)
	if got := stale(); len(got) != 0 {
		t.Errorf("the other router still egresses on the range, yet %v would be withdrawn", got)
	}
	gateway(b, off)
	if got := stale(); !slices.Equal(got, []string{"10.87.0.0/24"}) {
		t.Errorf("no router needs the range any more, yet the withdrawal is %v", got)
	}
}
