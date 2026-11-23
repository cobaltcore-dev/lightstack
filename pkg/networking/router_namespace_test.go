package networking

import (
	"slices"
	"strings"
	"testing"
)

// SPEC-033 round 2, T112 (FR-033, FR-035, FR-037, FR-038, FR-049; research R28, R37;
// contracts/router-namespace-dataplane.md): a router's state on the network node, rendered from its
// records by pure functions — the namespace's nat and mangle tables, its devices, addresses and
// routes, and what the host holds for it.

const (
	testRouterID  = "6f1c2d3e-4a5b-4c6d-8e7f-901a2b3c4d5e"
	testGatewayID = "aa11bb22cc33-4d5e-8f90-a1b2c3d4e5f6"
	testIface1    = "bb22cc33dd44-4e5f-9a01-b2c3d4e5f6a7"
	testIface2    = "cc33dd44ee55-4f60-8b12-c3d4e5f6a7b8"
	testNetwork1  = "1a2b3c4d-0000-4000-8000-000000000001"
	testNetwork2  = "2b3c4d5e-0000-4000-8000-000000000002"
	testPFID      = "9f8e7d6c-5b4a-4321-8765-0fedcba98765"
)

// testRouter has a gateway with SNAT on, two interfaces, two floating IPs on ports and a port
// forwarding on a third floating IP, which has no port.
func testRouter() RouterState {
	return RouterState{
		ID: testRouterID,
		Gateway: &RouterGateway{
			PortID:    testGatewayID,
			Addresses: []string{"203.0.113.2/24"},
			NextHop:   "203.0.113.1",
			SNAT:      true,
		},
		Interfaces: []RouterInterface{
			{PortID: testIface2, Addresses: []string{"10.0.2.1/24"}, Bridge: BridgeNameForNetwork(testNetwork2)},
			{PortID: testIface1, Addresses: []string{"10.0.1.1/24"}, Bridge: BridgeNameForNetwork(testNetwork1)},
		},
		FloatingIPs: []RouterFloatingIP{
			{Address: "203.0.113.11", FixedIP: "10.0.2.7"},
			{Address: "203.0.113.10", FixedIP: "10.0.1.5"},
		},
		PortForwardings: []RouterPortForwarding{
			{ID: testPFID, FloatingIP: "203.0.113.12", Protocol: "tcp", ExternalPort: 2222, InternalIP: "10.0.1.6", InternalPort: 22},
		},
	}
}

func TestRenderRouterNAT(t *testing.T) {
	const qg = "qg-aa11bb22cc3"
	want := strings.Join([]string{
		"*nat",
		":PREROUTING ACCEPT [0:0]",
		":INPUT ACCEPT [0:0]",
		":OUTPUT ACCEPT [0:0]",
		":POSTROUTING ACCEPT [0:0]",
		":ls-l3-PREROUTING - [0:0]",
		":ls-l3-OUTPUT - [0:0]",
		":ls-l3-POSTROUTING - [0:0]",
		":ls-l3-snat - [0:0]",
		":ls-l3-float-snat - [0:0]",
		":ls-l3-fip-pf - [0:0]",
		":ls-pf-9f8e7d6c - [0:0]",
		"-A PREROUTING -m comment --comment lightstack -j ls-l3-PREROUTING",
		"-A OUTPUT -m comment --comment lightstack -j ls-l3-OUTPUT",
		"-A POSTROUTING -m comment --comment lightstack -j ls-l3-POSTROUTING",
		"-A POSTROUTING -m comment --comment lightstack -j ls-l3-snat",
		"-A ls-l3-PREROUTING -m comment --comment lightstack -j ls-l3-fip-pf",
		"-A ls-l3-PREROUTING -d 203.0.113.10/32 -m comment --comment lightstack -j DNAT --to-destination 10.0.1.5",
		"-A ls-l3-PREROUTING -d 203.0.113.11/32 -m comment --comment lightstack -j DNAT --to-destination 10.0.2.7",
		"-A ls-l3-OUTPUT -d 203.0.113.10/32 -m comment --comment lightstack -j DNAT --to-destination 10.0.1.5",
		"-A ls-l3-OUTPUT -d 203.0.113.11/32 -m comment --comment lightstack -j DNAT --to-destination 10.0.2.7",
		"-A ls-l3-POSTROUTING ! -o " + qg + " -m conntrack ! --ctstate DNAT -m comment --comment lightstack -j ACCEPT",
		"-A ls-l3-snat -m comment --comment lightstack -j ls-l3-float-snat",
		"-A ls-l3-snat -o " + qg + " -m comment --comment lightstack -j SNAT --to-source 203.0.113.2",
		"-A ls-l3-snat -m mark ! --mark 0x2/0xffff -m conntrack --ctstate DNAT -m comment --comment lightstack -j SNAT --to-source 203.0.113.2",
		"-A ls-l3-float-snat -s 10.0.1.5/32 -m comment --comment lightstack -j SNAT --to-source 203.0.113.10",
		"-A ls-l3-float-snat -s 10.0.2.7/32 -m comment --comment lightstack -j SNAT --to-source 203.0.113.11",
		"-A ls-l3-fip-pf -m comment --comment lightstack -j ls-pf-9f8e7d6c",
		"-A ls-pf-9f8e7d6c -d 203.0.113.12/32 -p tcp -m tcp --dport 2222 -m comment --comment lightstack -j DNAT --to-destination 10.0.1.6:22",
		"COMMIT",
		"",
	}, "\n")
	if got := renderRouterNAT(testRouter()); got != want {
		t.Errorf("nat payload:\n%s\nwant:\n%s", got, want)
	}

	// With SNAT off only the gateway's SNAT rule goes, as upstream's _add_snat_rules: the exception
	// for traffic between the router's own networks and the hairpin rule stay, or a VM with a
	// floating IP would reach another network of the router with its floating address.
	off := testRouter()
	off.Gateway.SNAT = false
	got := renderRouterNAT(off)
	if strings.Contains(got, "-o "+qg+" -m comment --comment lightstack -j SNAT") {
		t.Error("SNAT off: the gateway's SNAT rule is still rendered")
	}
	for _, kept := range []string{"! -o " + qg + " -m conntrack ! --ctstate DNAT", "-m mark ! --mark 0x2/0xffff"} {
		if !strings.Contains(got, kept) {
			t.Errorf("SNAT off: %q is gone; want it kept", kept)
		}
	}

	// A router without a gateway has no SNAT, no exception and no hairpin: nothing leaves it.
	bare := RouterState{ID: testRouterID, Interfaces: testRouter().Interfaces}
	got = renderRouterNAT(bare)
	for _, gone := range []string{"SNAT", "ACCEPT\n-A", "--mark"} {
		if strings.Contains(got, gone) {
			t.Errorf("no gateway: %q rendered", gone)
		}
	}
}

func TestRenderRouterMangle(t *testing.T) {
	want := strings.Join([]string{
		"*mangle",
		":PREROUTING ACCEPT [0:0]",
		":INPUT ACCEPT [0:0]",
		":FORWARD ACCEPT [0:0]",
		":OUTPUT ACCEPT [0:0]",
		":POSTROUTING ACCEPT [0:0]",
		":ls-l3-mark - [0:0]",
		"-A PREROUTING -m comment --comment lightstack -j ls-l3-mark",
		"-A ls-l3-mark -i qg-aa11bb22cc3 -m comment --comment lightstack -j MARK --set-xmark 0x2/0xffff",
		"COMMIT",
		"",
	}, "\n")
	if got := renderRouterMangle(testRouter()); got != want {
		t.Errorf("mangle payload:\n%s\nwant:\n%s", got, want)
	}
	if got := renderRouterMangle(RouterState{ID: testRouterID}); strings.Contains(got, "MARK") {
		t.Errorf("no gateway: a mark rule rendered:\n%s", got)
	}
}

func TestPlanRouter(t *testing.T) {
	p := PlanRouter(testRouter())
	if p.Namespace != "qrouter-"+testRouterID {
		t.Errorf("namespace %q", p.Namespace)
	}
	wantLegs := []Leg{
		{Inside: "qg-aa11bb22cc3", Outside: "lg-aa11bb22cc3"},
		{Inside: "qr-bb22cc33dd4", Outside: "lr-bb22cc33dd4", Bridge: BridgeNameForNetwork(testNetwork1)},
		{Inside: "qr-cc33dd44ee5", Outside: "lr-cc33dd44ee5", Bridge: BridgeNameForNetwork(testNetwork2)},
	}
	if !slices.Equal(p.Legs, wantLegs) {
		t.Errorf("legs %+v; want %+v", p.Legs, wantLegs)
	}
	wantAddrs := map[string][]string{
		"qg-aa11bb22cc3": {"203.0.113.2/24"},
		"qr-bb22cc33dd4": {"10.0.1.1/24"},
		"qr-cc33dd44ee5": {"10.0.2.1/24"},
	}
	if len(p.Addresses) != len(wantAddrs) {
		t.Errorf("addresses %v; want %v", p.Addresses, wantAddrs)
	}
	for dev, want := range wantAddrs {
		if !slices.Equal(p.Addresses[dev], want) {
			t.Errorf("addresses of %s: %v; want %v", dev, p.Addresses[dev], want)
		}
	}
	// Each floating address with a port or a port forwarding, as /32 on the external leg.
	if want := []string{"203.0.113.10/32", "203.0.113.11/32", "203.0.113.12/32"}; !slices.Equal(p.Floating, want) {
		t.Errorf("floating addresses %v; want %v", p.Floating, want)
	}
	if p.DefaultRoute != "203.0.113.1" {
		t.Errorf("default route via %q; want 203.0.113.1", p.DefaultRoute)
	}
	// The host routes and proxies the gateway's address and every floating address to the leg.
	if want := []string{"203.0.113.2", "203.0.113.10", "203.0.113.11", "203.0.113.12"}; !slices.Equal(p.HostAddresses, want) {
		t.Errorf("host routes and proxy entries for %v; want %v", p.HostAddresses, want)
	}
	if p.HostLeg != "lg-aa11bb22cc3" {
		t.Errorf("host leg %q", p.HostLeg)
	}

	// A router without a gateway has internal legs only, and nothing on the host.
	bare := PlanRouter(RouterState{ID: testRouterID, Interfaces: testRouter().Interfaces})
	if len(bare.Legs) != 2 || bare.DefaultRoute != "" || len(bare.HostAddresses) != 0 || bare.HostLeg != "" || len(bare.Floating) != 0 {
		t.Errorf("no gateway: %+v", bare)
	}
}
