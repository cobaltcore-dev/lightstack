package neutron

import (
	"context"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// In stub mode the network purge must be a no-op (no DB access, no host calls) —
// it returns early before any enumeration. Real-mode behavior is host-validated on
// a cluster (SPEC-010 / cloud-storage#472).
func TestPurgeRealModeNetworkState_StubIsNoOp(t *testing.T) {
	svc := NewService(testPool, "stub", nil)
	if err := svc.PurgeRealModeNetworkState(context.Background()); err != nil {
		t.Fatalf("stub-mode network purge should be a no-op, got: %v", err)
	}
}

// SPEC-033 round 2, T145 (research R48): the real-mode purge removes the new layout. Every router and
// network DHCP the records name, and every one the node holds besides, converges to absent; the node's
// whole port firewall goes, its fixed chains included; the retired state of earlier layouts goes as
// reconciliation removes it; then the bridges and the project namespaces of older deployments.
func TestPurgeRealModeNetworkState_RemovesTheLayout(t *testing.T) {
	requireDB(t)
	svc := NewService(testPool, "iptables", nil)
	routers, dhcp, firewall, retired := newFakeRouters(), newFakeDHCP(), newFakeFirewall(), &fakeRetired{}
	bridges, projects := newFakeBridges(), &fakeProjectNamespaces{}
	svc.routers, svc.dhcp, svc.firewall, svc.retired, svc.brManager, svc.nsManager = routers, dhcp, firewall, retired, bridges, projects
	svc.uplink = func() (string, error) { return "eth0", nil }

	network := seedNetwork(t, "spec033-purge-"+uuid.NewString()[:6])
	router := uuid.NewString()
	mustExec(t, "INSERT INTO routers (id, name, project_id, status) VALUES ($1, 'spec033-purge', $2, 'ACTIVE')", router, seededProjectID)
	t.Cleanup(func() { mustExec(t, "DELETE FROM routers WHERE id = $1", router) })
	strayRouter, strayNetwork := uuid.NewString(), uuid.NewString()
	routers.plant(router, networking.RouterState{ID: router})
	routers.plant(strayRouter, networking.RouterState{ID: strayRouter})
	dhcp.plant(network, networking.DHCPState{NetworkID: network})
	dhcp.plant(strayNetwork, networking.DHCPState{NetworkID: strayNetwork})
	firewall.plant(networking.FirewallPort{ID: uuid.NewString()})
	strayBridge := networking.BridgeNameForNetwork(uuid.NewString())
	for _, b := range []string{networking.BridgeNameForNetwork(network), strayBridge} {
		if err := bridges.CreateBridge(b); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.PurgeRealModeNetworkState(context.Background()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if held := routers.heldIDs(); len(held) != 0 {
		t.Errorf("routers still held: %q", held)
	}
	if held := dhcp.heldIDs(); len(held) != 0 {
		t.Errorf("networks' DHCP still held: %q", held)
	}
	if firewall.removedAll != 1 || len(firewall.heldPorts()) != 0 {
		t.Errorf("the firewall was removed %d times and holds %q; want removed whole, once", firewall.removedAll, firewall.heldPorts())
	}
	if calls := retired.recorded(); len(calls) != 1 || calls[0].uplink != "eth0" {
		t.Errorf("retired-state removals %+v; want one, on the uplink", calls)
	}
	if got, _ := bridges.ListManagedBridges(); len(got) != 0 {
		t.Errorf("bridges left: %q", got)
	}
	if !slices.Contains(projects.deleted, seededProjectID) {
		t.Errorf("project namespaces deleted %q; want the seeded project's among them", projects.deleted)
	}
}
