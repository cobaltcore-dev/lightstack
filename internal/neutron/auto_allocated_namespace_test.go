package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) F10, second path — the auto-allocated topology handlers used to build
// their bridge inside light-ns-<project> while every other real-mode path used the namespace Lightstack runs
// in. That produced a second bridge of the same name that nothing could reach: EnsureBridgeIP and
// ListManagedBridges are netlink-only, so the namespaced bridge could never be given its gateway
// address and was invisible to the reconciler's prune, while the guest TAPs, dnsmasq and the NAT rules
// were all on the root-namespace bridge.
//
// The placement itself is no longer expressible — BridgeManager takes no namespace — so what these
// tests cover is the handler behaviour around the change: dropping the CreateNamespace call must not
// alter what the API does, and the delete path must still be reached. Placement remains a real-mode
// property that stub mode cannot observe (the create block is behind `if svc.mode != "stub"`), so it
// needs host validation, exactly as the port half did.

// TestAutoAllocatedTopologyCreateIsIdempotent — a repeat POST returns the SAME network rather than
// creating a second one or erroring.
//
// This mattered for the retired in-namespace path in a way it does not now: that path shelled out to
// `ip link add`, which fails outright on a second call, where the netlink path tolerates an existing
// bridge. The handler short-circuits before either, so the guarantee is the handler's — assert it, so
// removing the namespace plumbing cannot have moved it.
func TestAutoAllocatedTopologyCreateIsIdempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	cleanupAutoAllocated(t)

	firstID := postAutoAllocated(t, r, hdr, http.StatusCreated)
	secondID := postAutoAllocated(t, r, hdr, http.StatusOK)

	if firstID != secondID {
		t.Errorf("repeat POST produced a different topology: %q then %q; it must return the existing one", firstID, secondID)
	}

	var count int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM networks WHERE project_id = $1 AND name = 'auto-allocated-network'",
		seededProjectID).Scan(&count); err != nil {
		t.Fatalf("count auto-allocated networks: %v", err)
	}
	if count != 1 {
		t.Errorf("found %d auto-allocated networks, want exactly 1", count)
	}
}

// TestAutoAllocatedTopologyDeleteRemovesTheTopology — DELETE takes the network and its subnets with it
// and leaves GET reporting 404, so the reconciler's prune sees a bridge with no network row and can
// reclaim it. Before the fix the handler deleted a bridge in a namespace instead, leaving the real one
// for the reconciler to notice later; the row-level contract asserted here is what that prune keys on.
func TestAutoAllocatedTopologyDeleteRemovesTheTopology(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	cleanupAutoAllocated(t)

	netID := postAutoAllocated(t, r, hdr, http.StatusCreated)

	code, body := doReqBody(r, http.MethodDelete, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete auto-allocated topology: code=%d body=%s; want 204", code, body)
	}

	var networks, subnets int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM networks WHERE id = $1", netID).Scan(&networks); err != nil {
		t.Fatalf("count networks: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM subnets WHERE network_id = $1", netID).Scan(&subnets); err != nil {
		t.Fatalf("count subnets: %v", err)
	}
	if networks != 0 || subnets != 0 {
		t.Errorf("after delete: %d network row(s), %d subnet row(s); want 0 and 0", networks, subnets)
	}

	if code, _ := doReqBody(r, http.MethodGet, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr); code != http.StatusNotFound {
		t.Errorf("GET after delete: code=%d, want 404", code)
	}
}

// TestAutoAllocatedTopologyDeleteTearsDownTheHost — the topology's network gets the host teardown every
// network delete gets: its DHCP server, its metadata DNAT and its bridge. The handler used to delete
// only the bridge, so dnsmasq and the DNAT rule waited for the reconciler's next pass. Stub mode cannot
// observe the DNAT; the same helper removes it, between the two steps checked here.
func TestAutoAllocatedTopologyDeleteTearsDownTheHost(t *testing.T) {
	requireDB(t)
	r, svc := newNeutronTestRouterWithService()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	cleanupAutoAllocated(t)

	netID := postAutoAllocated(t, r, hdr, http.StatusCreated)
	markDHCPServing(t, svc, netID)
	bridge := plantStubBridge(t, svc, netID)

	code, body := doReqBody(r, http.MethodDelete, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete auto-allocated topology: code=%d body=%s; want 204", code, body)
	}
	if svc.dhcpManager.IsRunning(netID) {
		t.Errorf("DHCP still serving network %s after its topology was deleted", netID[:8])
	}
	if hasStubBridge(t, svc, bridge) {
		t.Errorf("bridge %s outlived its topology", bridge)
	}
}

// TestAutoAllocatedTopologyDeleteRefusesWhileInUse — a topology whose network still carries a VM's port
// stays, refused with NetworkInUse. The handler used to delete the subnets and the network regardless,
// cascading the port away under the running VM. Upstream's delete_auto_allocated_topology deletes the
// network through delete_network, whose NetworkInUse propagates to the caller.
func TestAutoAllocatedTopologyDeleteRefusesWhileInUse(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r, svc := newNeutronTestRouterWithService()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	cleanupAutoAllocated(t)

	netID := postAutoAllocated(t, r, hdr, http.StatusCreated)
	portID := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address)
		VALUES ($1, 'spec024-vm-port', $2, $3, 'fa:16:3e:00:00:04')`, portID, netID, seededProjectID)
	markDHCPServing(t, svc, netID)

	code, body := doReqBody(r, http.MethodDelete, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr)
	if code != http.StatusConflict {
		t.Fatalf("delete auto-allocated topology in use: code=%d body=%s; want 409", code, body)
	}
	assertNeutronShape(t, neutronError(t, body), "NetworkInUse")

	var ports, subnets int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM ports WHERE id = $1", portID).Scan(&ports); err != nil {
		t.Fatalf("count ports: %v", err)
	}
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM subnets WHERE network_id = $1", netID).Scan(&subnets); err != nil {
		t.Fatalf("count subnets: %v", err)
	}
	if ports != 1 || subnets == 0 {
		t.Errorf("after a refused delete: %d port(s), %d subnet(s); want the port and the subnet kept", ports, subnets)
	}
	if !svc.dhcpManager.IsRunning(netID) {
		t.Errorf("a refused delete stopped DHCP for network %s", netID[:8])
	}
}

// cleanupAutoAllocated removes the project's auto-allocated topology after the test, whatever state it
// ended in. Registered unconditionally rather than relying on the handler's own DELETE: these tests
// share one long-lived postgres with every other worktree, and a topology left behind changes what the
// next run of these very tests observes (the create path returns 200-with-the-existing-row, not 201).
func cleanupAutoAllocated(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		var id string
		if err := testPool.QueryRow(ctx,
			"SELECT id FROM networks WHERE project_id = $1 AND name = 'auto-allocated-network'",
			seededProjectID).Scan(&id); err != nil {
			return
		}
		testPool.Exec(ctx, "DELETE FROM ports WHERE network_id = $1", id)   //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM subnets WHERE network_id = $1", id) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", id)        //nolint:errcheck // best-effort cleanup
	})
}

// postAutoAllocated posts the topology and returns its id, asserting the status on the way.
func postAutoAllocated(t *testing.T, r http.Handler, hdr map[string]string, wantCode int) string {
	t.Helper()

	code, body := doReqBody(r, http.MethodPost, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr)
	if code != wantCode {
		t.Fatalf("post auto-allocated topology: code=%d body=%s; want %d", code, body, wantCode)
	}

	var resp struct {
		Topology struct {
			ID string `json:"id"`
		} `json:"auto_allocated_topology"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("parse topology: %v", err)
	}
	if resp.Topology.ID == "" {
		t.Fatalf("topology reported no id: %s", body)
	}
	return resp.Topology.ID
}
