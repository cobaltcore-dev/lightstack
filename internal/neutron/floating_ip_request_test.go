package neutron

import (
	"net/http"
	"testing"
)

// SPEC-033 T271 (FR-071): a floating IP's request is refused where upstream's is, rather than half applied.
// A fixed_ip_address without a port_id is upstream's 400 (_check_and_get_fip_assoc), on create and on update;
// an update's fixed_ip_address or description that is not a string is its attribute validator's 400. Each was
// ignored, and the request answered success having done nothing it asked.
func TestFloatingIPRequestIsRefusedWhereUpstreamRefusesIt(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	fip, _ := rig.floatingIP(t, "")
	withoutPort := "Bad floatingip request: fixed_ip_address cannot be specified without a port_id."
	for _, tc := range []struct{ name, method, path, body, message string }{
		{"create: fixed_ip_address without port_id", http.MethodPost, "/v2.0/floatingips",
			`{"floatingip":{"floating_network_id":"` + rig.external + `","fixed_ip_address":"10.77.0.5"}}`, withoutPort},
		{"update: fixed_ip_address without port_id", http.MethodPut, "/v2.0/floatingips/" + fip,
			`{"floatingip":{"fixed_ip_address":"10.77.0.5"}}`, withoutPort},
		{"update: fixed_ip_address not a string", http.MethodPut, "/v2.0/floatingips/" + fip,
			`{"floatingip":{"port_id":null,"fixed_ip_address":5}}`, "Invalid input for fixed_ip_address. Reason: '5' is not a valid IP address."},
		{"update: description not a string", http.MethodPut, "/v2.0/floatingips/" + fip,
			`{"floatingip":{"description":5}}`, "Invalid input for description. Reason: '5' is not a valid string."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := rig.do(t, tc.method, tc.path, tc.body); code != http.StatusBadRequest || neutronErrorMessage(t, body) != tc.message {
				t.Errorf("code=%d body=%s; want 400 %q", code, body, tc.message)
			}
		})
	}
}

// SPEC-033 T272 (FR-030): an allocation pool's addresses carry no IPv6 zone, as upstream's validation and
// subnet_dns.go's own address check refuse one; a zoned pool was stored, rendered and handed out.
func TestAllocationPoolRefusesAZone(t *testing.T) {
	requireDB(t)
	p := newPoolRig()
	network := seedNetwork(t, "spec033-t272")
	code, body := doJSONReq(p.r, http.MethodPost, "/v2.0/subnets", `{"subnet":{"network_id":"`+network+`","cidr":"2001:db8:91::/64",`+
		`"ip_version":6,"allocation_pools":[{"start":"2001:db8:91::10%eth0","end":"2001:db8:91::20%eth0"}]}}`, p.hdr)
	if code != http.StatusBadRequest || neutronErrorType(t, body) != "InvalidAllocationPool" {
		t.Errorf("code=%d body=%s; want 400 InvalidAllocationPool", code, body)
	}
}
