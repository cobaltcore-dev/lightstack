package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// DeleteNetwork refused a network that still had subnets ("network has subnets; delete subnets
// first"). Upstream deletes them with it: db_base_plugin_v2.delete_network removes every subnet of
// the network before the network itself, and refuses only while ports remain. So a delete a Neutron
// client expects to succeed answered 409, and every client had to empty the network first.

// seedDHCPSubnet adds a gatewayless DHCP-enabled subnet to a network and returns its id. It writes the
// row alone, as a subnet stored before the network's DHCP port existed.
func seedDHCPSubnet(t *testing.T, networkID, name, cidr string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULL, 4, true, now(), now())
	`, id, name, networkID, seededProjectID, cidr); err != nil {
		t.Fatalf("seed subnet %s: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = $1", id) //nolint:errcheck // best-effort
	})
	return id
}

// holdDHCP gives svc a fake DHCP holding the network's, as the network node does once the network has
// DHCP, and returns it.
func holdDHCP(t *testing.T, svc *Service, networkID string) *fakeDHCP {
	t.Helper()
	dhcp := newFakeDHCP()
	if err := dhcp.ConvergeDHCP(context.Background(), networkID, &networking.DHCPState{NetworkID: networkID}); err != nil {
		t.Fatal(err)
	}
	svc.dhcp = dhcp
	return dhcp
}

// TestDeleteNetworkTakesItsSubnets — the subnets go with the network, and so does what the host ran
// for them: the network's DHCP, which leaves the network node before the record goes (SPEC-033
// FR-040), and the bridge.
func TestDeleteNetworkTakesItsSubnets(t *testing.T) {
	requireDB(t)
	r, svc := newNeutronTestRouterWithService()

	id := seedNetwork(t, "spec024-del-subnets-"+uuid.NewString()[:8])
	subnets := []string{
		seedDHCPSubnet(t, id, "spec024-del-subnets-dhcp", "10.96.0.0/24"),
		seedSubnet(t, id, "spec024-del-subnets-plain", "10.96.1.0/24", nil, 4, false),
	}
	dhcp := holdDHCP(t, svc, id)
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
	if _, held := dhcp.state(id); held {
		t.Errorf("DHCP still held for deleted network %s; a dnsmasq with no network to serve is a leak", id[:8])
	}
	if hasStubBridge(t, svc, bridge) {
		t.Errorf("bridge %s outlived its network", bridge)
	}
}
