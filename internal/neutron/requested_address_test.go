package neutron

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T198 (FR-032, FR-038, FR-053, FR-054): an address requested for a port, a
// router's gateway or a floating IP, and one Nova checks for an interface, pass one check, upstream's
// IPAM's (_verify_ip): a host address of the subnet, held by nothing — 409 IpAddressAlreadyAllocated
// otherwise. The subnet's gateway is no exception, as upstream's: only the pools leave it out, so a free
// one is allocated and a held one, a router interface's say, answers 409 on every path alike.

func TestRequestedAddressesPassOneCheck(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	manager := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member,manager"}
	ctx := context.Background()

	t.Run("a port, and Nova's check", func(t *testing.T) {
		gw := "10.73.0.1"
		network := seedNetwork(t, "spec033-src26-"+uuid.NewString()[:6])
		subnet := seedSubnet(t, network, "spec033-src26", "10.73.0.0/24", &gw, 4, false)
		t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", network) })
		if ok, err := IPAvailable(ctx, testPool, subnet, gw); err != nil || !ok {
			t.Errorf("IPAvailable of the free gateway: %v (%v); want true", ok, err)
		}
		body := `{"port":{"network_id":"` + network + `","fixed_ips":[{"subnet_id":"` + subnet + `","ip_address":"` + gw + `"}]}}`
		if code, resp := doJSONReq(r, http.MethodPost, "/v2.0/ports", body, member); code != http.StatusCreated {
			t.Errorf("a port on the free gateway: code=%d body=%s; want 201", code, resp)
		}
		code, resp := doJSONReq(r, http.MethodPost, "/v2.0/ports", body, member)
		if code != http.StatusConflict || neutronErrorType(t, resp) != "IpAddressAlreadyAllocated" {
			t.Errorf("a port on the gateway a port holds: code=%d body=%s; want 409 IpAddressAlreadyAllocated", code, resp)
		}
		if ok, err := IPAvailable(ctx, testPool, subnet, gw); err != nil || ok {
			t.Errorf("IPAvailable of the held gateway: %v (%v); want false", ok, err)
		}
	})

	t.Run("a floating IP", func(t *testing.T) {
		prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
		gw := prefix + ".1"
		network := seedExternalNetwork(t, "spec033-src26-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
		subnet := seedSubnet(t, network, "spec033-src26-ext", prefix+".0/24", &gw, 4, false)
		storeTestPools(t, subnet, [2]string{prefix + ".10", prefix + ".20"})
		cleanupFloatingIPsOn(t, network)
		chosen := `{"floatingip":{"floating_network_id":"` + network + `","floating_ip_address":"` + gw + `"}}`
		if code, resp, address := createFloatingIP(r, manager, chosen); code != http.StatusCreated || address != gw {
			t.Errorf("a floating IP on the free gateway: code=%d address=%s body=%s; want 201 %s", code, address, resp, gw)
		}
		code, resp, _ := createFloatingIP(r, manager, chosen)
		if code != http.StatusConflict || neutronErrorType(t, resp) != "IpAddressAlreadyAllocated" {
			t.Errorf("a floating IP on the gateway a floating IP holds: code=%d body=%s; want 409 IpAddressAlreadyAllocated", code, resp)
		}
	})

	t.Run("a router's gateway", func(t *testing.T) {
		rr := newRouterRig(t)
		code, body, _ := rr.createRouter(t, rr.admin, `{"network_id":"`+rr.external+`","external_fixed_ips":[{"ip_address":"`+rr.gatewayOfExternal+`"}]}`)
		if gw := gatewayOf(t, body); code != http.StatusCreated || gw == nil || len(gw.ExternalFixedIPs) != 1 || gw.ExternalFixedIPs[0].IPAddress != rr.gatewayOfExternal {
			t.Errorf("a router gateway on the external subnet's free gateway: code=%d body=%s; want 201 holding %s", code, body, rr.gatewayOfExternal)
		}
	})
}
