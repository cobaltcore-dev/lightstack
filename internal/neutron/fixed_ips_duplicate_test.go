package neutron

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T254 (FR-032): one request never gives an address twice. An address requested twice is refused,
// as upstream's fixed_ips validator ("Duplicate IP address"); two entries naming only a subnet get two
// addresses, as upstream's IPAM, and an entry naming only a subnet never takes an address another entry of
// the request names. Ports and router gateways share the resolution.
func TestOneRequestGivesEachAddressOnce(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	gw := "10.91.0.1"
	network := seedNetwork(t, "spec033-t254-"+uuid.NewString()[:6])
	subnet := seedSubnet(t, network, "spec033-t254", "10.91.0.0/29", &gw, 4, false)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", network) })
	post := func(fixed string) (int, string) {
		return doJSONReq(r, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`","fixed_ips":`+fixed+`}}`, member)
	}

	t.Run("an address requested twice is refused", func(t *testing.T) {
		code, body := post(`[{"ip_address":"10.91.0.5"},{"ip_address":"10.91.0.5"}]`)
		want := "Invalid input for fixed_ips. Reason: Duplicate IP address '10.91.0.5'."
		if code != http.StatusBadRequest || neutronErrorMessage(t, body) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, body, want)
		}
	})

	t.Run("two subnet entries get two addresses, neither one another entry names", func(t *testing.T) {
		storeTestPools(t, subnet, [2]string{"10.91.0.2", "10.91.0.4"})
		code, body := post(`[{"subnet_id":"` + subnet + `"},{"subnet_id":"` + subnet + `"},{"ip_address":"10.91.0.3"}]`)
		if code != http.StatusCreated {
			t.Fatalf("code=%d body=%s; want 201", code, body)
		}
		got := portAddressesOf(t, body)
		slices.Sort(got)
		if want := []string{"10.91.0.2", "10.91.0.3", "10.91.0.4"}; !slices.Equal(got, want) {
			t.Errorf("addresses %v; want %v, each once", got, want)
		}
	})

	t.Run("a router gateway's address requested twice is refused", func(t *testing.T) {
		rr := newRouterRig(t)
		code, body, _ := rr.createRouter(t, rr.admin, `{"network_id":"`+rr.external+`","external_fixed_ips":[`+
			`{"ip_address":"`+rr.prefix+`.15"},{"ip_address":"`+rr.prefix+`.15"}]}`)
		want := "Invalid input for external_gateway_info. Reason: Duplicate IP address '" + rr.prefix + ".15'."
		if code != http.StatusBadRequest || neutronErrorMessage(t, body) != want {
			t.Errorf("code=%d body=%s; want 400 %q", code, body, want)
		}
	})
}

// SPEC-033 T255 (FR-037): a gateway update keeps the router's external address unless it asks for another.
// An empty external_fixed_ips asks for no change, and an entry naming only the subnet the gateway already has
// an address on keeps that address, as upstream's _check_for_external_ip_change and _get_changed_ips_for_port.
// Both moved the address, changing the router's SNAT source and dropping its connections.
func TestGatewayUpdateKeepsTheAddressItIsNotAskedToChange(t *testing.T) {
	requireDB(t)
	rr := newRouterRig(t)
	code, body, id := rr.createRouter(t, rr.admin, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	before := gatewayOf(t, body).ExternalFixedIPs
	for _, tc := range []struct{ name, fixed string }{
		{"an empty list", `[]`},
		{"an entry naming only its subnet", `[{"subnet_id":"` + rr.subnet + `"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := rr.updateGateway(t, rr.admin, id, `{"network_id":"`+rr.external+`","external_fixed_ips":`+tc.fixed+`}`)
			if code != http.StatusOK {
				t.Fatalf("update: code=%d body=%s", code, body)
			}
			if after := gatewayOf(t, body).ExternalFixedIPs; !slices.Equal(after, before) {
				t.Errorf("external_fixed_ips %+v after the update; want %+v kept", after, before)
			}
		})
	}
}

// portAddressesOf reads the addresses of a port response body.
func portAddressesOf(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Port struct {
			FixedIPs []fixedIPOut `json:"fixed_ips"`
		} `json:"port"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var addresses []string
	for _, f := range out.Port.FixedIPs {
		addresses = append(addresses, f.IPAddress)
	}
	return addresses
}
