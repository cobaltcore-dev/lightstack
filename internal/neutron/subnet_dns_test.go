package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// SPEC-033 T172: a subnet's dns_nameservers were stored as sent and written
// into the dnsmasq configuration of the network's DHCP, which starts as root on the network node; a
// newline in one started a directive of the subnet owner's choosing (dhcp-leasefile=, conf-file=,
// dhcp-script=). They are now checked as upstream checks them — neutron-lib's validate_nameservers,
// then _validate_subnet's count and IP version — before anything is stored (SPEC-033 FR-060).

func TestCreateSubnetChecksNameserversAsUpstream(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	network := seedNetwork(t, "src4-dns")
	t.Cleanup(func() { mustExec(t, "DELETE FROM subnets WHERE network_id = $1", network) })

	create := func(cidr, ipVersion, servers string) (int, string) {
		return doJSONReq(r, http.MethodPost, "/v2.0/subnets", `{"subnet":{"network_id":"`+network+`","cidr":"`+cidr+
			`","ip_version":`+ipVersion+`,"enable_dhcp":false,"dns_nameservers":`+servers+`}}`, hdr)
	}
	notValid := func(host string) string {
		return "Invalid input for dns_nameservers. Reason: '" + host + "' is not a valid nameserver. '" + host +
			"' is not a valid IP address."
	}
	cases := []struct {
		name, servers, neutronType, message string
	}{
		{"a directive after a newline", `["192.0.2.53\ndhcp-script=/bin/sh"]`, "BadRequest", notValid("192.0.2.53\ndhcp-script=/bin/sh")},
		{"a host name", `["dns.example"]`, "BadRequest", notValid("dns.example")},
		{"three parts", `["10.1.2"]`, "BadRequest", notValid("10.1.2")},
		{"an address with a zone", `["fe80::1%eth0"]`, "BadRequest", notValid("fe80::1%eth0")},
		{"leading zeros", `["010.0.0.1"]`, "BadRequest", "Invalid input for dns_nameservers. Reason: '010.0.0.1' is not a valid " +
			"nameserver. '010.0.0.1' is not an accepted IP address, '10.0.0.1' is recommended."},
		{"one server twice", `["192.0.2.53","192.0.2.53"]`, "BadRequest",
			"Invalid input for dns_nameservers. Reason: Duplicate nameserver '192.0.2.53'."},
		{"six servers", `["192.0.2.1","192.0.2.2","192.0.2.3","192.0.2.4","192.0.2.5","192.0.2.6"]`, "DNSNameServersExhausted",
			"Unable to complete operation for new subnet. The number of DNS nameservers exceeds the limit 5."},
		{"a server of the other IP version", `["2001:db8::53"]`, "InvalidInput",
			"Invalid input for operation: dns_nameserver '2001:db8::53' does not match the ip_version '4'."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := create("10.70.9.0/24", "4", tc.servers)
			if code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s; want 400", code, body)
			}
			if e := neutronError(t, body); e["type"] != tc.neutronType || e["message"] != tc.message {
				t.Errorf("refusal %v: %q; want %s: %q", e["type"], e["message"], tc.neutronType, tc.message)
			}
		})
	}
	var stored int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM subnets WHERE network_id = $1", network).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("%d refused subnet(s) stored", stored)
	}

	for _, accepted := range []struct{ cidr, servers string }{
		{"10.70.1.0/24", `["192.0.2.53","192.0.2.54"]`},
		{"10.70.2.0/24", `["192.0.2.1","192.0.2.2","192.0.2.3","192.0.2.4","192.0.2.5"]`},
	} {
		code, body := create(accepted.cidr, "4", accepted.servers)
		if code != http.StatusCreated {
			t.Fatalf("create with %s: code=%d body=%s; want 201", accepted.servers, code, body)
		}
		var out struct {
			Subnet struct {
				ID             string   `json:"id"`
				DNSNameservers []string `json:"dns_nameservers"`
			} `json:"subnet"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		_, shown := doReq(r, http.MethodGet, "/v2.0/subnets/"+out.Subnet.ID, hdr)
		want := strings.ReplaceAll(strings.Trim(accepted.servers, "[]"), `"`, "")
		if got := strings.Join(out.Subnet.DNSNameservers, ","); got != want || !strings.Contains(shown, accepted.servers) {
			t.Errorf("servers answered %q, shown %s; want %s as given", got, shown, accepted.servers)
		}
	}
}
