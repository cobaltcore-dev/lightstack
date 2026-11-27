package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T315 (FR-069, FR-039; upstream neutron master, db/l3_db.py and agent/l3/router_info.py): a router
// interface port holds one IPv4 subnet and any IPv6 ones, as upstream's. Every fixed IP of the port is on its
// leg (_internal_network_added hands the driver all of them); the router records every subnet the port
// joins, so the duplicate check, the floating-IP check of a removal or an undo (every subnet of the port,
// _remove_interface_by_port), and a removal by one subnet, which takes that subnet off a port that keeps
// others (_remove_interface_by_subnet), see each. One IPv6 port per network per router
// (_validate_one_router_ipv6_port_per_network), and an IPv6 subnet added by subnet joins the network's IPv6
// interface port (_add_interface_by_subnet).

// dualStack is a network with an IPv6 and an IPv4 subnet and a second IPv6 one, and a port holding an
// address on the first two, IPv6 first: the order that lost the IPv4 address.
type dualStack struct{ network, v6, v4, v6b, port string }

func newDualStack(t *testing.T, rig *unitRig) dualStack {
	t.Helper()
	gw6, gw4, gw6b := "fd00:184::1", "10.184.0.1", "fd00:185::1"
	d := dualStack{network: seedNetwork(t, "spec033-t315-"+uuid.NewString()[:6])}
	d.v6 = seedSubnet(t, d.network, "spec033-t315-v6", "fd00:184::/64", &gw6, 6, false)
	d.v4 = seedSubnet(t, d.network, "spec033-t315-v4", "10.184.0.0/24", &gw4, 4, false)
	d.v6b = seedSubnet(t, d.network, "spec033-t315-v6b", "fd00:185::/64", &gw6b, 6, false)
	d.port = d.newPort(t, rig, `[{"subnet_id":"`+d.v6+`","ip_address":"fd00:184::5"},{"subnet_id":"`+d.v4+`","ip_address":"10.184.0.5"}]`)
	return d
}

// newPort makes a port on the network with fixed, a fixed_ips list.
func (d dualStack) newPort(t *testing.T, rig *unitRig, fixed string) string {
	t.Helper()
	return createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/ports",
		`{"port":{"network_id":"`+d.network+`","fixed_ips":`+fixed+`}}`), "port")
}

func routerAction(rig *unitRig, router, action, body string) (int, string) {
	return doJSONReq(rig.r, http.MethodPut, "/v2.0/routers/"+router+"/"+action, body, rig.hdr)
}

// legAddresses is the addresses the network node holds on the port's leg of the router, nil without one.
func legAddresses(t *testing.T, rig *unitRig, router, port string) []string {
	t.Helper()
	var found []networking.RouterInterface
	for _, i := range rig.held(t, router).Interfaces {
		if i.PortID == port {
			found = append(found, i)
		}
	}
	switch len(found) {
	case 0:
		return nil
	case 1:
		return found[0].Addresses
	}
	t.Fatalf("the router holds %d legs for port %s; want one", len(found), port)
	return nil
}

// subnetsOfPort is the subnets of the port's fixed IPs, sorted.
func subnetsOfPort(t *testing.T, port string) []string {
	t.Helper()
	var fixed common.FixedIPs
	if err := testPool.QueryRow(context.Background(), "SELECT fixed_ips FROM ports WHERE id = $1", port).Scan(&fixed); err != nil {
		t.Fatalf("read port %s: %v", port, err)
	}
	var out []string
	for _, f := range fixed {
		out = append(out, f.SubnetID)
	}
	slices.Sort(out)
	return out
}

// failOnceDuring fails the router's next convergence once, running during first.
func failOnceDuring(rig *unitRig, during func()) {
	var once sync.Once
	rig.routers.hook = func(holderCall[networking.RouterState]) error {
		var err error
		once.Do(func() {
			during()
			err = errNode
		})
		return err
	}
}

func TestRouterInterfacePortWithSeveralSubnets(t *testing.T) {
	requireDB(t)
	// attached is a rig whose router has the dual-stack port as an interface, added by port.
	attached := func(t *testing.T) (*unitRig, string, dualStack) {
		t.Helper()
		rig := newUnitRig(t)
		router, _ := rig.router(t)
		d := newDualStack(t, rig)
		if code, body := routerAction(rig, router, "add_router_interface", `{"port_id":"`+d.port+`"}`); code != http.StatusOK {
			t.Fatalf("add the dual-stack port: code=%d body=%s; want 200", code, body)
		}
		return rig, router, d
	}
	// reaching gives a port on the IPv4 subnet a floating IP through the router.
	reaching := func(t *testing.T, rig *unitRig, d dualStack) {
		t.Helper()
		vm := d.newPort(t, rig, `[{"subnet_id":"`+d.v4+`","ip_address":"10.184.0.20"}]`)
		rig.floatingIP(t, vm)
	}

	t.Run("every fixed IP of the port is on its leg", func(t *testing.T) {
		rig, router, d := attached(t)
		got := legAddresses(t, rig, router, d.port)
		slices.Sort(got)
		if want := []string{"10.184.0.5/24", "fd00:184::5/64"}; !slices.Equal(got, want) {
			t.Errorf("the leg holds %v; want %v", got, want)
		}
		for _, s := range []string{d.v6, d.v4} {
			if n, _ := interfacesOn(t, router, s); n != 1 {
				t.Errorf("%d interfaces of the router on subnet %s; want the port's", n, s)
			}
		}
	})

	t.Run("the port's second subnet is the router's: adding it by subnet is refused", func(t *testing.T) {
		rig, router, d := attached(t)
		want := "Bad router request: Router already has a port on subnet " + d.v4 + "."
		if code, body := routerAction(rig, router, "add_router_interface", `{"subnet_id":"`+d.v4+`"}`); code != http.StatusBadRequest || neutronErrorMessage(t, body) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, body, want)
		}
	})

	t.Run("a removal by port is refused while a floating IP reaches its second subnet", func(t *testing.T) {
		rig, router, d := attached(t)
		reaching(t, rig, d)
		if code, body := routerAction(rig, router, "remove_router_interface", `{"port_id":"`+d.port+`"}`); code != http.StatusConflict || neutronErrorType(t, body) != "RouterInterfaceInUseByFloatingIP" {
			t.Errorf("code=%d body=%s; want 409 RouterInterfaceInUseByFloatingIP", code, body)
		}
	})

	t.Run("a removal by one subnet takes it off the port, which keeps the other", func(t *testing.T) {
		rig, router, d := attached(t)
		if code, body := routerAction(rig, router, "remove_router_interface", `{"subnet_id":"`+d.v4+`"}`); code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200", code, body)
		}
		if got := subnetsOfPort(t, d.port); !slices.Equal(got, []string{d.v6}) {
			t.Errorf("the port's subnets %v; want only %s", got, d.v6)
		}
		if got := legAddresses(t, rig, router, d.port); !slices.Equal(got, []string{"fd00:184::5/64"}) {
			t.Errorf("the leg holds %v; want only fd00:184::5/64", got)
		}
		if n, _ := interfacesOn(t, router, d.v4); n != 0 {
			t.Errorf("%d interfaces left on the removed subnet; want none", n)
		}
	})

	t.Run("a removal by one subnet is refused while a floating IP reaches it", func(t *testing.T) {
		rig, router, d := attached(t)
		reaching(t, rig, d)
		if code, body := routerAction(rig, router, "remove_router_interface", `{"subnet_id":"`+d.v4+`"}`); code != http.StatusConflict || neutronErrorType(t, body) != "RouterInterfaceInUseByFloatingIP" {
			t.Errorf("code=%d body=%s; want 409 RouterInterfaceInUseByFloatingIP", code, body)
		}
	})

	t.Run("a removal by one subnet the node cannot apply keeps it", func(t *testing.T) {
		rig, router, d := attached(t)
		failOnceDuring(rig, func() {})
		if code, body := routerAction(rig, router, "remove_router_interface", `{"subnet_id":"`+d.v4+`"}`); code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s; want 500", code, body)
		}
		if got := subnetsOfPort(t, d.port); len(got) != 2 {
			t.Errorf("the port's subnets %v; want both kept", got)
		}
		if got := legAddresses(t, rig, router, d.port); len(got) != 2 {
			t.Errorf("the leg holds %v; want both addresses back", got)
		}
	})

	t.Run("an add's undo keeps an interface a floating IP has come to reach through its second subnet", func(t *testing.T) {
		rig := newUnitRig(t)
		router, _ := rig.router(t)
		d := newDualStack(t, rig)
		vm := d.newPort(t, rig, `[{"subnet_id":"`+d.v4+`","ip_address":"10.184.0.20"}]`)
		failOnceDuring(rig, func() {
			mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, port_id, fixed_ip_address, router_id, status)
				VALUES ($1, $2, $3, $4, $5, '10.184.0.20', $6, 'ACTIVE')`, uuid.NewString(), seededProjectID, rig.external, rig.externalPrefix+".94", vm, router)
		})
		if code, body := routerAction(rig, router, "add_router_interface", `{"port_id":"`+d.port+`"}`); code != http.StatusInternalServerError {
			t.Fatalf("add with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if n, _ := interfacesOn(t, router, d.v4); n != 1 {
			t.Errorf("%d interfaces on the subnet the floating IP reaches; want the interface kept", n)
		}
	})

	t.Run("one IPv6 port of a network per router", func(t *testing.T) {
		rig, router, d := attached(t)
		other := d.newPort(t, rig, `[{"subnet_id":"`+d.v6b+`","ip_address":"fd00:185::5"}]`)
		want := "Bad router request: Router already contains IPv6 port " + d.port + " belonging to network id " + d.network +
			". Only one IPv6 port from the same network subnet can be connected to a router.."
		if code, body := routerAction(rig, router, "add_router_interface", `{"port_id":"`+other+`"}`); code != http.StatusBadRequest || neutronErrorMessage(t, body) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, body, want)
		}
	})

	t.Run("the router's gateway port counts as its IPv6 port on the external network", func(t *testing.T) {
		rig := newUnitRig(t)
		router, _ := rig.router(t)
		gw6 := "fd00:186::1"
		v6 := seedSubnet(t, rig.external, "spec033-t315-ext-v6", "fd00:186::/64", &gw6, 6, false)
		gateway := gatewayPorts(t, router)[0]
		var fixed common.FixedIPs
		if err := json.Unmarshal([]byte(gateway.fixedIPs), &fixed); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(append(fixed, common.FixedIP{SubnetID: v6, IPAddress: "fd00:186::5"}))
		mustExec(t, "UPDATE ports SET fixed_ips = $1::jsonb WHERE id = $2", string(raw), gateway.id)
		other := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+rig.external+`","fixed_ips":[{"subnet_id":"`+v6+`","ip_address":"fd00:186::7"}]}}`), "port")
		want := "Bad router request: Router already contains IPv6 port " + gateway.id + " belonging to network id " + rig.external +
			". Only one IPv6 port from the same network subnet can be connected to a router.."
		if code, body := routerAction(rig, router, "add_router_interface", `{"port_id":"`+other+`"}`); code != http.StatusBadRequest || neutronErrorMessage(t, body) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, body, want)
		}
	})

	t.Run("an IPv6 subnet added by subnet joins the network's IPv6 interface port", func(t *testing.T) {
		rig, router, d := attached(t)
		code, body := routerAction(rig, router, "add_router_interface", `{"subnet_id":"`+d.v6b+`"}`)
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200", code, body)
		}
		var answer struct {
			PortID string `json:"port_id"`
		}
		if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.PortID != d.port {
			got := answer.PortID
			t.Errorf("the interface's port %q (%v); want the network's IPv6 interface port %s", got, err, d.port)
		}
		if got := subnetsOfPort(t, d.port); len(got) != 3 || !slices.Contains(got, d.v6b) {
			t.Errorf("the port's subnets %v; want %s joined", got, d.v6b)
		}
		if got := legAddresses(t, rig, router, d.port); !slices.Contains(got, "fd00:185::1/64") {
			t.Errorf("the leg holds %v; want the subnet's gateway fd00:185::1/64 on it", got)
		}
		if n, _ := interfacesOn(t, router, d.v6b); n != 1 {
			t.Errorf("%d interfaces of the router on the joined subnet; want 1", n)
		}
	})

	t.Run("a join the node cannot apply is undone", func(t *testing.T) {
		rig, router, d := attached(t)
		failOnceDuring(rig, func() {})
		if code, body := routerAction(rig, router, "add_router_interface", `{"subnet_id":"`+d.v6b+`"}`); code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s; want 500", code, body)
		}
		if got := subnetsOfPort(t, d.port); slices.Contains(got, d.v6b) || len(got) != 2 {
			t.Errorf("the port's subnets %v; want the join undone", got)
		}
		if n, _ := interfacesOn(t, router, d.v6b); n != 0 {
			t.Errorf("%d interfaces on the subnet after the undo; want none", n)
		}
	})
}
