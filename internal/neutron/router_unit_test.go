package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T114 (FR-008, FR-008a, FR-033, FR-034, FR-038, FR-040, FR-045, FR-049; research
// R20, R27, R28, R34, R37): every change to a router's state converges the router on the network node
// before the answer, through the node call when another node answers the request. A change that
// cannot be applied answers 500: a create or an update is undone, compare-and-set; a deletion keeps
// its record and the router is converged from it again.

const (
	unitAPINode     = "spec033-api"
	unitNetworkNode = "spec033-net"
)

// unitRig is two nodes on one database: the API node, which answers the requests, and the network
// node, which holds the routers. The fake node caller carries the units between them.
type unitRig struct {
	r              *gin.Engine // the API node's routes
	api, net       *Service
	routers        *fakeRouters // the network node's
	apiRouters     *fakeRouters // the API node's, which must stay untouched
	caller         *fakeNodeCaller
	hdr            map[string]string
	external       string
	externalPrefix string
}

func newUnitRig(t *testing.T) *unitRig {
	t.Helper()
	r, api := newNeutronTestRouterWithService()
	_, net := newNeutronTestRouterWithService()
	api.SetNodes(NodeOptions{Name: unitAPINode, NetworkNode: unitNetworkNode})
	net.SetNodes(NodeOptions{Name: unitNetworkNode, NetworkNode: unitNetworkNode})
	rig := &unitRig{r: r, api: api, net: net, routers: newFakeRouters(), apiRouters: newFakeRouters(),
		hdr: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}}
	net.routers, api.routers = rig.routers, rig.apiRouters
	rig.caller = newFakeNodeCaller(api, net)
	api.caller, net.caller = rig.caller, rig.caller
	rig.externalPrefix = fmt.Sprintf("198.18.%d", rand.IntN(250))
	rig.external = seedExternalNetwork(t, "spec033-unit-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
	gateway := rig.externalPrefix + ".1"
	subnet := seedSubnet(t, rig.external, "spec033-unit-ext", rig.externalPrefix+".0/24", &gateway, 4, false)
	storeTestPools(t, subnet, [2]string{rig.externalPrefix + ".10", rig.externalPrefix + ".99"})
	t.Cleanup(func() {
		if calls := rig.apiRouters.recorded(); len(calls) != 0 {
			t.Errorf("the API node converged %d router(s) itself; the network node holds them", len(calls))
		}
	})
	return rig
}

func (rig *unitRig) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	return doJSONReq(rig.r, method, path, body, rig.hdr)
}

// must makes a request that has to answer want, and returns the body.
func (rig *unitRig) must(t *testing.T, want int, method, path, body string) string {
	t.Helper()
	code, resp := rig.do(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", method, path, code, resp, want)
	}
	return resp
}

// router creates a router with a gateway on the external network and an interface on a new internal
// network, and returns the router and the internal network.
func (rig *unitRig) router(t *testing.T) (routerID, network string) {
	t.Helper()
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers",
		`{"router":{"name":"spec033-unit","external_gateway_info":{"network_id":"`+rig.external+`"}}}`)
	routerID = createdID(t, resp, "router")
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM floating_ips WHERE router_id = $1", routerID)
		mustExec(t, "DELETE FROM ports WHERE device_id = $1", routerID)
		mustExec(t, "DELETE FROM routers WHERE id = $1", routerID)
	})
	network = seedNetwork(t, "spec033-unit-net-"+uuid.NewString()[:6])
	gateway := "10.77.0.1"
	subnet := seedSubnet(t, network, "spec033-unit-subnet", "10.77.0.0/24", &gateway, 4, true)
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface", `{"subnet_id":"`+subnet+`"}`)
	return routerID, network
}

// port creates a port on network and returns it and its address.
func (rig *unitRig) port(t *testing.T, network string) (id, address string) {
	t.Helper()
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`"}}`)
	var out struct {
		Port struct {
			ID       string `json:"id"`
			FixedIPs []struct {
				IP string `json:"ip_address"`
			} `json:"fixed_ips"`
		} `json:"port"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Port.ID == "" || len(out.Port.FixedIPs) == 0 {
		t.Fatalf("no port and address in %s (%v)", resp, err)
	}
	return out.Port.ID, out.Port.FixedIPs[0].IP
}

// floatingIP creates a floating IP on the external network, on port when it is not "".
func (rig *unitRig) floatingIP(t *testing.T, port string) (id, address string) {
	t.Helper()
	body := `{"floatingip":{"floating_network_id":"` + rig.external + `"}}`
	if port != "" {
		body = `{"floatingip":{"floating_network_id":"` + rig.external + `","port_id":"` + port + `"}}`
	}
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips", body)
	var out struct {
		FloatingIP struct {
			ID      string `json:"id"`
			Address string `json:"floating_ip_address"`
		} `json:"floatingip"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.FloatingIP.ID == "" {
		t.Fatalf("no floating IP in %s (%v)", resp, err)
	}
	return out.FloatingIP.ID, out.FloatingIP.Address
}

// held is the router as the network node holds it.
func (rig *unitRig) held(t *testing.T, routerID string) networking.RouterState {
	t.Helper()
	s, ok := rig.routers.state(routerID)
	if !ok {
		t.Fatalf("the network node holds no router %s", routerID)
	}
	return s
}

func hasFloating(s networking.RouterState, address, fixed string) bool {
	return slices.Contains(s.FloatingIPs, networking.RouterFloatingIP{Address: address, FixedIP: fixed})
}

func TestRouterUnitChangesConvergeBeforeTheAnswer(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	routerID, network := rig.router(t)

	s := rig.held(t, routerID)
	if s.Gateway == nil || !s.Gateway.SNAT || len(s.Gateway.Addresses) != 1 || s.Gateway.NextHop != rig.externalPrefix+".1" {
		t.Fatalf("after create: gateway %+v; want one address on the external subnet, SNAT on, next hop .1", s.Gateway)
	}
	if len(s.Interfaces) != 1 || !slices.Equal(s.Interfaces[0].Addresses, []string{"10.77.0.1/24"}) || s.Interfaces[0].Bridge != networking.BridgeNameForNetwork(network) {
		t.Fatalf("after the interface: %+v; want 10.77.0.1/24 on the network's bridge", s.Interfaces)
	}
	for _, c := range rig.caller.recorded() {
		if c.Node != unitNetworkNode || c.Req.Unit != unitRouter {
			t.Errorf("a call to %s for %s; want the router units on the network node", c.Node, c.Req.Unit)
		}
	}

	p1, x1 := rig.port(t, network)
	p2, x2 := rig.port(t, network)
	fip, f := rig.floatingIP(t, p1)
	if !hasFloating(rig.held(t, routerID), f, x1) {
		t.Errorf("after the create: %+v; want %s at %s", rig.held(t, routerID).FloatingIPs, f, x1)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":"`+p2+`"}}`)
	if s := rig.held(t, routerID); !hasFloating(s, f, x2) || hasFloating(s, f, x1) {
		t.Errorf("after the move: %+v; want %s at %s only", s.FloatingIPs, f, x2)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":null}}`)
	if s := rig.held(t, routerID); len(s.FloatingIPs) != 0 {
		t.Errorf("after the disassociation: %+v; want none", s.FloatingIPs)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":"`+p1+`"}}`)
	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+fip, "")
	if s := rig.held(t, routerID); len(s.FloatingIPs) != 0 {
		t.Errorf("after the delete: %+v; want none", s.FloatingIPs)
	}

	// Port forwardings, on a floating IP through the router without a port.
	pfip, pf := rig.floatingIP(t, "")
	mustExec(t, "UPDATE floating_ips SET router_id = $1 WHERE id = $2", routerID, pfip)
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+pfip+"/port_forwardings",
		`{"port_forwarding":{"internal_port_id":"`+p1+`","internal_ip_address":"`+x1+`","external_port":2222,"internal_port":22,"protocol":"tcp"}}`)
	pfID := createdID(t, resp, "port_forwarding")
	want := networking.RouterPortForwarding{ID: pfID, FloatingIP: pf, Protocol: "tcp", ExternalPort: 2222, InternalIP: x1, InternalPort: 22}
	if s := rig.held(t, routerID); !slices.Contains(s.PortForwardings, want) {
		t.Errorf("after the port forwarding's create: %+v; want %+v", s.PortForwardings, want)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/floatingips/"+pfip+"/port_forwardings/"+pfID, `{"port_forwarding":{"internal_port":2022}}`)
	want.InternalPort = 2022
	if s := rig.held(t, routerID); !slices.Contains(s.PortForwardings, want) {
		t.Errorf("after the port forwarding's update: %+v; want %+v", s.PortForwardings, want)
	}
	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+pfip+"/port_forwardings/"+pfID, "")
	if s := rig.held(t, routerID); len(s.PortForwardings) != 0 {
		t.Errorf("after the port forwarding's delete: %+v; want none", s.PortForwardings)
	}
	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+pfip, "")

	// The gateway: SNAT off, which is an administrator's (SPEC-033 T183), then cleared.
	if code, body := doJSONReq(rig.r, http.MethodPut, "/v2.0/routers/"+routerID,
		`{"router":{"external_gateway_info":{"network_id":"`+rig.external+`","enable_snat":false}}}`,
		map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}); code != http.StatusOK {
		t.Fatalf("SNAT off: code=%d body=%s; want 200", code, body)
	}
	if s := rig.held(t, routerID); s.Gateway == nil || s.Gateway.SNAT {
		t.Errorf("after SNAT off: gateway %+v; want it kept with SNAT off", s.Gateway)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID, `{"router":{"external_gateway_info":{}}}`)
	if s := rig.held(t, routerID); s.Gateway != nil {
		t.Errorf("after the clear: gateway %+v; want none", s.Gateway)
	}

	// The interface, then the router.
	var subnet string
	if err := testPool.QueryRow(context.Background(), "SELECT subnet_id::text FROM router_interfaces WHERE router_id = $1", routerID).Scan(&subnet); err != nil {
		t.Fatal(err)
	}
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface", `{"subnet_id":"`+subnet+`"}`)
	if s := rig.held(t, routerID); len(s.Interfaces) != 0 {
		t.Errorf("after the interface's removal: %+v; want none", s.Interfaces)
	}
	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/routers/"+routerID, "")
	if _, ok := rig.routers.state(routerID); ok {
		t.Error("after the router's delete: the network node still holds it")
	}
}

func TestRouterUnitFailuresUndoOrReapply(t *testing.T) {
	requireDB(t)
	errUnit := errors.New("the network node could not converge the router")
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// fails runs a request whose first convergence fails, and wants 500.
	fails := func(t *testing.T, rig *unitRig, method, path, body string) {
		t.Helper()
		rig.routers.failFirst(1, errUnit)
		if code, resp := rig.do(t, method, path, body); code != http.StatusInternalServerError {
			t.Fatalf("%s %s with a failing router unit: code=%d body=%s; want 500", method, path, code, resp)
		}
		rig.routers.failFirst(0, nil)
	}

	t.Run("a floating IP's create is undone", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, _ := rig.port(t, network)
		fails(t, rig, http.MethodPost, "/v2.0/floatingips", `{"floatingip":{"floating_network_id":"`+rig.external+`","port_id":"`+p+`"}}`)
		if n := count(t, "SELECT count(*) FROM floating_ips WHERE port_id = $1", p); n != 0 {
			t.Errorf("%d floating IP(s) left on the port; want the create undone", n)
		}
		if s := rig.held(t, routerID); len(s.FloatingIPs) != 0 {
			t.Errorf("the router after the undo: %+v; want it converged without the floating IP", s.FloatingIPs)
		}
	})

	t.Run("a floating IP's move is undone", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p1, x1 := rig.port(t, network)
		p2, _ := rig.port(t, network)
		fip, f := rig.floatingIP(t, p1)
		fails(t, rig, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":"`+p2+`"}}`)
		var port string
		if err := testPool.QueryRow(context.Background(), "SELECT port_id::text FROM floating_ips WHERE id = $1", fip).Scan(&port); err != nil || port != p1 {
			t.Errorf("the floating IP's port after the undo: %q, %v; want %s back", port, err, p1)
		}
		if !hasFloating(rig.held(t, routerID), f, x1) {
			t.Errorf("the router after the undo: %+v; want %s at %s", rig.held(t, routerID).FloatingIPs, f, x1)
		}
	})

	t.Run("a port forwarding's create is undone and its delete kept", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, x := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		mustExec(t, "UPDATE floating_ips SET router_id = $1 WHERE id = $2", routerID, fip)
		body := `{"port_forwarding":{"internal_port_id":"` + p + `","internal_ip_address":"` + x + `","external_port":2223,"internal_port":22,"protocol":"tcp"}}`
		fails(t, rig, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", body)
		if n := count(t, "SELECT count(*) FROM port_forwardings WHERE floatingip_id = $1", fip); n != 0 {
			t.Errorf("%d port forwarding(s) left; want the create undone", n)
		}
		pfID := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", body), "port_forwarding")
		fails(t, rig, http.MethodDelete, "/v2.0/floatingips/"+fip+"/port_forwardings/"+pfID, "")
		if n := count(t, "SELECT count(*) FROM port_forwardings WHERE id = $1", pfID); n != 1 {
			t.Errorf("the port forwarding after a failed delete: %d rows; want it kept", n)
		}
		if s := rig.held(t, routerID); len(s.PortForwardings) != 1 {
			t.Errorf("the router after the failed delete: %+v; want it converged with the port forwarding again", s.PortForwardings)
		}
	})

	t.Run("an interface's add is undone and its removal kept", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, _ := rig.router(t)
		network := seedNetwork(t, "spec033-unit-second-"+uuid.NewString()[:6])
		gateway := "10.78.0.1"
		subnet := seedSubnet(t, network, "spec033-unit-second", "10.78.0.0/24", &gateway, 4, false)
		fails(t, rig, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface", `{"subnet_id":"`+subnet+`"}`)
		if n := count(t, "SELECT count(*) FROM router_interfaces WHERE router_id = $1 AND subnet_id = $2", routerID, subnet); n != 0 {
			t.Errorf("%d interface(s) on the subnet; want the add undone", n)
		}
		if n := count(t, "SELECT count(*) FROM ports WHERE device_id = $1 AND network_id = $2", routerID, network); n != 0 {
			t.Errorf("%d interface port(s) left; want none", n)
		}
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface", `{"subnet_id":"`+subnet+`"}`)
		fails(t, rig, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface", `{"subnet_id":"`+subnet+`"}`)
		if n := count(t, "SELECT count(*) FROM router_interfaces WHERE router_id = $1 AND subnet_id = $2", routerID, subnet); n != 1 {
			t.Errorf("the interface after a failed removal: %d; want it kept", n)
		}
		if s := rig.held(t, routerID); len(s.Interfaces) != 2 {
			t.Errorf("the router after the failed removal: %+v; want both interfaces again", s.Interfaces)
		}
	})

	t.Run("a gateway's set is undone and its clear kept", func(t *testing.T) {
		rig := newUnitRig(t)
		resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers", `{"router":{"name":"spec033-unit-bare"}}`)
		routerID := createdID(t, resp, "router")
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM ports WHERE device_id = $1", routerID)
			mustExec(t, "DELETE FROM routers WHERE id = $1", routerID)
		})
		gateway := `{"router":{"external_gateway_info":{"network_id":"` + rig.external + `"}}}`
		fails(t, rig, http.MethodPut, "/v2.0/routers/"+routerID, gateway)
		if n := count(t, "SELECT count(*) FROM ports WHERE device_id = $1 AND device_owner = 'network:router_gateway'", routerID); n != 0 {
			t.Errorf("%d gateway port(s) after a failed set; want the set undone", n)
		}
		var info *string
		if err := testPool.QueryRow(context.Background(), "SELECT external_gateway_info FROM routers WHERE id = $1", routerID).Scan(&info); err != nil || info != nil && *info != "null" {
			t.Errorf("external_gateway_info after a failed set: %v, %v; want none", info, err)
		}
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID, gateway)
		fails(t, rig, http.MethodPut, "/v2.0/routers/"+routerID, `{"router":{"external_gateway_info":{}}}`)
		if n := count(t, "SELECT count(*) FROM ports WHERE device_id = $1 AND device_owner = 'network:router_gateway'", routerID); n != 1 {
			t.Errorf("gateway ports after a failed clear: %d; want the gateway kept", n)
		}
		if s := rig.held(t, routerID); s.Gateway == nil {
			t.Error("the router after the failed clear: no gateway; want it converged with the gateway again")
		}
	})

	t.Run("a router's create is undone and its delete kept", func(t *testing.T) {
		rig := newUnitRig(t)
		name := "spec033-unit-undo-" + uuid.NewString()[:6]
		fails(t, rig, http.MethodPost, "/v2.0/routers", `{"router":{"name":"`+name+`","external_gateway_info":{"network_id":"`+rig.external+`"}}}`)
		if n := count(t, "SELECT count(*) FROM routers WHERE name = $1", name); n != 0 {
			t.Errorf("%d router(s) left after a failed create; want the create undone", n)
		}
		routerID, _ := rig.router(t)
		var subnet string
		if err := testPool.QueryRow(context.Background(), "SELECT subnet_id::text FROM router_interfaces WHERE router_id = $1", routerID).Scan(&subnet); err != nil {
			t.Fatal(err)
		}
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface", `{"subnet_id":"`+subnet+`"}`)
		fails(t, rig, http.MethodDelete, "/v2.0/routers/"+routerID, "")
		if n := count(t, "SELECT count(*) FROM routers WHERE id = $1", routerID); n != 1 {
			t.Errorf("the router after a failed delete: %d; want it kept", n)
		}
		if _, ok := rig.routers.state(routerID); !ok {
			t.Error("the network node holds no router after the failed delete; want it converged again")
		}
	})
}

// A floating IP deleted with port forwardings takes their rules with it: its router is converged.
// SPEC-033 T189 (FR-045; constitution Article II): a router's default route is its gateway
// subnet's gateway, so changing that gateway converges, before the answer, every router whose gateway
// port has an address on the subnet, as upstream notifies them (_notify_subnet_gateway_ip_update). When
// one cannot take it, the update is undone and the routers converge back.
func TestRouterUnitFollowsItsGatewaySubnetsGateway(t *testing.T) {
	requireDB(t)
	var subnet string
	setup := func(t *testing.T) (*unitRig, string) {
		rig := newUnitRig(t)
		routerID, _ := rig.router(t)
		subnet = subnetOf(t, rig.external)
		return rig, routerID
	}
	nextHop := func(t *testing.T, rig *unitRig, routerID string) string {
		t.Helper()
		if s := rig.held(t, routerID); s.Gateway != nil {
			return s.Gateway.NextHop
		}
		return ""
	}

	t.Run("converged before the answer", func(t *testing.T) {
		rig, routerID := setup(t)
		moved := rig.externalPrefix + ".2"
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/subnets/"+subnet, `{"subnet":{"gateway_ip":"`+moved+`"}}`)
		if hop := nextHop(t, rig, routerID); hop != moved {
			t.Errorf("the router's next hop after the gateway's change: %q; want %s", hop, moved)
		}
	})

	t.Run("undone when a router cannot take it", func(t *testing.T) {
		rig, first := setup(t)
		second, _ := rig.router(t)
		old := rig.externalPrefix + ".1"
		// The first router converges, the second cannot: the first must be put back too.
		var mu sync.Mutex
		calls := 0
		rig.routers.hook = func(holderCall[networking.RouterState]) error {
			mu.Lock()
			defer mu.Unlock()
			if calls++; calls == 2 {
				return errors.New("the network node could not converge the router")
			}
			return nil
		}
		code, body := rig.do(t, http.MethodPut, "/v2.0/subnets/"+subnet, `{"subnet":{"gateway_ip":"`+rig.externalPrefix+`.2"}}`)
		if code != http.StatusInternalServerError {
			t.Fatalf("change the gateway with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		var gateway string
		if err := testPool.QueryRow(context.Background(), "SELECT COALESCE(gateway_ip, '') FROM subnets WHERE id = $1", subnet).Scan(&gateway); err != nil || gateway != old {
			t.Errorf("the subnet's gateway after the failed update: %q (%v); want %s back", gateway, err, old)
		}
		for _, router := range []string{first, second} {
			if hop := nextHop(t, rig, router); hop != old {
				t.Errorf("router %s's next hop after the undo: %q; want %s", router, hop, old)
			}
		}
	})
}

// SPEC-033 T184 (FR-040): a port's deletion takes its port forwardings off their routers before
// the records go, as its floating IPs, the forwardings' rows going with the port as upstream's do. When
// the router cannot drop them, the port stays and the router holds the forwarding again.
func TestRouterUnitPortDeleteTakesItsPortForwardings(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	routerID, network := rig.router(t)
	p, x := rig.port(t, network)
	fip, _ := rig.floatingIP(t, "")
	mustExec(t, "UPDATE floating_ips SET router_id = $1 WHERE id = $2", routerID, fip)
	pf := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings",
		`{"port_forwarding":{"internal_port_id":"`+p+`","internal_ip_address":"`+x+`","external_port":2225,"internal_port":22,"protocol":"tcp"}}`), "port_forwarding")
	other, y := rig.port(t, network)
	rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings",
		`{"port_forwarding":{"internal_port_id":"`+other+`","internal_ip_address":"`+y+`","external_port":2226,"internal_port":22,"protocol":"tcp"}}`)

	rig.routers.failFirst(1, errors.New("the network node could not converge the router"))
	code, body := rig.do(t, http.MethodDelete, "/v2.0/ports/"+p, "")
	rig.routers.failFirst(0, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM port_forwardings WHERE id = $1", pf).Scan(&n); err != nil || n != 1 {
		t.Errorf("the port forwarding after the failed delete: %d row(s) (%v); want it kept with its port", n, err)
	}
	if s := rig.held(t, routerID); len(s.PortForwardings) != 2 {
		t.Errorf("the router after the failed delete: %+v; want both port forwardings held again", s.PortForwardings)
	}

	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/ports/"+p, "")
	if s := rig.held(t, routerID); len(s.PortForwardings) != 1 || s.PortForwardings[0].InternalIP != y {
		t.Errorf("after the port's delete: %+v; want only the other port's forwarding, to %s", s.PortForwardings, y)
	}
}

// SPEC-033 T184 (FR-065): refusing to delete a router's port through the port API changes no
// node: nothing is withdrawn to be put back.
func TestRouterUnitRefusedPortDeleteConvergesNothing(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	routerID, _ := rig.router(t)
	var iface string
	if err := testPool.QueryRow(context.Background(), "SELECT port_id::text FROM router_interfaces WHERE router_id = $1", routerID).Scan(&iface); err != nil {
		t.Fatal(err)
	}
	before := len(rig.caller.recorded())
	if code, body := rig.do(t, http.MethodDelete, "/v2.0/ports/"+iface, ""); code != http.StatusConflict {
		t.Fatalf("delete the interface's port: code=%d body=%s; want 409", code, body)
	}
	if calls := rig.caller.recorded()[before:]; len(calls) != 0 {
		t.Errorf("the refused delete made %d node call(s): %+v; want none", len(calls), calls)
	}
}

func TestRouterUnitFloatingIPDeleteTakesItsPortForwardings(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	routerID, network := rig.router(t)
	p, x := rig.port(t, network)
	fip, f := rig.floatingIP(t, "")
	mustExec(t, "UPDATE floating_ips SET router_id = $1 WHERE id = $2", routerID, fip)
	rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings",
		`{"port_forwarding":{"internal_port_id":"`+p+`","internal_ip_address":"`+x+`","external_port":2224,"internal_port":22,"protocol":"tcp"}}`)
	if s := rig.held(t, routerID); len(s.PortForwardings) != 1 {
		t.Fatalf("before: %+v; want the port forwarding", s.PortForwardings)
	}
	rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+fip, "")
	if s := rig.held(t, routerID); len(s.PortForwardings) != 0 || slices.Contains(networking.PlanRouter(s).Floating, f+"/32") {
		t.Errorf("after the floating IP's delete: %+v; want its port forwardings and address gone", s)
	}
}
