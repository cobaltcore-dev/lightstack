package neutron

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T170: the network node routes a router's gateway addresses to
// its leg and answers ARP for them on the uplink. A member's port claiming to be their router's
// gateway put an address of the member's choosing there — the LAN's own gateway, say — so the node
// answered for it on the physical network.

func TestRouterUnitIgnoresAMembersGatewayPort(t *testing.T) {
	requireDB(t)
	rig := newUnitRig(t)
	resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers", `{"router":{"name":"src2-no-gateway"}}`)
	routerID := createdID(t, resp, "router")
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM ports WHERE device_id = $1", routerID)
		mustExec(t, "DELETE FROM routers WHERE id = $1", routerID)
	})
	const lanGateway = "192.0.2.1"
	network := seedNetwork(t, "src2-lan-"+uuid.NewString()[:6])
	elsewhere := "192.0.2.254"
	subnet := seedSubnet(t, network, "src2-lan", "192.0.2.0/24", &elsewhere, 4, false)
	forged := forgeRouterPort(t, ownerRouterGateway, routerID, network, subnet, lanGateway)

	if err := rig.net.convergeRouterUnit(context.Background(), routerID); err != nil {
		t.Fatalf("converge the router: %v", err)
	}
	if s, held := rig.routers.state(routerID); held && (s.Gateway != nil || len(networking.PlanRouter(s).HostAddresses) != 0) {
		t.Errorf("the network node holds gateway %+v, answering for %v; want none: the router has no gateway",
			s.Gateway, networking.PlanRouter(s).HostAddresses)
	}

	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+routerID,
		`{"router":{"external_gateway_info":{"network_id":"`+rig.external+`"}}}`)
	s := rig.held(t, routerID)
	if s.Gateway == nil || s.Gateway.PortID == forged || slices.Contains(networking.PlanRouter(s).HostAddresses, lanGateway) {
		t.Errorf("with a gateway: %+v; want the router's own port, never answering for %s", s.Gateway, lanGateway)
	}
}
