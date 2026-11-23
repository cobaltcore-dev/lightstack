package neutron

import (
	"context"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T109 (FR-033, FR-034; research R26, R27): which node holds which unit, and how a
// caller finds a node: routers and DHCP live on the network node, a port's chains on the node it is
// bound to; a node is reached at its registered tunnel address on the Neutron port, and a node the
// registry does not hold as active fails the step.

// seedComputeNode registers a node in the registry's table, removed again when the test ends.
func seedComputeNode(t *testing.T, name, tunnelIP, status string, heartbeat time.Time) {
	t.Helper()
	mustExec(t, `INSERT INTO compute_nodes (id, hostname, tunnel_ip, status, last_heartbeat, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5, $5)`, uuid.NewString(), name, tunnelIP, status, heartbeat)
	t.Cleanup(func() { mustExec(t, "DELETE FROM compute_nodes WHERE hostname = $1", name) })
}

func TestUnitHolders(t *testing.T) {
	requireDB(t)

	t.Run("routers and DHCP: the network node", func(t *testing.T) {
		_, svc := nodeTarget(t, "spec033-node-a", "spec033-node-b")
		if got := svc.networkNodeName(); got != "spec033-node-b" {
			t.Errorf("network node %q; want spec033-node-b", got)
		}
		if svc.isNetworkNode() {
			t.Error("node a reports itself the network node")
		}
		_, svc = nodeTarget(t, "spec033-node-b", "spec033-node-b")
		if !svc.isNetworkNode() {
			t.Error("node b does not report itself the network node")
		}
	})

	t.Run("a single node is its own network node", func(t *testing.T) {
		svc := NewService(testPool, "stub", nil)
		if !svc.isNetworkNode() {
			t.Errorf("a service with no node settings: network node %q, node %q; want itself", svc.networkNodeName(), svc.nodes.Name)
		}
	})
}

func TestNodeAddress(t *testing.T) {
	requireDB(t)
	registry, err := compute.NewNodeRegistry(testPool, "", "127.0.0.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, svc := newNeutronTestRouterWithService()
	svc.SetNodes(NodeOptions{Name: "spec033-addr-self", NetworkNode: "spec033-addr-self", Registry: registry, Port: 9696, Secret: testNodeSecret})
	ctx := context.Background()
	tag := uuid.NewString()[:6]

	v4, v6 := "spec033-addr-v4-"+tag, "spec033-addr-v6-"+tag
	seedComputeNode(t, v4, "10.99.0.7", "active", time.Now())
	seedComputeNode(t, v6, "fd00::7", "active", time.Now())
	for node, want := range map[string]string{v4: "http://10.99.0.7:9696", v6: "http://[fd00::7]:9696"} {
		if got, err := svc.nodeAddress(ctx, node); err != nil || got != want {
			t.Errorf("address of %s: %q, %v; want %q", node, got, err, want)
		}
	}

	stale, down := "spec033-addr-stale-"+tag, "spec033-addr-down-"+tag
	seedComputeNode(t, stale, "10.99.0.8", "active", time.Now().Add(-time.Hour))
	seedComputeNode(t, down, "10.99.0.9", "inactive", time.Now())
	for _, node := range []string{stale, down, "spec033-addr-never-" + tag} {
		if got, err := svc.nodeAddress(ctx, node); err == nil {
			t.Errorf("address of %s, which the registry does not hold as active: %q; want the step to fail", node, got)
		}
	}

	// Without VXLAN there is no registry: there is no other node to reach.
	single := NewService(testPool, "stub", nil)
	if got, err := single.nodeAddress(ctx, v4); err == nil {
		t.Errorf("a single node's address of another node: %q; want the step to fail", got)
	}
}
