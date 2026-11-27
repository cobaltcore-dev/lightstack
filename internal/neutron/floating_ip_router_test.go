package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T098 (FR-028, FR-029, FR-054; research R36): a floating IP reaches a fixed IP only
// through a router that has its gateway on the floating IP's network and an interface on the fixed IP's
// subnet — the one whose interface holds the subnet's gateway, else the first, as upstream's
// get_router_for_floatingip — 404 ExternalGatewayForFloatingIPNotFound otherwise, on create and on
// update. A fixed IP has at most one floating IP per external network (409
// FloatingIPPortAlreadyAssociated). A requested fixed_ip_address must be the port's (400).

// seedInterfacePort gives a router an interface port on a subnet at ip, as AddRouterInterface writes it.
func seedInterfacePort(t *testing.T, routerID, networkID, subnetID, ip string) {
	t.Helper()
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-rif', $2, $3, $4, 'network:router_interface', 'fa:16:3e:98:00:01', 'ACTIVE', $5::jsonb)`,
		port, networkID, seededProjectID, routerID, `[{"subnet_id":"`+subnetID+`","ip_address":"`+ip+`"}]`)
	mustExec(t, `INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`, routerID, port, subnetID)
}

// seedVMPort seeds a port on a subnet holding ip.
func seedVMPort(t *testing.T, networkID, subnetID, ip string) string {
	t.Helper()
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-vm', $2, $3, 'fa:16:3e:98:00:02', 'ACTIVE', $4::jsonb)`,
		port, networkID, seededProjectID, `[{"subnet_id":"`+subnetID+`","ip_address":"`+ip+`"}]`)
	return port
}

func floatingIPRouter(t *testing.T, id string) (string, bool) {
	t.Helper()
	var router *string
	err := testPool.QueryRow(context.Background(), "SELECT router_id::text FROM floating_ips WHERE id = $1", id).Scan(&router)
	if err != nil {
		return "", false
	}
	if router == nil {
		return "", true
	}
	return *router, true
}

func fipID(body string) string {
	var out struct {
		FloatingIP struct {
			ID string `json:"id"`
		} `json:"floatingip"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.FloatingIP.ID
}

func TestFloatingIPNeedsAJoiningRouter(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	cleanupFloatingIPsOn(t, rr.external)
	hdr := rr.member
	inner := fmt.Sprintf("10.98.%d", rand.IntN(250))

	// An internal subnet no router joins.
	gw := inner + ".1"
	lonely := seedNetwork(t, "spec033-fip-lonely")
	lonelySubnet := seedSubnet(t, lonely, "spec033-fip-lonely", inner+".0/24", &gw, 4, false)
	lonelyPort := seedVMPort(t, lonely, lonelySubnet, inner+".5")

	t.Run("no router joins the subnet to the floating network", func(t *testing.T) {
		code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+lonelyPort+`"}}`)
		if code != http.StatusNotFound || neutronErrorType(t, body) != "ExternalGatewayForFloatingIPNotFound" {
			t.Fatalf("create: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM floating_ips WHERE port_id = $1", lonelyPort).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d floating IPs on the port (%v); want none stored", n, err)
		}

		code, body, _ = createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`"}}`)
		if code != http.StatusCreated {
			t.Fatalf("create unassociated: code=%d body=%s", code, body)
		}
		id := fipID(body)
		code, body = doJSONReq(rr.r, http.MethodPut, "/v2.0/floatingips/"+id, `{"floatingip":{"port_id":"`+lonelyPort+`"}}`, hdr)
		if code != http.StatusNotFound || neutronErrorType(t, body) != "ExternalGatewayForFloatingIPNotFound" {
			t.Errorf("associate: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", code, body)
		}
		var port *string
		if err := testPool.QueryRow(context.Background(), "SELECT port_id::text FROM floating_ips WHERE id = $1", id).Scan(&port); err != nil || port != nil {
			t.Errorf("port_id after the refusal = %v (%v); want still unassociated", port, err)
		}
	})

	t.Run("a router joining the subnet to another external network", func(t *testing.T) {
		other := newRouterRig(t)
		_, _, router := other.createRouter(t, other.member, `{"network_id":"`+other.external+`"}`)
		gw := "10.97.0.1"
		net := seedNetwork(t, "spec033-fip-other")
		sub := seedSubnet(t, net, "spec033-fip-other", "10.97.0.0/24", &gw, 4, false)
		seedInterfacePort(t, router, net, sub, gw)
		port := seedVMPort(t, net, sub, "10.97.0.5")
		code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+port+`"}}`)
		if code != http.StatusNotFound || neutronErrorType(t, body) != "ExternalGatewayForFloatingIPNotFound" {
			t.Errorf("create: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", code, body)
		}
	})

	t.Run("of two joining routers, the one holding the subnet's gateway", func(t *testing.T) {
		gw := "10.94.0.1"
		net := seedNetwork(t, "spec033-fip-two")
		sub := seedSubnet(t, net, "spec033-fip-two", "10.94.0.0/24", &gw, 4, false)
		// The first router reaches the subnet on another address; the second holds its gateway.
		_, _, first := rr.createRouter(t, hdr, `{"network_id":"`+rr.external+`"}`)
		seedInterfacePort(t, first, net, sub, "10.94.0.254")
		_, _, second := rr.createRouter(t, hdr, `{"network_id":"`+rr.external+`"}`)
		seedInterfacePort(t, second, net, sub, gw)
		port := seedVMPort(t, net, sub, "10.94.0.5")
		code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+port+`"}}`)
		if code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", code, body)
		}
		if router, _ := floatingIPRouter(t, fipID(body)); router != second {
			t.Errorf("router_id = %s; want %s, whose interface holds the subnet's gateway (not %s)", router, second, first)
		}
	})

	t.Run("one floating IP per fixed IP per network", func(t *testing.T) {
		gw := "10.93.7.1"
		net := seedNetwork(t, "spec033-fip-029")
		sub := seedSubnet(t, net, "spec033-fip-029", "10.93.7.0/24", &gw, 4, false)
		_, _, router := rr.createRouter(t, hdr, `{"network_id":"`+rr.external+`"}`)
		seedInterfacePort(t, router, net, sub, gw)
		port := seedVMPort(t, net, sub, "10.93.7.5")
		code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+port+`"}}`)
		if code != http.StatusCreated {
			t.Fatalf("first: code=%d body=%s", code, body)
		}
		code, body, _ = createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+port+`"}}`)
		if code != http.StatusConflict || neutronErrorType(t, body) != "FloatingIPPortAlreadyAssociated" {
			t.Errorf("a second create on the fixed IP: code=%d body=%s; want 409 FloatingIPPortAlreadyAssociated", code, body)
		}
		code, body, _ = createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`"}}`)
		if code != http.StatusCreated {
			t.Fatalf("unassociated: code=%d body=%s", code, body)
		}
		code, body = doJSONReq(rr.r, http.MethodPut, "/v2.0/floatingips/"+fipID(body), `{"floatingip":{"port_id":"`+port+`"}}`, hdr)
		if code != http.StatusConflict || neutronErrorType(t, body) != "FloatingIPPortAlreadyAssociated" {
			t.Errorf("an update onto the fixed IP: code=%d body=%s; want 409 FloatingIPPortAlreadyAssociated", code, body)
		}

		// A floating IP of another external network may reach the same fixed IP.
		other := newRouterRig(t)
		cleanupFloatingIPsOn(t, other.external)
		_, _, otherRouter := other.createRouter(t, other.member, `{"network_id":"`+other.external+`"}`)
		seedInterfacePort(t, otherRouter, net, sub, "10.93.7.253")
		if code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+other.external+`","port_id":"`+port+`"}}`); code != http.StatusCreated {
			t.Errorf("a floating IP of another network on the fixed IP: code=%d body=%s; want 201", code, body)
		}
	})

	t.Run("a requested fixed_ip_address must be the port's", func(t *testing.T) {
		gw := "10.93.8.1"
		net := seedNetwork(t, "spec033-fip-054")
		sub := seedSubnet(t, net, "spec033-fip-054", "10.93.8.0/24", &gw, 4, false)
		_, _, router := rr.createRouter(t, hdr, `{"network_id":"`+rr.external+`"}`)
		seedInterfacePort(t, router, net, sub, gw)
		port := seedVMPort(t, net, sub, "10.93.8.5")
		code, body, _ := createFloatingIP(rr.r, hdr, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+port+`","fixed_ip_address":"10.93.8.6"}}`)
		if code != http.StatusBadRequest {
			t.Errorf("create with another fixed IP: code=%d body=%s; want 400", code, body)
		}
	})
}
