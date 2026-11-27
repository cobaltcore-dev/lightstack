package neutron

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T252, T261 (FR-040): a server's deletion or detach that did not stand binds the ports it unbound
// back, compare-and-set: only a port that still names the server and is still unbound. A port another
// request has detached, or bound, since stays as that request left it. The record is the binding: when the
// node cannot be converged, the binding stays and the reconciliation pass converges the node from it.
func TestRebindPort(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	binding := func(t *testing.T, port string) string {
		t.Helper()
		var node *string
		if err := testPool.QueryRow(ctx, "SELECT binding_host_id FROM ports WHERE id = $1", port).Scan(&node); err != nil {
			t.Fatal(err)
		}
		if node == nil {
			return ""
		}
		return *node
	}
	attached := func(t *testing.T, rig *portRig, device string) string {
		t.Helper()
		p, _ := rig.port(t, rig.group(t, ruleSSH))
		mustExec(t, "UPDATE ports SET device_id = $1, binding_host_id = NULL WHERE id = $2", device, p)
		return p
	}

	t.Run("a port that still names the server and is unbound is bound back", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		server := uuid.NewString()
		p := attached(t, rig, server)
		if err := rig.api.RebindPort(ctx, p, server, portNode1); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		if got := binding(t, p); got != portNode1 {
			t.Errorf("binding %q; want %s", got, portNode1)
		}
		if _, ok := rig.firewalls[portNode1].port(p); !ok {
			t.Errorf("%s holds no firewall for the port bound back", portNode1)
		}
	})

	t.Run("a port detached since stays detached", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		p := attached(t, rig, uuid.NewString())
		mustExec(t, "UPDATE ports SET device_id = NULL WHERE id = $1", p)
		if err := rig.api.RebindPort(ctx, p, uuid.NewString(), portNode1); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		if got := binding(t, p); got != "" {
			t.Errorf("binding %q; want none: the port is no server's", got)
		}
	})

	t.Run("a port bound since keeps its binding", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		server := uuid.NewString()
		p := attached(t, rig, server)
		mustExec(t, "UPDATE ports SET binding_host_id = $1 WHERE id = $2", portNode2, p)
		if err := rig.api.RebindPort(ctx, p, server, portNode1); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		if got := binding(t, p); got != portNode2 {
			t.Errorf("binding %q; want %s, the later binding", got, portNode2)
		}
	})

	t.Run("a node that cannot be converged keeps the binding for the reconciliation pass", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		server := uuid.NewString()
		p := attached(t, rig, server)
		rig.firewalls[portNode1].failFirst(1, errNode)
		if err := rig.api.RebindPort(ctx, p, server, portNode1); err == nil {
			t.Error("a failing node: no error")
		}
		if got := binding(t, p); got != portNode1 {
			t.Errorf("binding %q; want %s kept, so the reconciliation pass converges that node", got, portNode1)
		}
	})
}
