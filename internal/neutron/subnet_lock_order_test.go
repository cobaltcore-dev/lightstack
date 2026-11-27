package neutron

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T206 (FR-032): a port's requested fixed IPs lock the subnets they name in the
// network's order — creation order, as every other allocation locks them — before any is resolved, so
// two creates naming them in opposite orders cannot deadlock. PostgreSQL only: a transaction holds the
// first subnet's lock while a create names [second, first]; once the create waits, the second subnet
// must still be free, and the create goes through when the lock is released.

func TestRequestedFixedIPsLockSubnetsInTheNetworksOrder(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	network := seedNetwork(t, "spec033-src29-"+uuid.NewString()[:6])
	first := seedSubnet(t, network, "spec033-src29-a", "10.71.1.0/24", nil, 4, false)
	second := seedSubnet(t, network, "spec033-src29-b", "10.71.2.0/24", nil, 4, false)
	mustExec(t, "UPDATE subnets SET created_at = $1 WHERE id = $2", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), first) // the network's first
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", network) })

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op once committed
	holder := dbtest.HolderPID(t, tx)
	if _, err := LockSubnet(ctx, tx, first); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := doJSONReq(r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+network+`","fixed_ips":[{"subnet_id":"`+second+`"},{"subnet_id":"`+first+`"}]}}`, hdr)
		done <- code
	}()
	dbtest.WaitUntilBlocked(t, testPool, holder)
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	//sqlcheck:postgres-only
	_, probeErr := probe.Exec(ctx, "SELECT id FROM subnets WHERE id = $1 FOR UPDATE NOWAIT", second)
	_ = probe.Rollback(ctx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusCreated {
		t.Errorf("the create once the lock was released: code=%d; want 201", code)
	}
	if probeErr != nil {
		t.Errorf("the second subnet was locked while the create waited for the first (%v): locks taken in the request's order, which deadlocks against [first, second]", probeErr)
	}
}
