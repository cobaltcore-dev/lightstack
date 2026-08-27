package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/google/uuid"
)

// Deleting ONE subnet used to stop DHCP for the WHOLE network.
//
// DeleteSubnet called StopDHCP(networkID) unconditionally, before the row was even removed. dnsmasq
// is per-network, not per-subnet, so a network carrying two DHCP-enabled subnets lost DHCP for the
// surviving one too — every guest on it stopped renewing until the reconciler's next pass, up to 30
// seconds later. The metadata redirect was already handled correctly (removed only when the last
// DHCP subnet went); DHCP itself was not.

// seedDHCPSubnet adds a DHCP-enabled subnet to a network and returns its id.
func seedDHCPSubnet(t *testing.T, networkID, name, cidr string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULL, 4, true, now(), now())
	`, id, name, networkID, seededProjectID, cidr); err != nil {
		t.Fatalf("seed subnet %s: %v", name, err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM subnets WHERE id = $1", id) //nolint:errcheck // best-effort
	})
	return id
}

// markDHCPServing puts the network into the DHCP manager's "serving" state, the way a real subnet
// create would. In stub mode this is bookkeeping only — no dnsmasq is spawned.
func markDHCPServing(t *testing.T, svc *Service, networkID string) {
	t.Helper()
	if err := svc.dhcpManager.EnsureDHCP(networking.DHCPConfig{
		NetworkID: networkID, BridgeName: "br-" + networkID[:8], SubnetCIDR: "10.60.0.0/24",
		DHCPRangeStart: "10.60.0.10", DHCPRangeEnd: "10.60.0.250", LeaseTime: "24h",
	}); err != nil {
		t.Fatalf("mark DHCP serving: %v", err)
	}
	if !svc.dhcpManager.IsRunning(networkID) {
		t.Fatalf("precondition failed: DHCP not serving %s after EnsureDHCP", networkID)
	}
}

func TestDeleteSubnetKeepsDHCPForRemainingSubnets(t *testing.T) {
	requireDB(t)
	r, svc := newNeutronTestRouterWithService()

	netID := seedExternalNetwork(t, "dhcp-keep-"+uuid.NewString()[:8], seededProjectID, false, false)
	keep := seedDHCPSubnet(t, netID, "dhcp-keep-a", "10.60.0.0/24")
	drop := seedDHCPSubnet(t, netID, "dhcp-keep-b", "10.61.0.0/24")
	markDHCPServing(t, svc, netID)

	code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+drop, neutronAuthHdr(seededProjectID, false))
	if code != http.StatusNoContent {
		t.Fatalf("delete subnet: code=%d body=%s; want 204", code, body)
	}

	if !svc.dhcpManager.IsRunning(netID) {
		t.Errorf("DHCP was stopped for network %s after deleting one of its two DHCP subnets; the "+
			"surviving subnet %s still needs serving and its guests stop renewing until the next "+
			"reconcile pass", netID[:8], keep[:8])
	}
}

// ...and when the last one goes there is nothing left to serve, so it must stop.
func TestDeleteSubnetStopsDHCPWhenTheLastSubnetGoes(t *testing.T) {
	requireDB(t)
	r, svc := newNeutronTestRouterWithService()

	netID := seedExternalNetwork(t, "dhcp-last-"+uuid.NewString()[:8], seededProjectID, false, false)
	only := seedDHCPSubnet(t, netID, "dhcp-last-a", "10.62.0.0/24")
	markDHCPServing(t, svc, netID)

	code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+only, neutronAuthHdr(seededProjectID, false))
	if code != http.StatusNoContent {
		t.Fatalf("delete subnet: code=%d body=%s; want 204", code, body)
	}

	if svc.dhcpManager.IsRunning(netID) {
		t.Errorf("DHCP still serving network %s after its last DHCP subnet was deleted; a dnsmasq "+
			"with no subnet to serve is a leak", netID[:8])
	}
}
