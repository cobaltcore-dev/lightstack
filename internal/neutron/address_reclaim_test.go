package neutron

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T188 (FR-008): an undo or a restore that puts an address back — a deleted
// floating IP's, a router's previous gateway port's, a subnet's previous gateway — takes the
// allocator's locks and puts nothing back when something took the address while the node call failed:
// the later change stays, and the address keeps one holder.

// holdersOf counts what holds address on network: ports, and floating IPs.
func holdersOf(t *testing.T, network, address string) int {
	t.Helper()
	ctx := context.Background()
	rows, err := testPool.Query(ctx, "SELECT fixed_ips FROM ports WHERE network_id = $1", network)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var fixed common.FixedIPs
		if err := rows.Scan(&fixed); err != nil {
			t.Fatal(err)
		}
		for _, f := range fixed {
			if f.IPAddress == address {
				n++
			}
		}
	}
	rows.Close()
	var floating int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM floating_ips WHERE floating_network_id = $1 AND floating_ip_address = $2",
		network, address).Scan(&floating); err != nil {
		t.Fatal(err)
	}
	return n + floating
}

// takeAddress inserts a port on network holding address of subnet, as a concurrent allocation would.
func takeAddress(t *testing.T, network, subnet, address string) {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-src16', $2, $3, 'compute:nova', 'fa:16:3e:16:00:01', 'ACTIVE', $4::jsonb)`,
		id, network, seededProjectID, `[{"subnet_id":"`+subnet+`","ip_address":"`+address+`"}]`)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", id) })
}

// failOnceTaking fails a node call once, taking address meanwhile.
func failOnceTaking(t *testing.T, network, subnet, address string) func() error {
	var once sync.Once
	return func() error {
		var err error
		once.Do(func() {
			takeAddress(t, network, subnet, address)
			err = errors.New("the node could not converge the unit")
		})
		return err
	}
}

func subnetOf(t *testing.T, network string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), "SELECT id::text FROM subnets WHERE network_id = $1", network).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUndoAndRestoreLeaveAnAddressTakenMeanwhile(t *testing.T) {
	requireDB(t)
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	t.Run("a floating IP's deletion", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		fip, address := rig.floatingIP(t, p)
		fail := failOnceTaking(t, rig.external, subnetOf(t, rig.external), address)
		rig.routers.hook = func(holderCall[networking.RouterState]) error { return fail() }
		if code, body := rig.do(t, http.MethodDelete, "/v2.0/floatingips/"+fip, ""); code != http.StatusInternalServerError {
			t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if n := holdersOf(t, rig.external, address); n != 1 {
			t.Errorf("%d holder(s) of %s; want one, the port that took it: the floating IP stays deleted", n, address)
		}
	})

	t.Run("a router gateway's change", func(t *testing.T) {
		rig := newUnitRig(t)
		routerID, _ := rig.router(t)
		ports := gatewayPorts(t, routerID)
		var fixed common.FixedIPs
		if err := testPool.QueryRow(context.Background(), "SELECT fixed_ips FROM ports WHERE id = $1", ports[0].id).Scan(&fixed); err != nil || len(fixed) != 1 {
			t.Fatalf("the gateway port's addresses %+v (%v); want one", fixed, err)
		}
		old, moved := fixed[0].IPAddress, rig.externalPrefix+".77"
		fail := failOnceTaking(t, rig.external, fixed[0].SubnetID, old)
		rig.routers.hook = func(holderCall[networking.RouterState]) error { return fail() }
		if code, body := doJSONReq(rig.r, http.MethodPut, "/v2.0/routers/"+routerID,
			`{"router":{"external_gateway_info":{"network_id":"`+rig.external+`","external_fixed_ips":[{"ip_address":"`+moved+`"}]}}}`,
			admin); code != http.StatusInternalServerError {
			t.Fatalf("move the gateway with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if n := holdersOf(t, rig.external, old); n != 1 {
			t.Errorf("%d holder(s) of %s; want one, the port that took it: the gateway stays at %s", n, old, moved)
		}
	})

	t.Run("a subnet gateway's change", func(t *testing.T) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-src16-"+uuid.NewString()[:6])
		subnet := rig.subnet(t, rig.hdr, network, `"cidr":"10.98.0.0/24","allocation_pools":[{"start":"10.98.0.10","end":"10.98.0.100"}]`)
		fail := failOnceTaking(t, network, subnet, "10.98.0.1")
		rig.dhcp.hook = func(holderCall[networking.DHCPState]) error { return fail() }
		if code, body := rig.do(t, rig.hdr, http.MethodPut, "/v2.0/subnets/"+subnet, `{"subnet":{"gateway_ip":"10.98.0.254"}}`); code != http.StatusInternalServerError {
			t.Fatalf("change the gateway with a failing DHCP unit: code=%d body=%s; want 500", code, body)
		}
		var gateway string
		if err := testPool.QueryRow(context.Background(), "SELECT COALESCE(gateway_ip, '') FROM subnets WHERE id = $1", subnet).Scan(&gateway); err != nil {
			t.Fatal(err)
		}
		if gateway != "10.98.0.254" {
			t.Errorf("the subnet's gateway after the failed update: %q; want 10.98.0.254 kept, the port took 10.98.0.1", gateway)
		}
	})
}
