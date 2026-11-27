package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T185 (FR-040): both requests that delete a network — DELETE /v2.0/networks/{id}
// and the auto-allocated topology's DELETE — go through one sequence. The network's DHCP leaves the
// network node, then the record goes in a transaction that locks it and checks again that nothing uses
// it. A port created while the DHCP was being removed keeps the network (409), and its DHCP is put
// back; the topology's delete used to check once, unlocked, and the cascade took the port.

func TestNetworkDeleteKeepsAPortCreatedMeanwhile(t *testing.T) {
	requireDB(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	for _, route := range []struct {
		name string
		path func(network string) string
	}{
		{"DELETE /v2.0/networks/{id}", func(network string) string { return "/v2.0/networks/" + network }},
		{"DELETE /v2.0/auto-allocated-topology/{project}", func(string) string { return "/v2.0/auto-allocated-topology/" + seededProjectID }},
	} {
		t.Run(route.name, func(t *testing.T) {
			r, svc := newNeutronTestRouterWithService()
			dhcp := newFakeDHCP()
			svc.dhcp, svc.brManager = dhcp, newFakeBridges()
			cleanupAutoAllocated(t)
			network := postAutoAllocated(t, r, hdr, http.StatusCreated)
			port := uuid.NewString()
			t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", port) })
			dhcp.hook = func(call holderCall[networking.DHCPState]) error {
				if call.ID == network && call.State == nil {
					mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_owner, mac_address, status, fixed_ips)
						VALUES ($1, 'spec033-src13', $2, $3, 'compute:nova', 'fa:16:3e:13:00:01', 'ACTIVE', '[]'::jsonb)`,
						port, network, seededProjectID)
				}
				return nil
			}

			if code, body := doReq(r, http.MethodDelete, route.path(network), hdr); code != http.StatusConflict {
				t.Errorf("delete while a port was created: code=%d body=%s; want 409", code, body)
			}
			var ports int
			if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE id = $1", port).Scan(&ports); err != nil || ports != 1 {
				t.Errorf("the port created meanwhile: %d row(s) (%v); want it kept", ports, err)
			}
			if _, held := dhcp.state(network); !held {
				t.Error("the network's DHCP after the refusal: none; want it put back")
			}
		})
	}
}
