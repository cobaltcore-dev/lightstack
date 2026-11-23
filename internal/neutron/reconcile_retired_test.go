package neutron

import (
	"context"
	"errors"
	"testing"
)

// SPEC-033 round 2, T143 (FR-042, FR-044; research R29, R33, R40), as SPEC-033 T190 narrowed it:
// every node removes what earlier layouts left and this one no longer writes, once per process — it is
// never written again — as far as a mark or a name only Lightstack gives makes it Lightstack's
// (pkg/networking's RetiredState). A pass hands the node no records: a tenant chooses a subnet's CIDR,
// and an operator's own masquerade or DNAT has the earlier layout's shape. The records are the
// operator's teardown's to use (lightstack cleanup).

var errNoUplinkForTest = errors.New("no default route")

func TestReconcileRemovesTheRetiredStateOncePerProcess(t *testing.T) {
	rig := newReconcileRig(t)
	rig.topology(t)

	for _, n := range []*reconcileNode{rig.net, rig.cmp} {
		n.reconcile(context.Background())
		n.reconcile(context.Background())
		calls := n.retired.recorded()
		if len(calls) != 1 {
			t.Fatalf("%s: %d removals in two passes; want one", n.svc.nodes.Name, len(calls))
		}
		c := calls[0]
		if c.uplink != "eth0" {
			t.Errorf("%s: uplink %q; want the node's", n.svc.nodes.Name, c.uplink)
		}
		if len(c.records.FloatingIPs)+len(c.records.SubnetCIDRs) != 0 {
			t.Errorf("%s: records %+v handed to the removal; want none", n.svc.nodes.Name, c.records)
		}
	}

	t.Run("a failed removal is made again the next pass", func(t *testing.T) {
		rig := newReconcileRig(t)
		rig.cmp.retired.failNext(errors.New("iptables-save: exit status 4"))
		rig.cmp.reconcile(context.Background())
		rig.cmp.reconcile(context.Background())
		rig.cmp.reconcile(context.Background())
		if calls := rig.cmp.retired.recorded(); len(calls) != 2 {
			t.Errorf("%d removals in three passes, the first failing; want two", len(calls))
		}
	})

	t.Run("without an uplink the rest still goes", func(t *testing.T) {
		rig := newReconcileRig(t)
		rig.cmp.svc.uplink = func() (string, error) { return "", errNoUplinkForTest }
		rig.cmp.reconcile(context.Background())
		if calls := rig.cmp.retired.recorded(); len(calls) != 1 || calls[0].uplink != "" {
			t.Errorf("removals %+v; want one, with no uplink", calls)
		}
	})
}
