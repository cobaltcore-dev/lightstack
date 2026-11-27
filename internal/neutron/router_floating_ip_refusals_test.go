package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T094 (FR-039; research R31): a router keeps the path a floating IP goes through, as
// upstream's does. Removing the interface on a subnet whose fixed IPs a floating IP of the router
// reaches answers 409 RouterInterfaceInUseByFloatingIP; clearing the gateway, or moving it to another
// network, while a floating IP uses the router answers 409 RouterExternalGatewayInUseByFloatingIp.
// Nothing changes in either case; with no such floating IP both succeed.

// fip039 is a router with a gateway and one interface, and a floating IP through it.
type fip039 struct {
	rr               *routerRig
	router, internal string
	internalSubnet   string
	floatingIP       string
	otherExternal    string
}

func newFIP039(t *testing.T) *fip039 {
	t.Helper()
	rr := newRouterRig(t)
	f := &fip039{rr: rr}
	code, body, id := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create router: code=%d body=%s", code, body)
	}
	f.router = id
	gw := "10.96.0.1"
	f.internal = seedNetwork(t, "spec033-039-"+uuid.NewString()[:6])
	f.internalSubnet = seedSubnet(t, f.internal, "spec033-039", "10.96.0.0/24", &gw, 4, false)
	if code, body := doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+id+"/add_router_interface",
		`{"subnet_id":"`+f.internalSubnet+`"}`, rr.member); code != http.StatusOK {
		t.Fatalf("add interface: code=%d body=%s", code, body)
	}
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-039-vm', $2, $3, 'fa:16:3e:39:00:01', 'ACTIVE', $4::jsonb)`,
		port, f.internal, seededProjectID, `[{"subnet_id":"`+f.internalSubnet+`","ip_address":"10.96.0.5"}]`)
	f.floatingIP = uuid.NewString()
	mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status)
		VALUES ($1, $2, $3, $4, '10.96.0.5', $5, $6, 'ACTIVE')`,
		f.floatingIP, seededProjectID, rr.external, rr.prefix+".12", port, id)
	t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE id = $1", f.floatingIP) })
	f.otherExternal = seedExternalNetwork(t, "spec033-039-ext2-"+uuid.NewString()[:6], seededProjectID, false, true)
	gw2 := "198.19.250.1"
	seedSubnet(t, f.otherExternal, "spec033-039-ext2", "198.19.250.0/24", &gw2, 4, false)
	return f
}

func (f *fip039) interfaces(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM router_interfaces WHERE router_id = $1", f.router).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRouterRefusesToDropAFloatingIPsPath(t *testing.T) {
	requireDB(t)

	t.Run("the interface a floating IP reaches through", func(t *testing.T) {
		f := newFIP039(t)
		code, body := doJSONReq(f.rr.r, http.MethodPut, "/v2.0/routers/"+f.router+"/remove_router_interface",
			`{"subnet_id":"`+f.internalSubnet+`"}`, f.rr.member)
		if code != http.StatusConflict || neutronErrorType(t, body) != "RouterInterfaceInUseByFloatingIP" {
			t.Fatalf("remove: code=%d body=%s; want 409 RouterInterfaceInUseByFloatingIP", code, body)
		}
		if n := f.interfaces(t); n != 1 {
			t.Errorf("%d interfaces after the refusal; want 1", n)
		}
		mustExec(t, "DELETE FROM floating_ips WHERE id = $1", f.floatingIP)
		if code, body := doJSONReq(f.rr.r, http.MethodPut, "/v2.0/routers/"+f.router+"/remove_router_interface",
			`{"subnet_id":"`+f.internalSubnet+`"}`, f.rr.member); code != http.StatusOK {
			t.Errorf("remove once no floating IP uses it: code=%d body=%s; want 200", code, body)
		}
	})

	// SPEC-033 T184: a port forwarding's internal address counts as a floating IP's fixed IP when
	// its port is on the subnet, as upstream's _check_router_interface_not_in_use has it.
	t.Run("the interface a port forwarding reaches through", func(t *testing.T) {
		f := newFIP039(t)
		gw := "10.97.0.1"
		second := seedNetwork(t, "spec033-039-pf-"+uuid.NewString()[:6])
		secondSubnet := seedSubnet(t, second, "spec033-039-pf", "10.97.0.0/24", &gw, 4, false)
		if code, body := doJSONReq(f.rr.r, http.MethodPut, "/v2.0/routers/"+f.router+"/add_router_interface",
			`{"subnet_id":"`+secondSubnet+`"}`, f.rr.member); code != http.StatusOK {
			t.Fatalf("add the second interface: code=%d body=%s", code, body)
		}
		elsewhere := seedNetwork(t, "spec033-039-pf-other-"+uuid.NewString()[:6])
		elsewhereSubnet := seedSubnet(t, elsewhere, "spec033-039-pf-other", "10.97.0.0/24", &gw, 4, false)
		portOn := func(network, subnet, mac string) string {
			id := uuid.NewString()
			mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
				VALUES ($1, 'spec033-039-pf', $2, $3, $4, 'ACTIVE', $5::jsonb)`,
				id, network, seededProjectID, mac, `[{"subnet_id":"`+subnet+`","ip_address":"10.97.0.7"}]`)
			t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", id) })
			return id
		}
		onSubnet := portOn(second, secondSubnet, "fa:16:3e:39:01:01")
		offSubnet := portOn(elsewhere, elsewhereSubnet, "fa:16:3e:39:01:02")
		forwarding := uuid.NewString()
		mustExec(t, `INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id, internal_ip_address, external_port, internal_port, protocol, status)
			VALUES ($1, $2, $3, $4, '10.97.0.7', 2222, 22, 'tcp', 'ACTIVE')`, forwarding, seededProjectID, f.floatingIP, onSubnet)
		remove := func() (int, string) {
			return doJSONReq(f.rr.r, http.MethodPut, "/v2.0/routers/"+f.router+"/remove_router_interface",
				`{"subnet_id":"`+secondSubnet+`"}`, f.rr.member)
		}
		if code, body := remove(); code != http.StatusConflict || neutronErrorType(t, body) != "RouterInterfaceInUseByFloatingIP" {
			t.Fatalf("remove: code=%d body=%s; want 409 RouterInterfaceInUseByFloatingIP", code, body)
		}
		if n := f.interfaces(t); n != 2 {
			t.Errorf("%d interfaces after the refusal; want 2", n)
		}
		// The same address on a port of another network is not the subnet's, for a port forwarding and a
		// floating IP alike: upstream asks for both.
		mustExec(t, "UPDATE port_forwardings SET internal_port_id = $1 WHERE id = $2", offSubnet, forwarding)
		mustExec(t, "UPDATE floating_ips SET port_id = $1, fixed_ip_address = '10.97.0.7' WHERE id = $2", offSubnet, f.floatingIP)
		if code, body := remove(); code != http.StatusOK {
			t.Errorf("remove once the forwarding's and the floating IP's port is off the subnet: code=%d body=%s; want 200", code, body)
		}
	})

	t.Run("the gateway a floating IP goes through", func(t *testing.T) {
		f := newFIP039(t)
		for _, gateway := range []string{`{}`, `null`, `{"network_id":"` + f.otherExternal + `"}`} {
			code, body := f.rr.updateGateway(t, f.rr.member, f.router, gateway)
			if code != http.StatusConflict || neutronErrorType(t, body) != "RouterExternalGatewayInUseByFloatingIp" {
				t.Errorf("gateway %s: code=%d body=%s; want 409 RouterExternalGatewayInUseByFloatingIp", gateway, code, body)
			}
		}
		if ports := gatewayPorts(t, f.router); len(ports) != 1 || ports[0].network != f.rr.external {
			t.Errorf("gateway ports after the refusals: %+v; want the one on %s", ports, f.rr.external)
		}
		// Turning SNAT off, an administrator's (SPEC-033 T183), keeps the gateway and its port:
		// not a change of path.
		if code, body := f.rr.updateGateway(t, f.rr.admin, f.router, `{"network_id":"`+f.rr.external+`","enable_snat":false}`); code != http.StatusOK {
			t.Errorf("enable_snat only: code=%d body=%s; want 200", code, body)
		}
		mustExec(t, "DELETE FROM floating_ips WHERE id = $1", f.floatingIP)
		if code, body := f.rr.updateGateway(t, f.rr.member, f.router, `{}`); code != http.StatusOK {
			t.Errorf("clear once no floating IP uses it: code=%d body=%s; want 200", code, body)
		}
	})
}
