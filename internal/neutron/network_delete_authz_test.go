package neutron

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// DeleteNetwork used to tear down host state before anything confirmed the caller owns the network.
//
// The metadata DNAT and the bridge went first, keyed on the raw path parameter; only the row delete
// after them was scoped to the caller's project. A caller who could merely see a network — shared,
// or external per FR-033a — removed its bridge and was then told 404. The in-use checks in front were
// no barrier either: an id that is not a UUID made both COUNT queries fail, an error they ignored, so
// `<first 8 hex>-x` reached the teardown even for a network in use and named its live bridge.
// Deleting a bridge detaches every guest TAP on it, and the reconciler recreates bridges but never
// re-attaches TAPs. The same checks also answered 409 for a network the caller cannot see, which
// told them it exists.

// TestDeleteNetworkLeavesForeignHostStateAlone — a caller who does not own the network gets 404 and
// changes nothing: not the row, not the bridge.
func TestDeleteNetworkLeavesForeignHostStateAlone(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r, svc := newNeutronTestRouterWithService()
	caller := map[string]string{"X-Test-Project": seededProjectID}

	byID := func(id string) string { return id }
	nonUUID := func(id string) string { return id[:8] + "-x" }

	cases := []struct {
		name             string
		shared, external bool
		inUse            bool // a port, so the in-use check would answer 409 for the real id
		path             func(id string) string
	}{
		{"shared network by id", true, false, false, byID},
		{"external network by id", false, true, false, byID},
		{"shared network in use by a non-UUID id", true, false, true, nonUUID},
		{"private network in use by id", false, false, true, byID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedExternalNetwork(t, "spec024-del-"+uuid.NewString()[:8], otherProjectID, tc.shared, tc.external)
			if tc.inUse {
				// The port goes when seedExternalNetwork's cleanup deletes the network.
				mustExec(t, `INSERT INTO ports (name, network_id, project_id, mac_address)
					VALUES ('spec024-del-port', $1, $2, 'fa:16:3e:00:00:03')`, id, otherProjectID)
			}
			bridge := plantStubBridge(t, svc, id)

			code, body := doReq(r, http.MethodDelete, "/v2.0/networks/"+tc.path(id), caller)
			if code != http.StatusNotFound {
				t.Fatalf("DELETE by a non-owner = %d, want 404 (body %q)", code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "NetworkNotFound")

			if !hasStubBridge(t, svc, bridge) {
				t.Errorf("bridge %s was deleted by a caller who does not own the network", bridge)
			}
			if !networkRowExists(t, id) {
				t.Errorf("network %s was deleted by a caller who does not own it", id)
			}
		})
	}

	// The control: the owner's delete does remove the bridge, so the checks above can see one go.
	t.Run("owner", func(t *testing.T) {
		id := seedExternalNetwork(t, "spec024-del-own-"+uuid.NewString()[:8], seededProjectID, false, false)
		bridge := plantStubBridge(t, svc, id)

		code, body := doReq(r, http.MethodDelete, "/v2.0/networks/"+id, caller)
		if code != http.StatusNoContent {
			t.Fatalf("DELETE by the owner = %d, want 204 (body %q)", code, body)
		}
		if hasStubBridge(t, svc, bridge) {
			t.Errorf("the owner's delete left bridge %s behind", bridge)
		}
		if networkRowExists(t, id) {
			t.Errorf("the owner's delete left network %s behind", id)
		}
	})
}

// plantStubBridge gives a seeded network the bridge CreateNetwork would have built for it.
func plantStubBridge(t *testing.T, svc *Service, networkID string) string {
	t.Helper()
	bridge := "br-" + networkID[:8]
	if err := svc.brManager.CreateBridge(bridge); err != nil {
		t.Fatalf("create stub bridge %s: %v", bridge, err)
	}
	t.Cleanup(func() {
		svc.brManager.DeleteBridge(bridge) //nolint:errcheck // best-effort cleanup
	})
	return bridge
}

func hasStubBridge(t *testing.T, svc *Service, bridge string) bool {
	t.Helper()
	names, err := svc.brManager.ListManagedBridges()
	if err != nil {
		t.Fatalf("list bridges: %v", err)
	}
	return slices.Contains(names, bridge)
}

func networkRowExists(t *testing.T, networkID string) bool {
	t.Helper()
	var exists bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM networks WHERE id = $1)", networkID).Scan(&exists); err != nil {
		t.Fatalf("read network %s: %v", networkID, err)
	}
	return exists
}
