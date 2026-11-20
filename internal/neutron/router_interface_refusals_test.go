package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Branch review SRC-3 (2026-11-23-review.md): adding a router interface took any subnet the caller can
// read — a shared network's, an external one's — and gave the router's port the subnet's gateway
// address, whoever held it. On the network node an interface is a leg with that address on the
// network's bridge, so a member's router answered for another project's gateway, or became it.
// Upstream's _add_interface_by_subnet refuses a subnet of another project unless its network is shared
// with the caller's project alone (a globally shared network is refused, LP-1757482), and a subnet the
// router already has a port on or one overlapping its interfaces' (_check_for_dup_router_subnets); and
// the interface's port cannot take an address another port holds (SPEC-033 FR-055).

// seedProjectSubnet seeds a subnet of project on network, removed again by cleanup.
func seedProjectSubnet(t *testing.T, project, network, cidr, gateway string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, 'src3-subnet', $2, $3, $4, $5, 4, false, now(), now())`, id, network, project, cidr, gateway)
	t.Cleanup(func() { mustExec(t, "DELETE FROM subnets WHERE id = $1", id) })
	return id
}

func (rr *routerRig) addInterface(hdr map[string]string, routerID, subnetID string) (int, string) {
	return doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface", `{"subnet_id":"`+subnetID+`"}`, hdr)
}

// interfacesOn counts the router's interfaces on the subnet, and the ports claiming one there.
func interfacesOn(t *testing.T, routerID, subnetID string) (interfaces, ports int) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM router_interfaces WHERE router_id = $1 AND subnet_id = $2",
		routerID, subnetID).Scan(&interfaces); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM ports WHERE device_id = $1 AND device_owner = 'network:router_interface' AND fixed_ips::text LIKE '%' || $2 || '%'",
		routerID, subnetID).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	return interfaces, ports
}

func TestRouterInterfaceRefusals(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)

	// refused makes an add that must answer code with upstream's type and message, and store nothing.
	refused := func(t *testing.T, hdr map[string]string, routerID, subnetID string, code int, neutronType, message string) {
		t.Helper()
		before, beforePorts := interfacesOn(t, routerID, subnetID)
		got, body := rr.addInterface(hdr, routerID, subnetID)
		if got != code {
			t.Fatalf("add: code=%d body=%s; want %d", got, body, code)
		}
		if e := neutronError(t, body); e["type"] != neutronType || e["message"] != message {
			t.Errorf("refusal %v: %v; want %s: %s", e["type"], e["message"], neutronType, message)
		}
		if after, afterPorts := interfacesOn(t, routerID, subnetID); after != before || afterPorts != beforePorts {
			t.Errorf("after the refusal: %d interfaces, %d ports on the subnet; want %d and %d", after, afterPorts, before, beforePorts)
		}
	}
	router := func(t *testing.T) string {
		t.Helper()
		_, body, id := rr.createRouter(t, rr.member, "")
		if id == "" {
			t.Fatalf("create the router: %s", body)
		}
		return id
	}

	t.Run("a subnet of another project's shared network", func(t *testing.T) {
		owner := createTestProject(t, "src3-shared-owner")
		shared := seedExternalNetwork(t, "src3-shared-"+uuid.NewString()[:6], owner, true, false)
		subnet := seedProjectSubnet(t, owner, shared, "10.93.0.0/24", "10.93.0.1")
		routerID := router(t)
		refused(t, rr.member, routerID, subnet, http.StatusBadRequest, "BadRequest",
			"Bad router request: Cannot add interface to router because subnet "+subnet+" is not owned by project making the request.")
		if code, body := rr.addInterface(rr.admin, routerID, subnet); code != http.StatusOK {
			t.Errorf("an administrator's add: code=%d body=%s; want 200, as upstream's", code, body)
		}
	})

	t.Run("a subnet whose gateway another router holds", func(t *testing.T) {
		gateway := "10.92.0.1"
		network := seedNetwork(t, "src3-held")
		subnet := seedSubnet(t, network, "src3-held", "10.92.0.0/24", &gateway, 4, false)
		if code, body := rr.addInterface(rr.member, router(t), subnet); code != http.StatusOK {
			t.Fatalf("the first router's add: code=%d body=%s", code, body)
		}
		refused(t, rr.member, router(t), subnet, http.StatusConflict, "IpAddressAlreadyAllocated",
			"IP address "+gateway+" already allocated in subnet "+subnet)
	})

	t.Run("the subnet of the router's gateway", func(t *testing.T) {
		_, body, routerID := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
		if routerID == "" {
			t.Fatalf("create the router: %s", body)
		}
		refused(t, rr.member, routerID, rr.subnet, http.StatusBadRequest, "BadRequest",
			"Bad router request: Router already has a port on subnet "+rr.subnet+".")
	})

	t.Run("a subnet the router has an interface on", func(t *testing.T) {
		routerID := router(t)
		gateway := "10.89.0.1"
		network := seedNetwork(t, "src3-twice")
		subnet := seedSubnet(t, network, "src3-twice", "10.89.0.0/24", &gateway, 4, false)
		if code, body := rr.addInterface(rr.member, routerID, subnet); code != http.StatusOK {
			t.Fatalf("the first add: code=%d body=%s", code, body)
		}
		refused(t, rr.member, routerID, subnet, http.StatusBadRequest, "BadRequest",
			"Bad router request: Router already has a port on subnet "+subnet+".")
	})

	t.Run("a subnet overlapping one of the router's", func(t *testing.T) {
		routerID := router(t)
		narrowGateway, wideGateway := "10.91.7.1", "10.91.0.1"
		narrowNet, wideNet := seedNetwork(t, "src3-narrow"), seedNetwork(t, "src3-wide")
		narrow := seedSubnet(t, narrowNet, "src3-narrow", "10.91.7.0/24", &narrowGateway, 4, false)
		wide := seedSubnet(t, wideNet, "src3-wide", "10.91.0.0/16", &wideGateway, 4, false)
		if code, body := rr.addInterface(rr.member, routerID, narrow); code != http.StatusOK {
			t.Fatalf("add the /24: code=%d body=%s", code, body)
		}
		refused(t, rr.member, routerID, wide, http.StatusBadRequest, "BadRequest",
			"Bad router request: Cidr 10.91.0.0/16 of subnet "+wide+" overlaps with cidr 10.91.7.0/24 of subnet "+narrow+".")
	})

	t.Run("a member's port claiming an interface does not stand for one", func(t *testing.T) {
		routerID := router(t)
		gateway := "10.90.0.1"
		network := seedNetwork(t, "src3-claimed")
		subnet := seedSubnet(t, network, "src3-claimed", "10.90.0.0/24", &gateway, 4, false)
		forgeRouterPort(t, ownerRouterInterface, routerID, network, subnet, "10.90.0.7")
		if code, body := rr.addInterface(rr.member, routerID, subnet); code != http.StatusOK {
			t.Errorf("add: code=%d body=%s; want 200: the router has no interface there", code, body)
		}
	})
}
