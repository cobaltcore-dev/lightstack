package neutron

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T260: branches of round 2's API that no test reached.
func TestRoundTwoBranchesNoTestReached(t *testing.T) {
	requireDB(t)

	t.Run("a floating IP on a port with two IPv4 addresses names one, and an IPv4 one", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		port := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+network+`","fixed_ips":[{"ip_address":"10.77.0.21"},{"ip_address":"10.77.0.22"}]}}`), "port")
		for fixed, want := range map[string]string{
			"":                                  "Port " + port + " has multiple fixed IPv4 addresses.  Must provide a specific IPv4 address when assigning a floating IP",
			`,"fixed_ip_address":"2001:db8::5"`: "Cannot process floating IP association with 2001:db8::5, since that is not an IPv4 address",
		} {
			code, body := rig.do(t, http.MethodPost, "/v2.0/floatingips",
				`{"floatingip":{"floating_network_id":"`+rig.external+`","port_id":"`+port+`"`+fixed+`}}`)
			if code != http.StatusBadRequest || neutronErrorMessage(t, body) != "Bad floatingip request: "+want+"." {
				t.Errorf("fixed %q: code=%d body=%s; want 400 %q", fixed, code, body, want)
			}
		}
	})

	t.Run("an exhausted subnet answers IpAddressGenerationFailure", func(t *testing.T) {
		r := newNeutronTestRouter()
		member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
		gw := "10.96.0.1"
		network := seedNetwork(t, "spec033-t260-"+uuid.NewString()[:6])
		subnet := seedSubnet(t, network, "spec033-t260", "10.96.0.0/29", &gw, 4, false)
		storeTestPools(t, subnet, [2]string{"10.96.0.3", "10.96.0.3"})
		takeAddress(t, network, subnet, "10.96.0.3")
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`","fixed_ips":[{"subnet_id":"`+subnet+`"}]}}`, member)
		if code != http.StatusConflict || neutronErrorType(t, body) != "IpAddressGenerationFailure" {
			t.Errorf("port: code=%d body=%s; want 409 IpAddressGenerationFailure", code, body)
		}
		rr := newRouterRig(t)
		takeAddresses(t, rr.external, rr.subnet, rr.prefix, 10, 20)
		code, body, _ = rr.createRouter(t, rr.admin, `{"network_id":"`+rr.external+`"}`)
		if code != http.StatusConflict || neutronErrorType(t, body) != "IpAddressGenerationFailure" {
			t.Errorf("router gateway: code=%d body=%s; want 409 IpAddressGenerationFailure", code, body)
		}
	})

	t.Run("a forwarding to a port another router reaches is refused once the floating IP records one", func(t *testing.T) {
		rig := newUnitRig(t)
		_, first := rig.router(t)
		_, second := rig.router(t)
		p, x := rig.port(t, first)
		q, y := rig.port(t, second)
		fip, _ := rig.floatingIP(t, "")
		rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2401))
		code, body := rig.do(t, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(q, y, 2402))
		if code != http.StatusBadRequest {
			t.Errorf("code=%d body=%s; want 400: the floating IP had been set on the first router", code, body)
		}
	})

	t.Run("a gateway change the network node cannot apply puts the pools written with it back", func(t *testing.T) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-t260-"+uuid.NewString()[:6])
		subnet := rig.subnet(t, rig.hdr, network, `"cidr":"10.97.0.0/24","allocation_pools":[{"start":"10.97.0.10","end":"10.97.0.100"}]`)
		var once sync.Once
		rig.dhcp.hook = func(holderCall[networking.DHCPState]) error {
			var err error
			once.Do(func() { err = errNode })
			return err
		}
		if code, body := rig.do(t, rig.hdr, http.MethodPut, "/v2.0/subnets/"+subnet,
			`{"subnet":{"gateway_ip":"10.97.0.254","allocation_pools":[{"start":"10.97.0.20","end":"10.97.0.30"}]}}`); code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s; want 500", code, body)
		}
		var start string
		if err := testPool.QueryRow(context.Background(), "SELECT first_ip FROM subnet_allocation_pools WHERE subnet_id = $1", subnet).Scan(&start); err != nil || start != "10.97.0.10" {
			t.Errorf("pool after the failed update starts at %q (%v); want 10.97.0.10 back", start, err)
		}
	})
}

// takeAddresses takes prefix.from through prefix.to on subnet of network, as concurrent allocations would.
func takeAddresses(t *testing.T, network, subnet, prefix string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		takeAddress(t, network, subnet, prefix+"."+strconv.Itoa(i))
	}
}

// SPEC-033 T260: an interface removal re-reads the interface under the router's lock once the network node
// has applied it; an interface replaced meanwhile is a 409, and the router converged from the records again.
func TestInterfaceRemovalRefusesAnInterfaceReplacedMeanwhile(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	router, network := rig.router(t)
	var subnet string
	if err := testPool.QueryRow(context.Background(), "SELECT id::text FROM subnets WHERE network_id = $1", network).Scan(&subnet); err != nil {
		t.Fatal(err)
	}
	replacement := seedProjectPort(t, seededProjectID, network)
	var once sync.Once
	rig.routers.hook = func(holderCall[networking.RouterState]) error {
		once.Do(func() {
			mustExec(t, "UPDATE router_interfaces SET port_id = $1 WHERE router_id = $2", replacement, router)
		})
		return nil
	}
	code, body := rig.do(t, http.MethodPut, "/v2.0/routers/"+router+"/remove_router_interface", `{"subnet_id":"`+subnet+`"}`)
	if code != http.StatusConflict {
		t.Errorf("code=%d body=%s; want 409: the interface changed meanwhile", code, body)
	}
}
