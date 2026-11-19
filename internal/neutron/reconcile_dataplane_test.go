package neutron

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T039 (FR-010, SC-014; research R24): a reconciliation pass lists the node first, then reads
// every floating IP's record, and converges both ways, one address at a time under the node lock. It
// removes only Lightstack's state, and nothing after an incomplete read.

func newReconcileRig(t *testing.T) (*Service, *fakeDataplane, *NetworkReconciler) {
	t.Helper()
	svc, fake := newRefreshService(t)
	return svc, fake, NewNetworkReconciler(svc, 0)
}

// seedUnassociatedFIP records a floating IP with no association, and returns its address.
func seedUnassociatedFIP(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	addr := fmt.Sprintf("198.18.201.%d", 1+rand.IntN(254))
	mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_ip_address, status) VALUES ($1, $2, $3, 'DOWN')`,
		id, seededProjectID, addr)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return addr
}

func floatingAddressOf(t *testing.T, fixedIP string) string {
	t.Helper()
	var addr string
	if err := testPool.QueryRow(context.Background(),
		"SELECT floating_ip_address FROM floating_ips WHERE fixed_ip_address = $1", fixedIP).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestReconcileFloatingIPs(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	t.Run("adds: the filter, then the NAT and the address", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		_, fixed, _ := portWithFloatingIP(t, 80)
		addr := floatingAddressOf(t, fixed)
		r.reconcileNAT(ctx)
		want := []string{"EnsureIngressFilter " + fixed, "EnsureFloatingIP " + addr + "->" + fixed}
		if got := fake.writes(); !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}
	})

	t.Run("removes a marked NAT no record holds: the address, the NAT, then the filter", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		fake.seedNAT("198.18.202.7", "10.37.0.7", true)
		fake.seedAddress("198.18.202.7")
		fake.seedFilter("10.37.0.7")
		r.reconcileNAT(ctx)
		want := []string{"WithdrawFloatingIP 198.18.202.7", "RemoveIngressFilter 10.37.0.7"}
		if got := fake.writes(); !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}
	})

	t.Run("removes a filter no record associates and no rule reaches", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		fake.seedFilter("10.37.0.8")
		r.reconcileNAT(ctx)
		if got := fake.writes(); !slices.Equal(got, []string{"RemoveIngressFilter 10.37.0.8"}) {
			t.Errorf("writes %q; want the orphan filter removed", got)
		}
	})

	t.Run("an earlier release's unmarked rule for an associated floating IP is replaced", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		_, fixed, _ := portWithFloatingIP(t, 80)
		addr := floatingAddressOf(t, fixed)
		fake.seedNAT(addr, fixed, false)
		fake.seedAddress(addr)
		r.reconcileNAT(ctx)
		if nat := fake.natOf(addr); len(nat) != 1 || nat[0] != (fakeNAT{fixed: fixed, marked: true}) {
			t.Errorf("NAT of %s: %+v; want the marked rule only", addr, nat)
		}
	})

	t.Run("an unmarked rule no record owns stays, and so does the filter it reaches", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		fake.seedNAT("198.18.202.9", "10.37.0.9", false)
		fake.seedFilter("10.37.0.9")
		r.reconcileNAT(ctx)
		if nat := fake.natOf("198.18.202.9"); len(nat) != 1 {
			t.Errorf("the operator's rule: %+v; want it kept", nat)
		}
		if _, ok := fake.filterOf("10.37.0.9"); !ok {
			t.Error("the filter the operator's rule reaches was removed")
		}
	})

	t.Run("a missing address is added back", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		_, fixed, _ := portWithFloatingIP(t, 80)
		addr := floatingAddressOf(t, fixed)
		fake.seedNAT(addr, fixed, true)
		r.reconcileNAT(ctx)
		if !fake.hasAddress(addr) {
			t.Errorf("the address %s is still missing", addr)
		}
	})

	t.Run("a failed read removes nothing", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		fake.seedNAT("198.18.202.10", "10.37.0.10", true)
		fake.seedFilter("10.37.0.10")
		fake.seedFilter("10.37.0.15") // no rule reaches it: an orphan on a complete read
		release := dbtest.Unavailable(t, testPool, "floating_ips")
		defer release()
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		r.reconcileNAT(dctx)
		if len(fake.natOf("198.18.202.10")) != 1 {
			t.Error("the NAT was removed after a failed read")
		}
		for _, x := range []string{"10.37.0.10", "10.37.0.15"} {
			if _, ok := fake.filterOf(x); !ok {
				t.Errorf("the filter of %s was removed after a failed read", x)
			}
		}
	})

	t.Run("a partial read removes nothing, and ensures what it read", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		port, fixed, _ := portWithFloatingIP(t, 80)
		addr := floatingAddressOf(t, fixed)
		fake.seedNAT("198.18.202.11", "10.37.0.11", true)
		fake.seedFilter("10.37.0.14") // no rule reaches it: an orphan on a complete read
		r.readFloatingIPs = func(context.Context) ([]reconcileFIP, bool, error) {
			return []reconcileFIP{{address: addr, fixed: fixed, port: port}}, false, nil
		}
		r.reconcileNAT(ctx)
		if len(fake.natOf("198.18.202.11")) != 1 {
			t.Error("a marked NAT with no record read was removed after a partial read")
		}
		if _, ok := fake.filterOf("10.37.0.14"); !ok {
			t.Error("a filter was removed after a partial read")
		}
		if nat := fake.natOf(addr); len(nat) != 1 || nat[0].fixed != fixed {
			t.Errorf("NAT of %s: %+v; want the association that was read ensured", addr, nat)
		}
	})

	t.Run("an unassociated floating IP's leftovers are Lightstack's and go", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		addr := seedUnassociatedFIP(t)
		fake.seedNAT(addr, "10.37.0.12", false)
		fake.seedAddress(addr)
		fake.seedFilter("10.37.0.12")
		r.reconcileNAT(ctx)
		if len(fake.natOf(addr)) != 0 || fake.hasAddress(addr) {
			t.Errorf("%s still has NAT %+v or its address", addr, fake.natOf(addr))
		}
		if _, ok := fake.filterOf("10.37.0.12"); ok {
			t.Error("the filter its rule reached is still there")
		}
	})

	t.Run("…unless something else still reaches the filter", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		addr := seedUnassociatedFIP(t)
		fake.seedNAT(addr, "10.37.0.13", false)
		fake.seedNAT("198.18.202.13", "10.37.0.13", false) // an operator's rule, no record
		fake.seedFilter("10.37.0.13")
		r.reconcileNAT(ctx)
		if _, ok := fake.filterOf("10.37.0.13"); !ok {
			t.Error("a filter another rule still reaches was removed")
		}
	})

	t.Run("no uplink: no call", func(t *testing.T) {
		svc, fake, r := newReconcileRig(t)
		portWithFloatingIP(t, 80)
		svc.uplink = func() (string, error) { return "", errors.New("no IPv4 default route / uplink found") }
		r.reconcileNAT(ctx)
		if calls := fake.allCalls(); len(calls) != 0 {
			t.Errorf("calls %+v; want none", calls)
		}
	})

	t.Run("unchanged: reads only", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		_, fixed, _ := portWithFloatingIP(t, 80)
		r.reconcileNAT(ctx) // converge once
		before := len(fake.writes())
		r.reconcileNAT(ctx)
		if got := fake.writes()[before:]; len(got) != 0 {
			t.Errorf("a pass over a matching node wrote %q", got)
		}
		if _, ok := fake.filterOf(fixed); !ok {
			t.Errorf("no filter for %s after the first pass", fixed)
		}
	})

	t.Run("a held lock skips that address; the next pass converges it", func(t *testing.T) {
		svc, fake, r := newReconcileRig(t)
		portA, fixedA, _ := portWithFloatingIP(t, 80)
		_, fixedB, _ := portWithFloatingIP(t, 22)
		addrA, addrB := floatingAddressOf(t, fixedA), floatingAddressOf(t, fixedB)
		entered, release := fake.blockFirst(fixedA)
		t.Cleanup(release)
		done := make(chan error, 1)
		go func() { done <- svc.RefreshPortFilters(ctx, []string{portA}) }()
		waitEntered(t, entered)
		time.AfterFunc(1300*time.Millisecond, release) // after the pass gave up on the first address

		r.reconcileNAT(ctx)
		<-done
		first, second := addrA, addrB
		if addrB < addrA {
			first, second = addrB, addrA
		}
		if len(fake.natOf(first)) != 0 {
			t.Errorf("the pass converged %s while the lock was held", first)
		}
		if len(fake.natOf(second)) != 1 {
			t.Errorf("the pass did not finish the other address %s", second)
		}
		r.reconcileNAT(ctx)
		if len(fake.natOf(first)) != 1 {
			t.Errorf("the next pass did not converge %s", first)
		}
	})

	t.Run("the step acts on the record as it is then, not as the pass read it", func(t *testing.T) {
		_, fake, r := newReconcileRig(t)
		port, fixed, _ := portWithFloatingIP(t, 80)
		addr := floatingAddressOf(t, fixed)
		r.readFloatingIPs = func(context.Context) ([]reconcileFIP, bool, error) {
			// Another request disassociates it after the pass read the records.
			mustExec(t, "UPDATE floating_ips SET port_id = NULL, fixed_ip_address = NULL WHERE floating_ip_address = $1", addr)
			return []reconcileFIP{{address: addr, fixed: fixed, port: port}}, true, nil
		}
		r.reconcileNAT(ctx)
		if nat := fake.natOf(addr); len(nat) != 0 {
			t.Errorf("NAT of %s: %+v; want none, as the record now says", addr, nat)
		}
	})
}
