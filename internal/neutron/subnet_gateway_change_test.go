package neutron

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// A subnet's gateway, as upstream governs it (neutron/db/db_base_plugin_v2.py, _validate_subnet).
//
// Nothing checked the value: `gateway_ip` is a VARCHAR, so "banana" was stored as given and then
// handed verbatim to the bridge address, the metadata DNAT and dnsmasq, which took DHCP away from the
// whole network. And a gateway still held by a router interface could be changed or cleared, leaving
// the router's port on an address the subnet no longer advertises. Upstream refuses both: an invalid
// gateway is a 400, and changing one a router port holds is GatewayIpInUse.

// TestSubnetGatewayIsValidated — on create and on update alike, an address of the subnet's IP
// version that is not its network or broadcast address. Lightstack also refuses one outside the
// subnet, which upstream allows: the network's bridge carries the gateway address, and it can only
// carry one on the subnet.
func TestSubnetGatewayIsValidated(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	gw := "10.93.0.1"
	updated := seedNetwork(t, "spec024-gw-update-"+uuid.NewString()[:8])
	subnetID := seedSubnet(t, updated, "spec024-gw-update", "10.93.0.0/24", &gw, 4, false)
	created := seedNetwork(t, "spec024-gw-create-"+uuid.NewString()[:8])
	createBody := func(gateway string) string {
		return fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"10.93.0.0/24","ip_version":4,"gateway_ip":%q}}`, created, gateway)
	}

	for _, tc := range []struct{ name, gateway string }{
		{"not an address", "banana"},
		{"an address with a prefix", "10.93.0.1/24"},
		{"another IP version", "2001:db8::1"},
		{"the network address", "10.93.0.0"},
		{"the broadcast address", "10.93.0.255"},
		{"outside the subnet", "10.99.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, subnet := createSubnet(t, r, hdr, createBody(tc.gateway)); code != http.StatusBadRequest {
				t.Errorf("create: code=%d body=%v, want 400", code, subnet)
			}
			code, raw := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
				fmt.Sprintf(`{"subnet":{"gateway_ip":%q}}`, tc.gateway), hdr)
			if code != http.StatusBadRequest {
				t.Errorf("update: code=%d body=%s, want 400", code, raw)
			}
			if got := storedGateway(t, subnetID); got == nil || *got != gw {
				t.Errorf("stored gateway after a refused update = %s, want %s", derefOr(got, "null"), gw)
			}
		})
	}

	t.Run("a valid gateway is accepted", func(t *testing.T) {
		if code, subnet := createSubnet(t, r, hdr, createBody("10.93.0.254")); code != http.StatusCreated {
			t.Errorf("create: code=%d body=%v, want 201", code, subnet)
		}
		if code, raw := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, `{"subnet":{"gateway_ip":"10.93.0.200"}}`, hdr); code != http.StatusOK {
			t.Errorf("update: code=%d body=%s, want 200", code, raw)
		}
		if got := storedGateway(t, subnetID); got == nil || *got != "10.93.0.200" {
			t.Errorf("stored gateway = %s, want 10.93.0.200", derefOr(got, "null"))
		}
	})
}

// seedRouterPort writes the rows AddRouterInterface writes for a router port holding ip on the
// subnet: the port, carrying the address in its fixed_ips, and its router_interfaces row. It
// returns the generated port id.
func seedRouterPort(t *testing.T, routerID, netID, subnetID, ip string) (portID string) {
	t.Helper()
	portID = uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		VALUES ($1, 'router-interface', $2, $3, $4, 'network:router_interface', 'fa:16:3e:00:00:05', $5::jsonb)`,
		portID, netID, seededProjectID, routerID, fmt.Sprintf(`[{"subnet_id":%q,"ip_address":%q}]`, subnetID, ip))
	mustExec(t, `INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`, routerID, portID, subnetID)
	return portID
}

// TestSubnetGatewayHeldByARouterIsNotChanged — GatewayIpInUse. Upstream refuses to change or clear a
// gateway while a router port holds it (get_alloc_routerports on the current gateway). Here a router
// interface is a port carrying the subnet's gateway in fixed_ips, recorded in router_interfaces —
// which is exactly what the check reads, so the fixture writes the rows AddRouterInterface writes.
func TestSubnetGatewayHeldByARouterIsNotChanged(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	gw := "10.92.0.1"
	netID := seedNetwork(t, "spec024-gw-router-"+uuid.NewString()[:8])
	subnetID := seedSubnet(t, netID, "spec024-gw-router", "10.92.0.0/24", &gw, 4, false)

	routerID := uuid.NewString()
	mustExec(t, `INSERT INTO routers (id, name, project_id) VALUES ($1, 'spec024-gw-router', $2)`, routerID, seededProjectID)
	cleanupRows(t, "routers", routerID)
	portID := seedRouterPort(t, routerID, netID, subnetID, gw)

	for _, tc := range []struct{ name, body string }{
		{"changed", `{"subnet":{"gateway_ip":"10.92.0.254"}}`},
		{"cleared", `{"subnet":{"gateway_ip":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, tc.body, nil)
			if code != http.StatusConflict {
				t.Fatalf("code=%d body=%s, want 409", code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "GatewayIpInUse")
			if got := storedGateway(t, subnetID); got == nil || *got != gw {
				t.Errorf("stored gateway after a refused update = %s, want %s", derefOr(got, "null"), gw)
			}
		})
	}

	t.Run("the same gateway is no change", func(t *testing.T) {
		if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, `{"subnet":{"gateway_ip":"10.92.0.1","name":"renamed"}}`, nil); code != http.StatusOK {
			t.Errorf("code=%d body=%s, want 200", code, body)
		}
	})

	t.Run("free once the router interface is gone", func(t *testing.T) {
		mustExec(t, `DELETE FROM ports WHERE id = $1`, portID) // cascades the router_interfaces row
		if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, `{"subnet":{"gateway_ip":"10.92.0.254"}}`, nil); code != http.StatusOK {
			t.Fatalf("code=%d body=%s, want 200", code, body)
		}
		if got := storedGateway(t, subnetID); got == nil || *got != "10.92.0.254" {
			t.Errorf("stored gateway = %s, want 10.92.0.254", derefOr(got, "null"))
		}
	})
}

// A router port is matched on its whole address: one holding 10.93.0.10 does not hold the
// gateway 10.93.0.1, so the gateway may still change.
func TestSubnetGatewayIsMatchedWholeAgainstRouterPorts(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()
	gw := "10.93.0.1"
	netID := seedNetwork(t, "spec028-gw-prefix-"+uuid.NewString()[:8])
	subnetID := seedSubnet(t, netID, "spec028-gw-prefix", "10.93.0.0/24", &gw, 4, false)
	routerID := uuid.NewString()
	mustExec(t, `INSERT INTO routers (id, name, project_id) VALUES ($1, 'spec028-gw-prefix', $2)`, routerID, seededProjectID)
	cleanupRows(t, "routers", routerID)
	portID := seedRouterPort(t, routerID, netID, subnetID, "10.93.0.10")
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM ports WHERE id = $1", portID) })

	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, `{"subnet":{"gateway_ip":"10.93.0.254"}}`, nil); code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200: no router port holds 10.93.0.1", code, body)
	}
}
