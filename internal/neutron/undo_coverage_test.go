package neutron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T199 (FR-008, FR-028, FR-049; constitution Article II): the undo paths the
// Article II exception rests on, each driven through a failing node step after the store, and two
// refusals that had no test.

func TestUndoPathsPutTheRecordBack(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	errUnit := errors.New("the network node could not converge the unit")
	gatewayAddresses := func(t *testing.T, routerID string) (string, []string) {
		t.Helper()
		ports := gatewayPorts(t, routerID)
		if len(ports) != 1 {
			t.Fatalf("gateway ports %+v; want one", ports)
		}
		var fixed common.FixedIPs
		if err := testPool.QueryRow(ctx, "SELECT fixed_ips FROM ports WHERE id = $1", ports[0].id).Scan(&fixed); err != nil {
			t.Fatal(err)
		}
		var addresses []string
		for _, f := range fixed {
			addresses = append(addresses, f.IPAddress)
		}
		return ports[0].id, addresses
	}

	t.Run("a router gateway's move is undone, its port put back as it was", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, _ := rig.router(t)
		port, before := gatewayAddresses(t, routerID)
		rig.routers.failFirst(1, errUnit)
		code, body := doJSONReq(rig.r, http.MethodPut, "/v2.0/routers/"+routerID,
			`{"router":{"external_gateway_info":{"network_id":"`+rig.external+`","external_fixed_ips":[{"ip_address":"`+rig.externalPrefix+`.77"}]}}}`, admin)
		rig.routers.failFirst(0, nil)
		if code != http.StatusInternalServerError {
			t.Fatalf("move the gateway with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if after, addresses := gatewayAddresses(t, routerID); after != port || len(addresses) != 1 || addresses[0] != before[0] {
			t.Errorf("the gateway port after the undo: %s %v; want %s %v", after, addresses, port, before)
		}
	})

	t.Run("a subnet's gateway and pools are undone together", func(t *testing.T) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-src27-"+uuid.NewString()[:6])
		subnet := rig.subnet(t, rig.hdr, network, `"cidr":"10.97.1.0/24","allocation_pools":[{"start":"10.97.1.10","end":"10.97.1.50"}]`)
		rig.dhcp.failFirst(1, errUnit)
		code, body := rig.do(t, rig.hdr, http.MethodPut, "/v2.0/subnets/"+subnet,
			`{"subnet":{"gateway_ip":"10.97.1.254","allocation_pools":[{"start":"10.97.1.100","end":"10.97.1.150"}]}}`)
		rig.dhcp.failFirst(0, nil)
		if code != http.StatusInternalServerError {
			t.Fatalf("update with a failing DHCP unit: code=%d body=%s; want 500", code, body)
		}
		_, shown := rig.do(t, rig.hdr, http.MethodGet, "/v2.0/subnets/"+subnet, "")
		var out struct {
			Subnet struct {
				Gateway string `json:"gateway_ip"`
				Pools   []struct {
					Start string `json:"start"`
					End   string `json:"end"`
				} `json:"allocation_pools"`
			} `json:"subnet"`
		}
		if err := json.Unmarshal([]byte(shown), &out); err != nil {
			t.Fatal(err)
		}
		if s := out.Subnet; s.Gateway != "10.97.1.1" || len(s.Pools) != 1 || s.Pools[0].Start != "10.97.1.10" || s.Pools[0].End != "10.97.1.50" {
			t.Errorf("the subnet after the undo: %+v; want gateway 10.97.1.1 and the pool 10.97.1.10-10.97.1.50 back", s)
		}
	})

	// failOnceDoing fails the router unit's first call, running act during it, as a concurrent
	// request would.
	failOnceDoing := func(rig *unitRig, act func()) {
		var once sync.Once
		rig.routers.hook = func(holderCall[networking.RouterState]) error {
			err := error(nil)
			once.Do(func() { act(); err = errUnit })
			return err
		}
	}

	t.Run("a floating IP's deletion undone without the router that went meanwhile", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		failOnceDoing(rig, func() { mustExec(t, "DELETE FROM routers WHERE id = $1", routerID) })
		if code, body := rig.do(t, http.MethodDelete, "/v2.0/floatingips/"+fip, ""); code != http.StatusInternalServerError {
			t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		var router sql.NullString
		if err := testPool.QueryRow(ctx, "SELECT router_id::text FROM floating_ips WHERE id = $1", fip).Scan(&router); err != nil || router.Valid {
			t.Errorf("the restored floating IP's router: %v (%v); want it restored without one", router, err)
		}
	})

	t.Run("a floating IP of a project that went meanwhile stays deleted", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		project := createTestProject(t, "spec033-src27-fip")
		var out struct {
			FloatingIP struct {
				ID string `json:"id"`
			} `json:"floatingip"`
		}
		code, body := doJSONReq(rig.r, http.MethodPost, "/v2.0/floatingips",
			`{"floatingip":{"floating_network_id":"`+rig.external+`","port_id":"`+p+`","project_id":"`+project+`"}}`, admin)
		if code != http.StatusCreated || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("an administrator's floating IP for the project: code=%d body=%s", code, body)
		}
		failOnceDoing(rig, func() { mustExec(t, "DELETE FROM projects WHERE id = $1", project) })
		if code, body := doJSONReq(rig.r, http.MethodDelete, "/v2.0/floatingips/"+out.FloatingIP.ID, "", admin); code != http.StatusInternalServerError {
			t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		var n int
		if err := testPool.QueryRow(ctx, "SELECT count(*) FROM floating_ips WHERE id = $1", out.FloatingIP.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d floating IP row(s) (%v); want it left deleted, its project gone", n, err)
		}
	})

	t.Run("a port forwarding's move to another port, applied and undone", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, network := rig.router(t)
		p, x := rig.port(t, network)
		q, y := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		pf := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2240)), "port_forwarding")
		move := func(port, address string) (int, string) {
			return rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip+"/port_forwardings/"+pf,
				`{"port_forwarding":{"internal_port_id":"`+port+`","internal_ip_address":"`+address+`"}}`)
		}
		held := func() string {
			if s := rig.held(t, routerID); len(s.PortForwardings) == 1 {
				return s.PortForwardings[0].InternalIP
			}
			return ""
		}
		if code, body := move(q, y); code != http.StatusOK || held() != y {
			t.Fatalf("move to the other port: code=%d body=%s, held %q; want 200 and %s held", code, body, held(), y)
		}
		rig.routers.failFirst(1, errUnit)
		code, body := move(p, x)
		rig.routers.failFirst(0, nil)
		var port, address string
		if err := testPool.QueryRow(ctx, "SELECT internal_port_id::text, internal_ip_address FROM port_forwardings WHERE id = $1", pf).Scan(&port, &address); err != nil {
			t.Fatal(err)
		}
		if code != http.StatusInternalServerError || port != q || address != y || held() != y {
			t.Errorf("a move the router cannot take: code=%d body=%s, record %s %s, held %q; want 500 and %s %s kept", code, body, port, address, held(), q, y)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, x := rig.port(t, network)
		fip, _ := rig.floatingIP(t, "")
		pf := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+fip+"/port_forwardings", forwardingBody(p, x, 2241)), "port_forwarding")
		if code, body := rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip+"/port_forwardings/"+pf, `{"port_forwarding":{"internal_port":70000}}`); code != http.StatusBadRequest {
			t.Errorf("internal_port out of range on update: code=%d body=%s; want 400", code, body)
		}
		gatewayless := seedNetwork(t, "spec033-src27-nogw-"+uuid.NewString()[:6])
		seedSubnet(t, gatewayless, "spec033-src27-nogw", "10.96.7.0/24", nil, 4, false)
		q, _ := rig.port(t, gatewayless)
		// SPEC-033 T218: no router joins such a subnet — an interface needs its gateway — so
		// it is upstream's 404 (FR-028); upstream's get_router_for_floatingip has no gateway check.
		code, body := rig.do(t, http.MethodPost, "/v2.0/floatingips", `{"floatingip":{"floating_network_id":"`+rig.external+`","port_id":"`+q+`"}}`)
		if code != http.StatusNotFound || neutronErrorType(t, body) != "ExternalGatewayForFloatingIPNotFound" {
			t.Errorf("a floating IP on a port of a subnet without a gateway: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", code, body)
		}
	})
}
