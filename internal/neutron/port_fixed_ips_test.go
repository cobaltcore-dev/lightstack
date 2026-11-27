package neutron

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/google/uuid"
)

// SPEC-033 FR-032 and FR-053, found in Phase 20: a port create's requested fixed_ips are honoured as
// upstream's. An address need only lie inside one of the network's subnets, not be its network or
// broadcast address, and be free; an entry naming only a subnet takes an address of its pools; an
// empty list gives the port none. The create ignored fixed_ips altogether and drew an address at
// random, so a port forwarding to the address asked for was refused.
func TestPortCreateRequestedFixedIPs(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	network := seedNetwork(t, "spec033-fixed-"+uuid.NewString()[:6])
	gateway := "10.99.0.1"
	subnet := seedSubnet(t, network, "spec033-fixed", "10.99.0.0/24", &gateway, 4, false)
	storeTestPools(t, subnet, [2]string{"10.99.0.100", "10.99.0.110"})

	create := func(t *testing.T, fixed string) (int, common.FixedIPs, string) {
		t.Helper()
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+network+`","fixed_ips":`+fixed+`}}`, hdr)
		var out struct {
			Port struct {
				FixedIPs common.FixedIPs `json:"fixed_ips"`
			} `json:"port"`
		}
		if code == http.StatusCreated {
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
		}
		return code, out.Port.FixedIPs, body
	}
	entry := func(subnetID, ip string) string {
		if ip == "" {
			return `[{"subnet_id":"` + subnetID + `"}]`
		}
		return `[{"subnet_id":"` + subnetID + `","ip_address":"` + ip + `"}]`
	}

	t.Run("an address inside the subnet, outside its pools", func(t *testing.T) {
		code, fixed, body := create(t, entry(subnet, "10.99.0.10"))
		if code != http.StatusCreated || len(fixed) != 1 || fixed[0].IPAddress != "10.99.0.10" || fixed[0].SubnetID != subnet {
			t.Fatalf("code=%d fixed=%v body=%s; want 201 with 10.99.0.10", code, fixed, body)
		}
	})

	t.Run("an address by itself", func(t *testing.T) {
		code, fixed, body := create(t, `[{"ip_address":"10.99.0.11"}]`)
		if code != http.StatusCreated || len(fixed) != 1 || fixed[0].IPAddress != "10.99.0.11" || fixed[0].SubnetID != subnet {
			t.Fatalf("code=%d fixed=%v body=%s; want 201 with 10.99.0.11 on the subnet holding it", code, fixed, body)
		}
	})

	t.Run("a subnet alone takes an address of its pools", func(t *testing.T) {
		code, fixed, body := create(t, entry(subnet, ""))
		if code != http.StatusCreated || len(fixed) != 1 || !inRange(fixed[0].IPAddress, "10.99.0.100", "10.99.0.110") {
			t.Fatalf("code=%d fixed=%v body=%s; want 201 with an address of the pool", code, fixed, body)
		}
	})

	t.Run("none", func(t *testing.T) {
		code, fixed, body := create(t, `[]`)
		if code != http.StatusCreated || len(fixed) != 0 {
			t.Fatalf("code=%d fixed=%v body=%s; want 201 with no address", code, fixed, body)
		}
	})

	for _, tc := range []struct {
		name, fixed string
		code        int
		kind        string
	}{
		{"an address held already", entry(subnet, "10.99.0.10"), http.StatusConflict, "IpAddressAlreadyAllocated"},
		{"the broadcast address", entry(subnet, "10.99.0.255"), http.StatusBadRequest, "InvalidIpForSubnet"},
		{"the network address", entry(subnet, "10.99.0.0"), http.StatusBadRequest, "InvalidIpForSubnet"},
		{"an address on none of the network's subnets", `[{"ip_address":"10.98.0.5"}]`, http.StatusBadRequest, "InvalidIpForNetwork"},
		{"a subnet of another network", entry(uuid.NewString(), ""), http.StatusBadRequest, "InvalidInput"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := create(t, tc.fixed)
			if code != tc.code || neutronErrorType(t, body) != tc.kind {
				t.Fatalf("code=%d body=%s; want %d %s", code, body, tc.code, tc.kind)
			}
		})
	}
}

// inRange reports whether ip lies in [first, last], all IPv4 in one /24.
func inRange(ip, first, last string) bool {
	a, b, c := parseGateway(&ip), parseGateway(&first), parseGateway(&last)
	return a.IsValid() && a.Compare(b) >= 0 && a.Compare(c) <= 0
}
