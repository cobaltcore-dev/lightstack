package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T274 (FR-069): add_router_interface takes a port, as upstream's _add_interface_by_port. The port
// must be one the caller can see (404), attached to nothing (409 PortInUse), with a fixed IP and at most one
// IPv4 subnet (400), on a subnet the router has no port on (400); it becomes the router's interface with its
// own address, and a removal deletes it, as upstream's. Naming both a subnet and a port, or neither, is 400.
// Horizon's Add Interface with an IP address goes this way: it creates the port, then adds it.
func TestRouterInterfaceByPort(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	ctx := context.Background()
	network := seedNetwork(t, "spec033-t274-"+uuid.NewString()[:6])
	subnet := seedProjectSubnet(t, seededProjectID, network, "10.74.0.0/24", "10.74.0.1")
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", network) })
	_, _, router := rr.createRouter(t, rr.member, "")
	add := func(hdr map[string]string, body string) (int, string) {
		return doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+router+"/add_router_interface", body, hdr)
	}
	port := func(t *testing.T, fixed string) string {
		t.Helper()
		_, body := doJSONReq(rr.r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+network+`","fixed_ips":`+fixed+`}}`, rr.member)
		return createdID(t, body, "port")
	}

	t.Run("a subnet and a port, or neither, are refused", func(t *testing.T) {
		for body, message := range map[string]string{
			`{"subnet_id":"` + subnet + `","port_id":"` + uuid.NewString() + `"}`: "Bad router request: Cannot specify both subnet-id and port-id.",
			`{}`: "Bad router request: Either subnet_id or port_id must be specified.",
		} {
			if code, resp := add(rr.member, body); code != http.StatusBadRequest || neutronErrorMessage(t, resp) != message {
				t.Errorf("%s: code=%d body=%s; want 400 %q", body, code, resp, message)
			}
		}
	})

	t.Run("another project's port is not found", func(t *testing.T) {
		requireSecondProject(t)
		other := seedProjectPort(t, otherProjectID, network)
		if code, resp := add(rr.member, `{"port_id":"`+other+`"}`); code != http.StatusNotFound {
			t.Errorf("code=%d body=%s; want 404", code, resp)
		}
	})

	t.Run("a port without a fixed IP is refused", func(t *testing.T) {
		bare := seedProjectPort(t, seededProjectID, network)
		want := "Bad router request: Router port must have at least one fixed IP."
		if code, resp := add(rr.member, `{"port_id":"`+bare+`"}`); code != http.StatusBadRequest || neutronErrorMessage(t, resp) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, resp, want)
		}
	})

	p := port(t, `[{"ip_address":"10.74.0.9"}]`)
	t.Run("a free port becomes the interface, with its own address", func(t *testing.T) {
		code, resp := add(rr.member, `{"port_id":"`+p+`"}`)
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200", code, resp)
		}
		var owner, device string
		if err := testPool.QueryRow(ctx, "SELECT device_owner, device_id::text FROM ports WHERE id = $1", p).Scan(&owner, &device); err != nil ||
			owner != ownerRouterInterface || device != router {
			t.Errorf("the port's device %q/%q (%v); want %s/%s", owner, device, err, ownerRouterInterface, router)
		}
		if n, _ := interfacesOn(t, router, subnet); n != 1 {
			t.Errorf("%d interfaces on the subnet; want 1", n)
		}
	})

	t.Run("a port in use is refused", func(t *testing.T) {
		if code, resp := add(rr.member, `{"port_id":"`+p+`"}`); code != http.StatusConflict || neutronErrorType(t, resp) != "PortInUse" {
			t.Errorf("code=%d body=%s; want 409 PortInUse", code, resp)
		}
	})

	t.Run("a second port on a subnet the router has a port on is refused", func(t *testing.T) {
		q := port(t, `[{"ip_address":"10.74.0.10"}]`)
		want := "Bad router request: Router already has a port on subnet " + subnet + "."
		if code, resp := add(rr.member, `{"port_id":"`+q+`"}`); code != http.StatusBadRequest || neutronErrorMessage(t, resp) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, resp, want)
		}
	})
}

// SPEC-033 T268, T269 (FR-032, FR-055): an interface added by subnet takes the subnet's gateway only while
// nothing holds it — a port or a floating IP, which may hold an external subnet's gateway (T198) — and reads
// that gateway under the subnet's lock, so a gateway changed meanwhile is the one it takes.
func TestRouterInterfaceTakesAGatewayNothingHolds(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	_, _, router := rr.createRouter(t, rr.admin, "")
	code, body := doJSONReq(rr.r, http.MethodPost, "/v2.0/floatingips",
		`{"floatingip":{"floating_network_id":"`+rr.external+`","floating_ip_address":"`+rr.gatewayOfExternal+`"}}`, rr.admin)
	if code != http.StatusCreated {
		t.Fatalf("a floating IP on the gateway address: code=%d body=%s", code, body)
	}
	t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE floating_ip_address = $1", rr.gatewayOfExternal) })
	code, body = rr.addInterface(rr.admin, router, rr.subnet)
	if code != http.StatusConflict || neutronErrorType(t, body) != "IpAddressAlreadyAllocated" {
		t.Errorf("an interface on the gateway a floating IP holds: code=%d body=%s; want 409 IpAddressAlreadyAllocated", code, body)
	}
}
