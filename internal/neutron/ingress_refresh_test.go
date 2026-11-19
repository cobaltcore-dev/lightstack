package neutron

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T009: a port's floating-IP filters are rebuilt from the database through the node's
// dataplane, one address at a time under the node lock (research R8, R9; FR-007, FR-007b, FR-010).
// Stub mode, with the service's dataplane replaced by the in-memory fake.

func newRefreshService(t *testing.T) (*Service, *fakeDataplane) {
	t.Helper()
	svc := NewService(testPool, "stub", nil)
	fake := newFakeDataplane()
	svc.dataplane = fake
	return svc, fake
}

// sgTestRule adds an IPv4 ingress rule for one port to a group.
func sgTestRule(t *testing.T, groupID, protocol string, port int) networking.IngressRule {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO security_group_rules (security_group_id, direction, ethertype, protocol, port_range_min, port_range_max)
		VALUES ($1, 'ingress', 'IPv4', $2, $3, $3)`, groupID, protocol, port); err != nil {
		t.Fatalf("seed rule %s/%d: %v", protocol, port, err)
	}
	return networking.IngressRule{Protocol: protocol, PortMin: port, PortMax: port}
}

// seedFloatingIP records a floating IP associated with portID at fixedIP. Nothing is programmed.
func seedFloatingIP(t *testing.T, portID, fixedIP string) string {
	t.Helper()
	id := uuid.NewString()
	addr := fmt.Sprintf("198.18.%d.%d", rand.IntN(256), 1+rand.IntN(254))
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO floating_ips (id, project_id, floating_ip_address, fixed_ip_address, port_id, status)
		VALUES ($1, $2, $3, $4, $5, 'ACTIVE')`, id, seededProjectID, addr, fixedIP, portID); err != nil {
		t.Fatalf("seed floating IP: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return addr
}

func testFixedIP() string { return fmt.Sprintf("10.33.%d.%d", rand.IntN(256), 1+rand.IntN(254)) }

// sameRules compares two rule lists regardless of order: the database returns them in any order.
func sameRules(a, b []networking.IngressRule) bool {
	order := func(x, y networking.IngressRule) int {
		return cmp.Or(cmp.Compare(x.Protocol, y.Protocol), cmp.Compare(x.PortMin, y.PortMin),
			cmp.Compare(x.PortMax, y.PortMax), cmp.Compare(x.Remote, y.Remote))
	}
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, order)
	slices.SortFunc(b, order)
	return slices.Equal(a, b)
}

// portWithFloatingIP builds a port in a group with one tcp rule, and a floating IP on it.
func portWithFloatingIP(t *testing.T, port int) (portID, fixedIP string, rule networking.IngressRule) {
	t.Helper()
	portID = sgTestPort(t)
	g := sgTestGroup(t, seededProjectID, "spec033-refresh-"+uuid.NewString()[:6])
	rule = sgTestRule(t, g, "tcp", port)
	sgTestAttach(t, portID, g)
	fixedIP = testFixedIP()
	seedFloatingIP(t, portID, fixedIP)
	return portID, fixedIP, rule
}

func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked dataplane call never started")
	}
}

func TestRefreshPortFilters(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	t.Run("one floating IP: one rebuild with the port's rules", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		port, fixed, rule := portWithFloatingIP(t, 80)
		if err := svc.RefreshPortFilters(ctx, []string{port}); err != nil {
			t.Fatalf("RefreshPortFilters: %v", err)
		}
		calls := fake.callsFor(fixed)
		if len(calls) != 1 {
			t.Fatalf("calls for %s: %+v; want one", fixed, calls)
		}
		if !sameRules(calls[0].Rules, []networking.IngressRule{rule}) {
			t.Errorf("rules: %+v; want [%+v]", calls[0].Rules, rule)
		}
	})

	t.Run("two floating IPs give two rebuilds, none gives none", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		port, fixed, _ := portWithFloatingIP(t, 80)
		second := testFixedIP()
		seedFloatingIP(t, port, second)
		bare := sgTestPort(t)
		if err := svc.RefreshPortFilters(ctx, []string{port, bare}); err != nil {
			t.Fatalf("RefreshPortFilters: %v", err)
		}
		if n := len(fake.allCalls()); n != 2 || len(fake.callsFor(fixed)) != 1 || len(fake.callsFor(second)) != 1 {
			t.Errorf("calls: %+v; want one for %s and one for %s", fake.allCalls(), fixed, second)
		}
	})

	t.Run("a port with no group left gets the reply-only filter", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		port, fixed, _ := portWithFloatingIP(t, 80)
		if _, err := testPool.Exec(ctx, "DELETE FROM port_security_groups WHERE port_id = $1", port); err != nil {
			t.Fatal(err)
		}
		if err := svc.RefreshPortFilters(ctx, []string{port}); err != nil {
			t.Fatalf("RefreshPortFilters: %v", err)
		}
		calls := fake.callsFor(fixed)
		if len(calls) != 1 || len(calls[0].Rules) != 0 {
			t.Errorf("calls: %+v; want one with no rules", calls)
		}
	})

	t.Run("the dataplane's error is returned, naming the port and the address", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		port, fixed, _ := portWithFloatingIP(t, 80)
		boom := errors.New("iptables-restore: line 4 failed")
		fake.failWith(boom)
		err := svc.RefreshPortFilters(ctx, []string{port})
		if !errors.Is(err, boom) {
			t.Fatalf("RefreshPortFilters: got %v, want the dataplane's error", err)
		}
		if msg := err.Error(); !strings.Contains(msg, port) || !strings.Contains(msg, fixed) {
			t.Errorf("error %q does not name port %s and address %s", msg, port, fixed)
		}
	})

	t.Run("node lock: one rebuild at a time, the rules read inside it", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		portX, fixedX, ruleX := portWithFloatingIP(t, 80)
		portY, fixedY, _ := portWithFloatingIP(t, 22)

		entered, release := fake.blockFirst(fixedX)
		t.Cleanup(release)
		errs := make(chan error, 3)
		go func() { errs <- svc.RefreshPortFilters(ctx, []string{portX}) }()
		waitEntered(t, entered)
		go func() { errs <- svc.RefreshPortFilters(ctx, []string{portY}) }()
		go func() { errs <- svc.RefreshPortFilters(ctx, []string{portX}) }()
		time.Sleep(200 * time.Millisecond)
		if calls := fake.allCalls(); len(calls) != 1 {
			t.Fatalf("while the first rebuild holds the lock, the dataplane saw %+v; want only the first", calls)
		}

		// Another request changes X's groups while the first rebuild still holds the lock.
		db := sgTestGroup(t, seededProjectID, "spec033-refresh-"+uuid.NewString()[:6])
		ruleNew := sgTestRule(t, db, "tcp", 443)
		sgTestAttach(t, portX, db)
		release()
		for range 3 {
			if err := <-errs; err != nil {
				t.Errorf("RefreshPortFilters: %v", err)
			}
		}

		calls := fake.callsFor(fixedX)
		want := []networking.IngressRule{ruleX, ruleNew}
		if len(calls) != 2 || !sameRules(calls[1].Rules, want) {
			t.Fatalf("calls for X: %+v; want a second rebuild carrying %+v", calls, want)
		}
		if got, _ := fake.filterOf(fixedX); !sameRules(got, want) {
			t.Errorf("X's filter: %+v; want %+v", got, want)
		}
		if len(fake.callsFor(fixedY)) != 1 {
			t.Errorf("calls for Y: %+v; want one", fake.callsFor(fixedY))
		}
	})

	t.Run("lock deadline: a busy node fails the change within about one second", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		portX, fixedX, _ := portWithFloatingIP(t, 80)
		portY, fixedY, _ := portWithFloatingIP(t, 22)

		entered, release := fake.blockFirst(fixedX)
		t.Cleanup(release)
		done := make(chan error, 1)
		go func() { done <- svc.RefreshPortFilters(ctx, []string{portX}) }()
		waitEntered(t, entered)

		start := time.Now()
		err := svc.RefreshPortFilters(ctx, []string{portY})
		elapsed := time.Since(start)
		release()
		if err == nil {
			t.Fatal("RefreshPortFilters on a busy node: got nil, want an error")
		}
		if elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Errorf("RefreshPortFilters took %v; want about one second", elapsed)
		}
		if !strings.Contains(err.Error(), fixedY) {
			t.Errorf("error %q does not name the address %s", err, fixedY)
		}
		if calls := fake.callsFor(fixedY); len(calls) != 0 {
			t.Errorf("the dataplane saw %+v for Y; want nothing", calls)
		}
		if err := <-done; err != nil {
			t.Errorf("the first rebuild: %v", err)
		}
	})

	t.Run("no uplink: the change fails, naming the port, and nothing is applied", func(t *testing.T) {
		svc, fake := newRefreshService(t)
		svc.uplink = func() (string, error) { return "", errors.New("no IPv4 default route / uplink found") }
		port, _, _ := portWithFloatingIP(t, 80)
		err := svc.RefreshPortFilters(ctx, []string{port})
		if err == nil || !strings.Contains(err.Error(), port) {
			t.Fatalf("RefreshPortFilters without an uplink: got %v, want an error naming port %s", err, port)
		}
		if calls := fake.allCalls(); len(calls) != 0 {
			t.Errorf("the dataplane saw %+v; want nothing", calls)
		}
	})
}

// A reconciliation pass rebuilds filters through the same locked path: while a change holds the lock
// it skips the address, and the next pass rebuilds it (R8, FR-010).
func TestReconcileNATUsesTheNodeLock(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc, fake := newRefreshService(t)
	r := NewNetworkReconciler(svc, 0)
	port, fixed, rule := portWithFloatingIP(t, 80)

	r.reconcileNAT(ctx)
	calls := fake.callsFor(fixed)
	if len(calls) != 1 || !sameRules(calls[0].Rules, []networking.IngressRule{rule}) {
		t.Fatalf("the first pass: %+v; want one rebuild with [%+v]", calls, rule)
	}

	entered, release := fake.blockFirst(fixed)
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- svc.RefreshPortFilters(ctx, []string{port}) }()
	waitEntered(t, entered)
	held := len(fake.callsFor(fixed))

	r.reconcileNAT(ctx)
	if n := len(fake.callsFor(fixed)); n != held {
		t.Errorf("a pass during a held lock reached the dataplane for %s (%d calls, want %d)", fixed, n, held)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("RefreshPortFilters: %v", err)
	}

	r.reconcileNAT(ctx)
	if n := len(fake.callsFor(fixed)); n != held+1 {
		t.Errorf("the next pass made %d calls for %s; want one more than %d", n, fixed, held)
	}
}
