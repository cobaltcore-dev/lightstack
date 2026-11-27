package neutron

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T219: a unit's lock lives while a request holds or waits for it and goes with
// the last of them, so a node's lock table holds the units being converged, not every router and
// network it ever converged. The lock still excludes, and a wait still ends at its deadline.
func TestUnitLocksKeepOnlyTheUnitsInUse(t *testing.T) {
	var l unitLocks
	held := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.slots)
	}

	release, err := l.acquire(context.Background(), "router:a")
	if err != nil {
		t.Fatal(err)
	}
	if n := held(); n != 1 {
		t.Errorf("%d slot(s) while one unit is held; want 1", n)
	}

	// A wait for the held unit ends at its deadline, busy, and leaves the holder its slot.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "router:a"); !errors.Is(err, errUnitBusy) {
		t.Errorf("acquire a held unit: %v; want errUnitBusy at the deadline", err)
	}
	if n := held(); n != 1 {
		t.Errorf("%d slot(s) after a wait gave up; want the holder's 1", n)
	}

	// A waiter gets the unit once the holder lets it go, and the slot goes with the last of them.
	var wg sync.WaitGroup
	wg.Add(1)
	acquired := make(chan struct{})
	go func() {
		defer wg.Done()
		r, err := l.acquire(context.Background(), "router:a")
		if err != nil {
			t.Error(err)
			return
		}
		close(acquired)
		r()
	}()
	select {
	case <-acquired:
		t.Fatal("a second request held the unit while the first did")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	wg.Wait()
	if n := held(); n != 0 {
		t.Errorf("%d slot(s) once no request holds or waits; want none", n)
	}

	for _, key := range []string{"router:b", "dhcp:c", "ports"} {
		r, err := l.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		r()
	}
	if n := held(); n != 0 {
		t.Errorf("%d slot(s) after three units converged one after another; want none", n)
	}
}

// slowFirewall converges in a fixed time, as a node's ip link, four *-save and two *-restore take, and
// counts its converges.
type slowFirewall struct {
	d     time.Duration
	calls atomic.Int32
}

func (f *slowFirewall) ConvergePorts(ctx context.Context, _ []networking.FirewallPort) error {
	f.calls.Add(1)
	select {
	case <-time.After(f.d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*slowFirewall) RemoveAll(context.Context) error { return nil }

// SPEC-033 T253 (FR-008, R27): requests waiting together on a unit share one converge. A converge that
// began after a request joined read its committed records, so once it succeeds the request has what it
// asked for. Each waiter re-ran the node's whole port convergence, one after another, so twenty guests
// booting at once on one node ran out of the 900 ms wait and ended in ERROR.
func TestUnitWaitersShareAConverge(t *testing.T) {
	requireDB(t)
	_, svc := newNeutronTestRouterWithService()
	node := "spec033-t253-" + uuid.NewString()[:6]
	svc.SetNodes(NodeOptions{Name: node})
	svc.brManager = newFakeBridges()
	fw := &slowFirewall{d: 100 * time.Millisecond}
	svc.firewall = fw
	network := seedNetwork(t, "spec033-t253-"+uuid.NewString()[:6])
	ports := make([]string, 20)
	for i := range ports {
		ports[i] = seedBarePort(t, network, seededProjectID)
	}
	var failed atomic.Int32
	var wg sync.WaitGroup
	for _, p := range ports {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := svc.BindPort(context.Background(), p, node); err != nil {
				failed.Add(1)
				t.Logf("bind %s: %v", p, err)
			}
		}(p)
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Errorf("%d of %d parallel binds failed; want none", n, len(ports))
	}
	if calls := fw.calls.Load(); calls >= int32(len(ports)) {
		t.Errorf("%d converges for %d binds; want the waiters to share them", calls, len(ports))
	}
}
