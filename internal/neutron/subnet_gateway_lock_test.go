package neutron

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T269 (FR-055): a subnet's gateway change checks GatewayIpInUse under the subnet's lock, the lock a
// router interface's add takes before it takes the gateway. Checked before the lock, an interface added in
// between kept the old gateway while the subnet advertised the new one. PostgreSQL only: a transaction holds
// the subnet's lock and adds an interface on the gateway; the update waits, and once the lock is released
// it must see the interface and answer 409.
func TestSubnetGatewayInUseIsCheckedUnderTheLock(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t)
	ctx := context.Background()
	r := newEnvelopeRouter()
	gw := "10.95.0.1"
	network := seedNetwork(t, "spec033-t269-"+uuid.NewString()[:6])
	subnet := seedSubnet(t, network, "spec033-t269", "10.95.0.0/24", &gw, 4, false)
	storeTestPools(t, subnet, [2]string{"10.95.0.2", "10.95.0.100"})
	router := uuid.NewString()
	mustExec(t, `INSERT INTO routers (id, name, project_id) VALUES ($1, 'spec033-t269', $2)`, router, seededProjectID)
	cleanupRows(t, "routers", router)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", network) })

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op once committed
	holder := dbtest.HolderPID(t, tx)
	if _, err := LockSubnet(ctx, tx, subnet); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnet, `{"subnet":{"gateway_ip":"10.95.0.254"}}`, nil)
		done <- code
	}()
	dbtest.WaitUntilBlocked(t, testPool, holder)
	port := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		VALUES ($1, 'router-interface', $2, $3, $4, 'network:router_interface', 'fa:16:3e:00:00:69', $5::jsonb)`,
		port, network, seededProjectID, router, fmt.Sprintf(`[{"subnet_id":%q,"ip_address":%q}]`, subnet, gw)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`, router, port, subnet); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusConflict {
		t.Errorf("the gateway change once the interface was added: code=%d; want 409 GatewayIpInUse", code)
	}
	if got := storedGateway(t, subnet); got == nil || *got != gw {
		t.Errorf("stored gateway %s; want %s kept", derefOr(got, "null"), gw)
	}
}
