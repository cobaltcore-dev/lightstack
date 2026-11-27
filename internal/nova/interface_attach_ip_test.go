package nova

import (
	"net/http"
	"strings"
	"testing"
)

// SPEC-033 round 2, T089 (FR-053; research R49): a requested fixed IP on an interface attach that is
// its subnet's network or broadcast address answers 400, as upstream's check_subnet_ip refuses it
// (Neutron's InvalidIpForSubnet). It used to be accepted and stored on the port.
func TestInterfaceAttach_NetworkAndBroadcastAddressesRefused(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	server, _ := serverWithPorts(t, r, rec, tenant, 1)
	network, _ := createTestNetwork(t, tenant) // 10.33.0.0/24

	for _, ip := range []string{"10.33.0.0", "10.33.0.255"} {
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface",
			`{"interfaceAttachment":{"net_id":"`+network+`","fixed_ip":"`+ip+`"}}`, hdr)
		if code != http.StatusBadRequest || !strings.Contains(body, "is not a valid IP for the specified subnet") {
			t.Errorf("attach with fixed_ip %s: code=%d body=%s; want 400 InvalidIpForSubnet", ip, code, body)
		}
	}
	var ports int
	if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM ports WHERE network_id = $1", network).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	if ports != 0 {
		t.Errorf("%d port(s) on the network after the refusals; want none", ports)
	}

	// A host address inside the subnet is still accepted.
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface",
		`{"interfaceAttachment":{"net_id":"`+network+`","fixed_ip":"10.33.0.77"}}`, hdr); code != http.StatusOK {
		t.Errorf("attach with fixed_ip 10.33.0.77: code=%d body=%s; want 200", code, body)
	}
}
