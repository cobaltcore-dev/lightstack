package neutron

import (
	"context"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T276 (FR-033): a node that is not the network node removes the routers and DHCP it holds only
// while the node neutron.network_node names is an active node. A name that matches no node — a typo, an FQDN
// for a short name — made every node, the intended one included, tear down every router and DHCP on every
// pass. And the setting is read trimmed, as the check that refuses an empty one reads it.
func TestOnlyAnActiveNetworkNodeTakesTheRoutersAway(t *testing.T) {
	rig := newReconcileRig(t)
	registry, err := compute.NewNodeRegistry(testPool, "", "127.0.0.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rig.cmp.svc.nodes.Registry = registry
	stray := uuid.NewString()
	rig.cmp.routers.plant(stray, networking.RouterState{ID: stray})

	rig.cmp.reconcile(context.Background())
	if held := rig.cmp.routers.heldIDs(); len(held) != 1 {
		t.Errorf("routers %q after a pass while the network node is no active node; want the router kept", held)
	}

	seedComputeNode(t, reconcileNetworkNode, "127.0.0.2", "active", time.Now())
	rig.cmp.reconcile(context.Background())
	if held := rig.cmp.routers.heldIDs(); len(held) != 0 {
		t.Errorf("routers %q after a pass once the network node is active; want none", held)
	}
}
