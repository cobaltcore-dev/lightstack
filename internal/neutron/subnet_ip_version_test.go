package neutron

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// SPEC-033 T247 (FR-068): a subnet's IP version is its CIDR's. A given ip_version must be 4 or 6 and match
// the CIDR, as upstream's _validate_ip_version; an absent one is the CIDR's. A subnet stored as IPv4 with an
// IPv6 CIDR became the network's IPv4 DHCP subnet, and rendering its dnsmasq configuration panicked the
// network node's reconciliation pass.
func TestSubnetIPVersionMatchesTheCIDR(t *testing.T) {
	requireDB(t)
	p := newPoolRig()
	create := func(t *testing.T, cidr, version string) (int, string, string) {
		t.Helper()
		network := seedNetwork(t, "spec033-ipversion")
		body := fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":%q%s}}`, network, cidr, version)
		code, out := doJSONReq(p.r, http.MethodPost, "/v2.0/subnets", body, p.hdr)
		return code, out, network
	}
	stored := func(t *testing.T, network string) int {
		t.Helper()
		var n int
		if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM subnets WHERE network_id = $1", network).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, tc := range []struct {
		name, cidr, version, message string
	}{
		{"an IPv6 CIDR as version 4", "2001:db8:93::/64", `,"ip_version":4`,
			"Invalid input for operation: cidr '2001:db8:93::/64' does not match the ip_version '4'."},
		{"an IPv4 CIDR as version 6", "10.93.1.0/24", `,"ip_version":6`,
			"Invalid input for operation: cidr '10.93.1.0/24' does not match the ip_version '6'."},
		{"version 5", "10.93.2.0/24", `,"ip_version":5`,
			"Invalid input for ip_version. Reason: '5' is not in [4, 6]."},
	} {
		t.Run(tc.name+" is refused and stores nothing", func(t *testing.T) {
			code, body, network := create(t, tc.cidr, tc.version)
			var out struct {
				NeutronError struct {
					Message string `json:"message"`
				} `json:"NeutronError"`
			}
			_ = json.Unmarshal([]byte(body), &out)
			if code != http.StatusBadRequest || out.NeutronError.Message != tc.message {
				t.Errorf("code=%d body=%s; want 400 %q", code, body, tc.message)
			}
			if n := stored(t, network); n != 0 {
				t.Errorf("%d subnets stored; want none", n)
			}
		})
	}

	t.Run("an absent version is the CIDR's", func(t *testing.T) {
		code, body, _ := create(t, "2001:db8:94::/64", "")
		var out struct {
			Subnet struct {
				IPVersion int `json:"ip_version"`
			} `json:"subnet"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusCreated || out.Subnet.IPVersion != 6 {
			t.Errorf("code=%d body=%s; want 201 with ip_version 6", code, body)
		}
	})
}
