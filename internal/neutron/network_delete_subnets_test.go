package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// DeleteNetwork refused a network that still had subnets ("network has subnets; delete subnets
// first"). Upstream deletes them with it: db_base_plugin_v2.delete_network removes every subnet of
// the network before the network itself, and refuses only while ports remain. So a delete a Neutron
// client expects to succeed answered 409, and every client had to empty the network first.

// TestDeleteNetworkTakesItsSubnets — the subnets go with the network, and so does what the host ran
// for them: the network's DHCP server, which nothing else would stop now that no subnet delete does,
// and the bridge.
func TestDeleteNetworkTakesItsSubnets(t *testing.T) {
	requireDB(t)
	r, svc := newNeutronTestRouterWithService()

	id := seedNetwork(t, "spec024-del-subnets-"+uuid.NewString()[:8])
	subnets := []string{
		seedDHCPSubnet(t, id, "spec024-del-subnets-dhcp", "10.96.0.0/24"),
		seedSubnet(t, id, "spec024-del-subnets-plain", "10.96.1.0/24", nil, 4, false),
	}
	markDHCPServing(t, svc, id)
	bridge := plantStubBridge(t, svc, id)

	code, body := doReq(r, http.MethodDelete, "/v2.0/networks/"+id, neutronAuthHdr(seededProjectID, false))
	if code != http.StatusNoContent {
		t.Fatalf("DELETE a network with subnets = %d, want 204 (body %s)", code, body)
	}

	if networkRowExists(t, id) {
		t.Errorf("network %s survived its delete", id)
	}
	for _, subnetID := range subnets {
		var exists bool
		if err := testPool.QueryRow(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM subnets WHERE id = $1)", subnetID).Scan(&exists); err != nil {
			t.Fatalf("read subnet %s: %v", subnetID, err)
		}
		if exists {
			t.Errorf("subnet %s outlived its network", subnetID)
		}
	}
	if svc.dhcpManager.IsRunning(id) {
		t.Errorf("DHCP still serving deleted network %s; a dnsmasq with no network to serve is a leak", id[:8])
	}
	if hasStubBridge(t, svc, bridge) {
		t.Errorf("bridge %s outlived its network", bridge)
	}
}
