package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T142 (FR-010 as restated by FR-042, FR-033, FR-036, FR-041; research R24, R26,
// R28, R29, R32; contracts/router-namespace-dataplane.md and port-firewall.md, Reconciliation): the
// reconciler converges the units its node holds, both ways. The network node converges every router
// and network DHCP with a record and removes those without one, with their listeners and stray proxy
// entries, after a complete read only; any other node removes every router and DHCP namespace it finds;
// every node converges the chains of the ports bound to it, which removes the chains of any other.

const (
	reconcileNetworkNode = "spec033-reconcile-net"
	reconcileComputeNode = "spec033-reconcile-cmp"
)

// reconcileNode is one node of the rig: its service and every seam that reaches its host.
type reconcileNode struct {
	svc       *Service
	routers   *fakeRouters
	dhcp      *fakeDHCP
	listeners *fakeListeners
	firewall  *fakeFirewall
	retired   *fakeRetired
	pass      *NetworkReconciler
}

// reconcileRig is two nodes on one database: the network node and a compute node, which answers the
// requests. The fake node caller carries the units between them. Each node's passes read only the
// records this test made, as if the database held nothing else: the package's tests share one database,
// and a pass over all of it would converge, and write the DHCP ports of, other tests' networks.
type reconcileRig struct {
	r              *gin.Engine // the compute node's routes
	net, cmp       *reconcileNode
	hdr            map[string]string
	external       string
	externalPrefix string
	mu             sync.Mutex
	ours           map[string]bool
}

func newReconcileRig(t *testing.T) *reconcileRig {
	t.Helper()
	requireDB(t)
	rig := &reconcileRig{hdr: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"},
		ours: map[string]bool{}}
	node := func(name string) (*gin.Engine, *reconcileNode) {
		r, svc := newNeutronTestRouterWithService()
		svc.SetNodes(NodeOptions{Name: name, NetworkNode: reconcileNetworkNode})
		n := &reconcileNode{svc: svc, routers: newFakeRouters(), dhcp: newFakeDHCP(), listeners: newFakeListeners(),
			firewall: newFakeFirewall(), retired: &fakeRetired{}}
		svc.routers, svc.dhcp, svc.firewall, svc.retired = n.routers, n.dhcp, n.firewall, n.retired
		svc.brManager = newFakeBridges()
		svc.SetMetadataListeners(n.listeners)
		svc.uplink = func() (string, error) { return "eth0", nil }
		n.pass = NewNetworkReconciler(svc, 0)
		read := n.pass.readIDs
		n.pass.readIDs = func(ctx context.Context, query string, args ...any) ([]string, bool, error) {
			all, complete, err := read(ctx, query, args...)
			rig.mu.Lock()
			defer rig.mu.Unlock()
			var mine []string
			for _, id := range all {
				if rig.ours[id] {
					mine = append(mine, id)
				}
			}
			return mine, complete, err
		}
		return r, n
	}
	rig.r, rig.cmp = node(reconcileComputeNode)
	_, rig.net = node(reconcileNetworkNode)
	caller := newFakeNodeCaller(rig.cmp.svc, rig.net.svc)
	rig.cmp.svc.caller, rig.net.svc.caller = caller, caller

	rig.externalPrefix = fmt.Sprintf("198.19.%d", rand.IntN(250))
	rig.external = seedExternalNetwork(t, "spec033-reconcile-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
	gateway := rig.externalPrefix + ".1"
	subnet := seedSubnet(t, rig.external, "spec033-reconcile-ext", rig.externalPrefix+".0/24", &gateway, 4, false)
	storeTestPools(t, subnet, [2]string{rig.externalPrefix + ".10", rig.externalPrefix + ".99"})
	rig.own(rig.external)
	return rig
}

// own adds records to what the passes read.
func (rig *reconcileRig) own(ids ...string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	for _, id := range ids {
		rig.ours[id] = true
	}
}

func (rig *reconcileRig) must(t *testing.T, want int, method, path, body string) string {
	t.Helper()
	code, resp := doJSONReq(rig.r, method, path, body, rig.hdr)
	if code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", method, path, code, resp, want)
	}
	return resp
}

// topology makes a router with a gateway and an interface on a new network with a DHCP subnet, a port
// on that network and a floating IP on the port, all through the API.
type reconcileTopology struct {
	router, network, port, floatingIP, floatingAddress string
}

func (rig *reconcileRig) topology(t *testing.T) reconcileTopology {
	t.Helper()
	var top reconcileTopology
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers",
		`{"router":{"name":"spec033-reconcile","external_gateway_info":{"network_id":"`+rig.external+`"}}}`)
	top.router = createdID(t, resp, "router")
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM floating_ips WHERE router_id = $1", top.router)
		mustExec(t, "DELETE FROM router_interfaces WHERE router_id = $1", top.router)
		mustExec(t, "DELETE FROM ports WHERE device_id = $1", top.router)
		mustExec(t, "DELETE FROM routers WHERE id = $1", top.router)
	})
	top.network = seedNetwork(t, "spec033-reconcile-net-"+uuid.NewString()[:6])
	rig.own(top.router, top.network)
	resp = rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+top.network+`","cidr":"10.79.0.0/24","ip_version":4,"gateway_ip":"10.79.0.1"}}`)
	subnet := createdID(t, resp, "subnet")
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+top.router+"/add_router_interface", `{"subnet_id":"`+subnet+`"}`)
	top.port = createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/ports",
		`{"port":{"network_id":"`+top.network+`"}}`), "port")
	var fip struct {
		FloatingIP struct {
			ID      string `json:"id"`
			Address string `json:"floating_ip_address"`
		} `json:"floatingip"`
	}
	resp = rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips",
		`{"floatingip":{"floating_network_id":"`+rig.external+`","port_id":"`+top.port+`"}}`)
	if err := json.Unmarshal([]byte(resp), &fip); err != nil || fip.FloatingIP.ID == "" {
		t.Fatalf("no floating IP in %s (%v)", resp, err)
	}
	top.floatingIP, top.floatingAddress = fip.FloatingIP.ID, fip.FloatingIP.Address
	return top
}

func (n *reconcileNode) reconcile(ctx context.Context) { n.pass.reconcile(ctx) }

func TestReconcileNetworkNodeConvergesEveryUnitWithARecord(t *testing.T) {
	rig := newReconcileRig(t)
	top := rig.topology(t)
	ctx := context.Background()

	// The network node lost the router and the network's DHCP — namespaces deleted out of band, a
	// dnsmasq that died with an earlier process — and holds a router and a network's DHCP no record
	// names, with a metadata listener, as an earlier process left them.
	rig.net.routers.drop(top.router)
	rig.net.dhcp.drop(top.network)
	strayRouter, strayNetwork := uuid.NewString(), uuid.NewString()
	rig.net.routers.plant(strayRouter, networking.RouterState{ID: strayRouter})
	rig.net.dhcp.plant(strayNetwork, networking.DHCPState{NetworkID: strayNetwork})
	rig.net.listeners.plant(strayNetwork)

	rig.net.reconcile(ctx)

	want, found, err := readRouterState(ctx, testPool, top.router, nil)
	if err != nil || !found {
		t.Fatalf("read the router's records: found %v, %v", found, err)
	}
	if got, ok := rig.net.routers.state(top.router); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("the router after the pass: %+v (held %v); want it as its records say: %+v", got, ok, want)
	}
	if _, ok := rig.net.dhcp.state(top.network); !ok || !rig.net.listeners.serving(top.network) {
		t.Error("the network's DHCP and its metadata listener were not converged back")
	}
	if _, ok := rig.net.routers.state(strayRouter); ok {
		t.Error("a router no record names was kept")
	}
	if _, ok := rig.net.dhcp.state(strayNetwork); ok || rig.net.listeners.serving(strayNetwork) {
		t.Error("a network's DHCP or listener no record names was kept")
	}

	t.Run("a unit as its records say is converged to the same, never removed", func(t *testing.T) {
		before := len(rig.net.routers.recorded())
		rig.net.reconcile(ctx)
		for _, c := range rig.net.routers.recorded()[before:] {
			if c.ID == top.router && c.State == nil {
				t.Fatal("the pass converged a router with a record to absent")
			}
		}
		if got, _ := rig.net.routers.state(top.router); !reflect.DeepEqual(got, want) {
			t.Errorf("the router after a second pass: %+v; want %+v", got, want)
		}
	})

	t.Run("the compute node converged no router and no DHCP", func(t *testing.T) {
		if calls := rig.cmp.routers.recorded(); len(calls) != 0 {
			t.Errorf("the compute node converged routers %v", calls)
		}
		if calls := rig.cmp.dhcp.recorded(); len(calls) != 0 {
			t.Errorf("the compute node converged DHCP %v", calls)
		}
	})
}

// Only a complete read of the records shows a unit has none: a pass whose read was partial, or failed,
// removes nothing, and still converges what it did read.
func TestReconcileNetworkNodeRemovesNothingAfterAnIncompleteRead(t *testing.T) {
	rig := newReconcileRig(t)
	top := rig.topology(t)
	strayRouter, strayNetwork := uuid.NewString(), uuid.NewString()
	rig.net.routers.plant(strayRouter, networking.RouterState{ID: strayRouter})
	rig.net.dhcp.plant(strayNetwork, networking.DHCPState{NetworkID: strayNetwork})
	rig.net.listeners.plant(strayNetwork)
	strays := func(t *testing.T) {
		t.Helper()
		if _, ok := rig.net.routers.state(strayRouter); !ok {
			t.Error("a router was removed after an incomplete read")
		}
		if _, ok := rig.net.dhcp.state(strayNetwork); !ok || !rig.net.listeners.serving(strayNetwork) {
			t.Error("a network's DHCP or listener was removed after an incomplete read")
		}
	}

	t.Run("a partial read", func(t *testing.T) {
		read := rig.net.pass.readIDs
		rig.net.pass.readIDs = func(ctx context.Context, query string, args ...any) ([]string, bool, error) {
			ids, _, err := read(ctx, query, args...)
			return ids, false, err
		}
		defer func() { rig.net.pass.readIDs = read }()
		rig.net.routers.drop(top.router)
		rig.net.reconcile(context.Background())
		strays(t)
		if _, ok := rig.net.routers.state(top.router); !ok {
			t.Error("a router the partial read did read was not converged")
		}
	})

	t.Run("a failed read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rig.net.reconcile(ctx)
		strays(t)
	})
}

// Any other node holds no router and no network's DHCP: whatever it finds, it removes, records or not —
// it was the network node once, say.
func TestReconcileOtherNodeRemovesEveryRouterAndDHCP(t *testing.T) {
	rig := newReconcileRig(t)
	top := rig.topology(t)
	stray := uuid.NewString()
	rig.cmp.routers.plant(top.router, networking.RouterState{ID: top.router})
	rig.cmp.routers.plant(stray, networking.RouterState{ID: stray})
	rig.cmp.dhcp.plant(top.network, networking.DHCPState{NetworkID: top.network})
	rig.cmp.listeners.plant(top.network)

	rig.cmp.reconcile(context.Background())

	if held := rig.cmp.routers.heldIDs(); len(held) != 0 {
		t.Errorf("the compute node still holds routers %q", held)
	}
	if held := rig.cmp.dhcp.heldIDs(); len(held) != 0 || rig.cmp.listeners.serving(top.network) {
		t.Errorf("the compute node still holds DHCP %q (listener %v)", held, rig.cmp.listeners.serving(top.network))
	}
	for _, c := range rig.cmp.routers.recorded() {
		if c.State != nil {
			t.Errorf("the compute node converged router %s to a state: only the network node holds routers", c.ID)
		}
	}
	if _, ok := rig.net.routers.state(top.router); !ok {
		t.Error("the network node's router went with the compute node's")
	}
}

// Every node converges the chains of the ports bound to it, so a port's chains on a node it is not
// bound to — its binding moved while that node was down, say — are removed, after a complete read.
func TestReconcileEveryNodeConvergesItsBoundPorts(t *testing.T) {
	rig := newReconcileRig(t)
	top := rig.topology(t)
	bind(t, top.port, reconcileComputeNode)
	stray := networking.FirewallPort{ID: uuid.NewString(), MAC: "fa:16:3e:00:00:99", Addresses: []string{"10.79.0.99"}}
	rig.cmp.firewall.plant(stray)
	rig.net.firewall.plant(stray)

	rig.cmp.reconcile(context.Background())
	rig.net.reconcile(context.Background())

	if got := rig.cmp.firewall.heldPorts(); !slices.Equal(got, []string{top.port}) {
		t.Errorf("the compute node holds the chains of %q; want its bound port %s alone", got, top.port)
	}
	if got := rig.net.firewall.heldPorts(); len(got) != 0 {
		t.Errorf("the network node holds the chains of %q; no port is bound to it", got)
	}

	t.Run("a failed read removes nothing", func(t *testing.T) {
		rig.cmp.firewall.plant(stray)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rig.cmp.reconcile(ctx)
		if got := rig.cmp.firewall.heldPorts(); !slices.Contains(got, stray.ID) {
			t.Errorf("the compute node holds %q after a failed read; want the stray chains kept", got)
		}
	})
}

// The network node removes the proxy entries on its uplink that no router's leg routes, when a record
// makes their address Lightstack's: a floating IP's, a router gateway's, or one in an external network's
// pools. Entries a router routes, and addresses no record knows, are left alone.
func TestReconcileNetworkNodeRemovesStrayProxies(t *testing.T) {
	rig := newReconcileRig(t)
	top := rig.topology(t)
	recorded := rig.externalPrefix + ".201" // in the subnet, outside its pools: only its record makes it ours
	seedFloatingAddress(t, rig.external, recorded)
	var pooled string // in the pools, held by nothing
	held, _ := rig.net.routers.state(top.router)
	for i := 99; i > 10 && pooled == ""; i-- {
		a := fmt.Sprintf("%s.%d", rig.externalPrefix, i)
		if !slices.Contains(networking.PlanRouter(held).HostAddresses, a) {
			pooled = a
		}
	}
	foreign := "240.0.0.7"
	rig.net.routers.proxy(recorded, pooled, foreign, top.floatingAddress)

	rig.net.reconcile(context.Background())

	for address, want := range map[string]bool{recorded: false, pooled: false, foreign: true} {
		if got := rig.net.routers.proxied(address); got != want {
			t.Errorf("%s proxied after the pass: %v; want %v", address, got, want)
		}
	}
	if s, _ := rig.net.routers.state(top.router); !slices.Contains(networking.PlanRouter(s).HostAddresses, top.floatingAddress) {
		t.Errorf("the router's floating address %s is no longer among its host addresses", top.floatingAddress)
	}

	// No other node may answer for a router's or a floating address (FR-033): one that was the network
	// node once would answer alongside the new one.
	t.Run("and on any other node, which routes none", func(t *testing.T) {
		rig.cmp.routers.proxy(pooled, top.floatingAddress, foreign)
		rig.cmp.reconcile(context.Background())
		for address, want := range map[string]bool{pooled: false, top.floatingAddress: false, foreign: true} {
			if got := rig.cmp.routers.proxied(address); got != want {
				t.Errorf("%s proxied on the compute node after its pass: %v; want %v", address, got, want)
			}
		}
	})
}
