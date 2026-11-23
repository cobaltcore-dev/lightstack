package neutron

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// SPEC-033 round 2, T139 (FR-008; research R40): the auto-allocated topology is built through the same
// functions as its parts: its network's bridge and its subnet's DHCP, port and unit. A unit that cannot
// be converged answers 500 and leaves no half topology.

func TestAutoAllocatedTopologyUnits(t *testing.T) {
	requireDB(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	ctx := context.Background()
	topology := func(t *testing.T) (int, int, int) {
		t.Helper()
		var networks, subnets, dhcpPorts int
		if err := testPool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM networks WHERE project_id = $1 AND name = 'auto-allocated-network'),
			       (SELECT count(*) FROM subnets s JOIN networks n ON n.id = s.network_id
			         WHERE n.project_id = $1 AND n.name = 'auto-allocated-network'),
			       (SELECT count(*) FROM ports p JOIN networks n ON n.id = p.network_id
			         WHERE n.project_id = $1 AND n.name = 'auto-allocated-network' AND p.device_owner = 'network:dhcp')`,
			seededProjectID).Scan(&networks, &subnets, &dhcpPorts); err != nil {
			t.Fatal(err)
		}
		return networks, subnets, dhcpPorts
	}

	t.Run("built with its DHCP", func(t *testing.T) {
		r, svc := newNeutronTestRouterWithService()
		dhcp := newFakeDHCP()
		svc.dhcp, svc.brManager = dhcp, newFakeBridges()
		cleanupAutoAllocated(t)
		network := postAutoAllocated(t, r, hdr, http.StatusCreated)
		if n, s, d := topology(t); n != 1 || s != 1 || d != 1 {
			t.Errorf("topology: %d network(s), %d subnet(s), %d DHCP port(s); want one of each", n, s, d)
		}
		if _, ok := dhcp.state(network); !ok {
			t.Error("the topology's network has no DHCP on the network node")
		}
	})

	for _, failing := range []string{"bridge", "DHCP"} {
		t.Run("a failing "+failing+" leaves nothing", func(t *testing.T) {
			r, svc := newNeutronTestRouterWithService()
			dhcp, bridges := newFakeDHCP(), newFakeBridges()
			svc.dhcp, svc.brManager = dhcp, bridges
			errUnit := errors.New("the unit could not be converged")
			if failing == "bridge" {
				bridges.hook = func(string) error { return errUnit }
			} else {
				dhcp.failFirst(1, errUnit)
			}
			cleanupAutoAllocated(t)
			if code, body := doReqBody(r, http.MethodPost, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr); code != http.StatusInternalServerError {
				t.Fatalf("create: code=%d body=%s; want 500", code, body)
			}
			if n, s, d := topology(t); n+s+d != 0 {
				t.Errorf("left %d network(s), %d subnet(s), %d DHCP port(s); want none", n, s, d)
			}
		})
	}
}
