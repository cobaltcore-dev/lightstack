package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T187 (FR-028, FR-039): a floating IP's router is chosen from records read
// unlocked, and a removal of the router's interface that holds the router's lock may commit before the
// create takes it. The create chooses again under the lock and answers 404
// ExternalGatewayForFloatingIPNotFound when the router no longer joins the port's subnet to the floating
// network, storing nothing. PostgreSQL only: on SQLite one transaction holds the whole file, so the
// choice and the lock cannot be interleaved.

func TestFloatingIPRouterIsChosenAgainUnderItsLock(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t)
	for _, other := range []bool{false, true} {
		name := "the router lost its interface"
		if other {
			name = "another router serves the subnet now"
		}
		t.Run(name, func(t *testing.T) { floatingIPRouterRace(t, other) })
	}
}

// floatingIPRouterRace creates a floating IP on a port of a subnet a router joins to the external
// network, while a transaction holding the router's lock removes that interface; with other, a second
// router also joins the subnet, which the create, choosing again, would pick instead.
func floatingIPRouterRace(t *testing.T, other bool) {
	ctx := context.Background()
	rr := newRouterRig(t)
	code, body, router := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create router: code=%d body=%s", code, body)
	}
	gw := "10.94.0.1"
	internal := seedNetwork(t, "spec033-src15-"+uuid.NewString()[:6])
	subnet := seedSubnet(t, internal, "spec033-src15", "10.94.0.0/24", &gw, 4, false)
	if code, body := doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+router+"/add_router_interface",
		`{"subnet_id":"`+subnet+`"}`, rr.member); code != http.StatusOK {
		t.Fatalf("add interface: code=%d body=%s", code, body)
	}
	var iface string
	if err := testPool.QueryRow(ctx, "SELECT port_id::text FROM router_interfaces WHERE router_id = $1", router).Scan(&iface); err != nil {
		t.Fatal(err)
	}
	if other {
		// A second router with a gateway and an interface on the subnet at another address, as a router
		// added by port would have.
		code, body, second := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("create the second router: code=%d body=%s", code, body)
		}
		port := uuid.NewString()
		mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
			VALUES ($1, '', $2, $3, $4, 'network:router_interface', 'fa:16:3e:15:00:02', 'ACTIVE', $5::jsonb)`,
			port, internal, seededProjectID, second, `[{"subnet_id":"`+subnet+`","ip_address":"10.94.0.2"}]`)
		mustExec(t, "INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)", second, port, subnet)
	}
	vm := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-src15-vm', $2, $3, 'fa:16:3e:15:00:01', 'ACTIVE', $4::jsonb)`,
		vm, internal, seededProjectID, `[{"subnet_id":"`+subnet+`","ip_address":"10.94.0.5"}]`)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", vm) })
	cleanupFloatingIPsOn(t, rr.external)

	// The interface's removal, as RemoveRouterInterface makes it, under the router's lock, held until the
	// create waits for that lock: the create has chosen the router by then.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op once committed
	holder := dbtest.HolderPID(t, tx)
	if _, err := tx.Exec(ctx, "SELECT id FROM routers WHERE id = $1 FOR UPDATE", router); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM ports WHERE id::text = $1", iface); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	go func() {
		code, body, _ := createFloatingIP(rr.r, rr.member, `{"floatingip":{"floating_network_id":"`+rr.external+`","port_id":"`+vm+`"}}`)
		done <- answer{code, body}
	}()
	dbtest.WaitUntilBlocked(t, testPool, holder)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got := <-done
	if got.code != http.StatusNotFound || neutronErrorType(t, got.body) != "ExternalGatewayForFloatingIPNotFound" {
		t.Errorf("create through the router whose interface went meanwhile: code=%d body=%s; want 404 ExternalGatewayForFloatingIPNotFound", got.code, got.body)
	}
	var stored int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM floating_ips WHERE port_id = $1", vm).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("%d floating IP(s) on the port (%v); want none", stored, err)
	}
}
