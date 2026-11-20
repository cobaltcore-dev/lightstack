package compute

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

var (
	testPool        database.DB
	testDBAvailable bool
)

func TestMain(m *testing.M) {
	testPool, testDBAvailable = dbtest.Open("../../migrations")
	code := m.Run()
	dbtest.Close(testPool)
	os.Exit(code)
}

// SPEC-033 round 2, T063 (research R26): the registry keys a node by its node name, the one name
// the whole process uses — LIGHTSTACK_NODE_NAME when set (the chart sets it from the pod's node),
// the host name otherwise — so neutron.network_node and a port's binding_host_id name a node the
// registry knows. It used to read os.Hostname() alone, which inside a pod is the pod's name.
func TestRegistryUsesTheNodeName(t *testing.T) {
	const node = "ls579-node-name-a"
	t.Setenv("LIGHTSTACK_NODE_NAME", node)

	nr, err := NewNodeRegistry(testPool, "auto", "192.0.2.57", time.Minute)
	if err != nil {
		t.Fatalf("NewNodeRegistry: %v", err)
	}
	if got := nr.GetHostname(); got != node {
		t.Fatalf("the registry names this node %q; want the node name %q", got, node)
	}

	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to check the registered rows")
	}
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM compute_nodes WHERE hostname = $1`, node)
	})

	if err := nr.RegisterNode(ctx); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	var tunnel string
	if err := testPool.QueryRow(ctx, `SELECT tunnel_ip FROM compute_nodes WHERE hostname = $1`, node).Scan(&tunnel); err != nil {
		t.Fatalf("no compute_nodes row under the node name: %v", err)
	}
	if tunnel != "192.0.2.57" {
		t.Errorf("tunnel_ip = %q; want 192.0.2.57", tunnel)
	}

	old := time.Now().Add(-time.Hour).UTC()
	if _, err := testPool.Exec(ctx, `UPDATE compute_nodes SET last_heartbeat = $1 WHERE hostname = $2`, old, node); err != nil {
		t.Fatal(err)
	}
	if err := nr.sendHeartbeat(ctx); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}
	var beat time.Time
	if err := testPool.QueryRow(ctx, `SELECT last_heartbeat FROM compute_nodes WHERE hostname = $1`, node).Scan(&beat); err != nil {
		t.Fatal(err)
	}
	if time.Since(beat) > time.Minute {
		t.Errorf("last_heartbeat under the node name is %v; want the heartbeat to have moved it", beat)
	}
}
