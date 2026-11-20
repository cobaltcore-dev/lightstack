package neutron

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Branch review SRC-2 (2026-11-23-review.md): a router's ports were found by device_owner and
// device_id, and port create and update store both as any member sends them. A member's port claiming
// to be their router's gateway became its gateway — setting the gateway kept it, at the address the
// member chose, where only an administrator chooses one (FR-038) — and a floating IP found its router
// through ports claiming an interface or a gateway. A router's gateway is the one port with no project,
// which only the router code writes, and its interfaces are its router_interfaces rows.

// forgeRouterPort stores what port create stores for a member who names a router as the device of a
// port of their own: the owner they chose, in their project. It is seeded, not created through the API,
// so the test reads the same on a tree whose port create does not yet honour fixed_ips.
func forgeRouterPort(t *testing.T, owner, routerID, networkID, subnetID, ip string) string {
	t.Helper()
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, 'src2-forged', $2, $3, $4, $5, $6, 'ACTIVE', $7::jsonb)`,
		port, networkID, seededProjectID, routerID, owner,
		fmt.Sprintf("fa:16:3e:52:%02x:%02x", rand.IntN(256), rand.IntN(256)),
		`[{"subnet_id":"`+subnetID+`","ip_address":"`+ip+`"}]`)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", port) })
	return port
}

// sharedExternal seeds an external network of another project that every project may make ports on,
// with one subnet whose pools are .10–.20.
func sharedExternal(t *testing.T) (network, subnet, prefix string) {
	t.Helper()
	prefix = fmt.Sprintf("198.19.%d", rand.IntN(250))
	owner := createTestProject(t, "src2-external-owner")
	network = seedExternalNetwork(t, "src2-shared-ext-"+uuid.NewString()[:6], owner, true, true)
	gateway := prefix + ".1"
	subnet = seedSubnet(t, network, "src2-shared-ext", prefix+".0/24", &gateway, 4, false)
	storeTestPools(t, subnet, [2]string{prefix + ".10", prefix + ".20"})
	cleanupFloatingIPsOn(t, network)
	return network, subnet, prefix
}

func portFixedIPs(t *testing.T, port string) (string, bool) {
	t.Helper()
	var fixed string
	err := testPool.QueryRow(context.Background(), "SELECT fixed_ips::text FROM ports WHERE id = $1", port).Scan(&fixed)
	return fixed, err == nil
}

func TestRouterGatewayIsNeverAMembersPort(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	external, subnet, prefix := sharedExternal(t)
	_, body, routerID := rr.createRouter(t, rr.member, "")
	if routerID == "" {
		t.Fatalf("create the router: %s", body)
	}
	chosen := prefix + ".50" // outside the pools: a gateway's address is an administrator's choice
	forged := forgeRouterPort(t, ownerRouterGateway, routerID, external, subnet, chosen)

	code, body := rr.updateGateway(t, rr.member, routerID, `{"network_id":"`+external+`"}`)
	if code != http.StatusOK {
		t.Fatalf("set the gateway: code=%d body=%s", code, body)
	}
	if gw := gatewayOf(t, body); gw == nil || len(gw.ExternalFixedIPs) != 1 || !inPool(prefix, gw.ExternalFixedIPs[0].IPAddress, 10, 20) {
		t.Errorf("gateway %+v; want one address from the pools, not the member's %s", gw, chosen)
	}
	code, body = doJSONReq(rr.r, http.MethodGet, "/v2.0/routers/"+routerID, "", rr.member)
	if gw := gatewayOf(t, body); code != http.StatusOK || gw == nil || len(gw.ExternalFixedIPs) != 1 || gw.ExternalFixedIPs[0].IPAddress == chosen {
		t.Errorf("show: code=%d gateway %+v; want the router's own port", code, gw)
	}
	if fixed, ok := portFixedIPs(t, forged); !ok || !strings.Contains(fixed, chosen) {
		t.Errorf("the member's port after setting the gateway: %q, present %v; want it as it was", fixed, ok)
	}

	if code, body = rr.updateGateway(t, rr.member, routerID, `{}`); code != http.StatusOK {
		t.Fatalf("clear the gateway: code=%d body=%s", code, body)
	}
	if fixed, ok := portFixedIPs(t, forged); !ok || !strings.Contains(fixed, chosen) {
		t.Errorf("the member's port after clearing the gateway: %q, present %v; want it as it was", fixed, ok)
	}
	if ports := gatewayPorts(t, routerID); len(ports) != 1 || ports[0].id != forged {
		t.Errorf("ports claiming the gateway after the clear: %+v; want only the member's", ports)
	}
}

func TestFloatingIPNeverFindsARouterThroughAMembersPort(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	cleanupFloatingIPsOn(t, rr.external)
	inner := fmt.Sprintf("10.95.%d", rand.IntN(250))
	gateway := inner + ".1"
	network := seedNetwork(t, "src2-inner")
	subnet := seedSubnet(t, network, "src2-inner", inner+".0/24", &gateway, 4, false)
	vm := seedVMPort(t, network, subnet, inner+".5")

	wantNotFound := func(t *testing.T, external string) {
		t.Helper()
		code, body, _ := createFloatingIP(rr.r, rr.member,
			`{"floatingip":{"floating_network_id":"`+external+`","port_id":"`+vm+`"}}`)
		if code != http.StatusNotFound || neutronErrorType(t, body) != "ExternalGatewayForFloatingIPNotFound" {
			t.Errorf("create: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", code, body)
		}
	}

	t.Run("a port claiming an interface of a router with a gateway", func(t *testing.T) {
		_, body, routerID := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
		if routerID == "" {
			t.Fatalf("create the router: %s", body)
		}
		// The router has a real interface, on another subnet: the claim, not the router, is refused.
		otherGateway := "10.94.1.1"
		other := seedNetwork(t, "src2-other")
		seedInterfacePort(t, routerID, other, seedSubnet(t, other, "src2-other", "10.94.1.0/24", &otherGateway, 4, false), otherGateway)
		forgeRouterPort(t, ownerRouterInterface, routerID, network, subnet, inner+".2")
		wantNotFound(t, rr.external)
	})

	t.Run("a port claiming the gateway of a router with an interface", func(t *testing.T) {
		external, extSubnet, prefix := sharedExternal(t)
		_, body, routerID := rr.createRouter(t, rr.member, "")
		if routerID == "" {
			t.Fatalf("create the router: %s", body)
		}
		seedInterfacePort(t, routerID, network, subnet, gateway)
		forgeRouterPort(t, ownerRouterGateway, routerID, external, extSubnet, prefix+".50")
		wantNotFound(t, external)
	})
}
