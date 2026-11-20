package neutron

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"
)

// Branch review SRC-11, T183 (FR-038, FR-054): upstream's policies give two attributes to fewer callers
// than their action. A floating IP's floating_ip_address is an administrator's or the project manager's
// to choose (create_floatingip:floating_ip_address); a router gateway's enable_snat is an
// administrator's, on create and on update (create_router and update_router:
// external_gateway_info:enable_snat). Anyone else giving one is refused 403 PolicyNotAuthorized,
// whatever its value, and nothing is stored. A floating_ip_address of null is the attribute's default,
// not a choice: upstream's rule matches only an attribute set to something else.

// rolesHeader is a request of the seeded project carrying the given comma-separated roles.
func rolesHeader(roles string) map[string]string {
	return map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": roles}
}

func TestFloatingIPAddressIsAnAdministratorsOrManagers(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
	network, _ := fipNetwork(t, prefix+".0/25", prefix+".10", prefix+".20")
	cleanupFloatingIPsOn(t, network)
	chosen := func(address string) string {
		return `{"floatingip":{"floating_network_id":"` + network + `","floating_ip_address":"` + address + `"}}`
	}

	code, body, _ := createFloatingIP(r, rolesHeader("member"), chosen(prefix+".30"))
	if code != http.StatusForbidden || neutronErrorType(t, body) != "PolicyNotAuthorized" {
		t.Errorf("a member's floating_ip_address: code=%d body=%s; want 403 PolicyNotAuthorized", code, body)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM floating_ips WHERE floating_network_id = $1", network).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d floating IP row(s) after the refusal (%v); want none", n, err)
	}

	code, body, address := createFloatingIP(r, rolesHeader("member"),
		`{"floatingip":{"floating_network_id":"`+network+`","floating_ip_address":null}}`)
	if code != http.StatusCreated || address != prefix+".10" {
		t.Errorf("a member's null floating_ip_address: code=%d address=%s body=%s; want 201 %s.10, from the pools", code, address, body, prefix)
	}

	for i, roles := range []string{"member,manager", "admin"} {
		want := fmt.Sprintf("%s.%d", prefix, 30+i)
		if code, body, address := createFloatingIP(r, rolesHeader(roles), chosen(want)); code != http.StatusCreated || address != want {
			t.Errorf("roles %s: code=%d address=%s body=%s; want 201 %s", roles, code, address, body, want)
		}
	}
}

func TestGatewayEnableSNATIsAnAdministrators(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	gateway := func(snat string) string { return `{"network_id":"` + rr.external + `","enable_snat":` + snat + `}` }
	snatOf := func(body string) *bool {
		if gw := gatewayOf(t, body); gw != nil {
			return gw.EnableSNAT
		}
		return nil
	}

	for _, snat := range []string{"false", "true"} {
		code, body, _ := rr.createRouter(t, rr.member, gateway(snat))
		if code != http.StatusForbidden || neutronErrorType(t, body) != "PolicyNotAuthorized" {
			t.Errorf("a member's create with enable_snat %s: code=%d body=%s; want 403 PolicyNotAuthorized", snat, code, body)
		}
	}
	var ports int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM ports WHERE network_id = $1 AND device_owner = 'network:router_gateway'", rr.external).Scan(&ports); err != nil || ports != 0 {
		t.Errorf("%d gateway port(s) after the refusals (%v); want none", ports, err)
	}

	code, body, id := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("a member's create without enable_snat: code=%d body=%s; want 201", code, body)
	}
	for _, snat := range []string{"false", "true"} {
		code, body := rr.updateGateway(t, rr.member, id, gateway(snat))
		if code != http.StatusForbidden || neutronErrorType(t, body) != "PolicyNotAuthorized" {
			t.Errorf("a member's update with enable_snat %s: code=%d body=%s; want 403 PolicyNotAuthorized", snat, code, body)
		}
	}
	code, body = doReq(rr.r, http.MethodGet, "/v2.0/routers/"+id, rr.member)
	if snat := snatOf(body); code != http.StatusOK || snat == nil || !*snat {
		t.Errorf("the member's router after the refusals: code=%d body=%s; want its gateway with SNAT on", code, body)
	}

	code, body, _ = rr.createRouter(t, rr.admin, gateway("false"))
	if snat := snatOf(body); code != http.StatusCreated || snat == nil || *snat {
		t.Errorf("an administrator's create with SNAT off: code=%d body=%s; want 201, SNAT off", code, body)
	}
	code, body = rr.updateGateway(t, rr.admin, id, gateway("false"))
	if snat := snatOf(body); code != http.StatusOK || snat == nil || *snat {
		t.Errorf("an administrator's update with SNAT off: code=%d body=%s; want 200, SNAT off", code, body)
	}
}
