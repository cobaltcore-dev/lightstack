package nova

import (
	"context"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
)

// SPEC-025 T036 (FR-024, FR-024b): drift the product did not cause must be corrected.
//
// Every action now persists its own result, so a stop or start needs no sweep. This
// covers what nothing in the request path can hear about — a guest that crashed, or
// one removed out of band — which is the state that left the API reporting a running
// server that did not exist (cloud-storage#547 B12).
func TestStateReconcilerCorrectsAVanishedGuest(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	vanished, vanishedDomain := seedLifecycleInstance(t, mgr, "spec025-recon-gone")
	alive, _ := seedLifecycleInstance(t, mgr, "spec025-recon-alive")

	// One guest disappears out of band; the other keeps running.
	if err := mgr.DeleteVM(t.Context(), vanishedDomain); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	NewStateReconciler(svc, time.Minute).sweep(context.Background())

	if status, power := readInstanceState(t, vanished); status != "SHUTOFF" || power != 4 {
		t.Errorf("vanished guest: status=%q power=%d; want SHUTOFF/4 — the record still claims it runs", status, power)
	}
	if status, power := readInstanceState(t, alive); status != "ACTIVE" || power != 1 {
		t.Errorf("live guest: status=%q power=%d; want ACTIVE/1 — the sweep must not touch a healthy server", status, power)
	}
}

// A hypervisor that cannot be reached is not evidence that a guest is gone. Correcting
// the record on that basis would replace a stale answer with a wrong one.
func TestStateReconcilerLeavesTheRecordAloneWhenItCannotTell(t *testing.T) {
	requireDB(t)
	_, _, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-recon-unknown")

	// A service with no hypervisor at all: the sweep must be a no-op, not a mass
	// correction of every running server.
	blind := NewService(testPool, "", "real", nil)
	t.Cleanup(blind.Shutdown)
	NewStateReconciler(blind, time.Minute).sweep(context.Background())

	if status, _ := readInstanceState(t, id); status != "ACTIVE" {
		t.Errorf("status = %q; a reconciler with no hypervisor must change nothing", status)
	}
}

// FR-024d: a non-positive configured interval is clamped, not honored — 0 would mean a
// busy loop.
func TestStateReconcilerClampsItsInterval(t *testing.T) {
	for _, in := range []time.Duration{0, -1 * time.Second} {
		if got := NewStateReconciler(nil, in).Interval(); got != common.DefaultStateReconcileInterval {
			t.Errorf("interval for %v = %v; want the default %v", in, got, common.DefaultStateReconcileInterval)
		}
	}
	if got := NewStateReconciler(nil, 5*time.Second).Interval(); got != 5*time.Second {
		t.Errorf("an explicit interval must be honored, got %v", got)
	}
}

// The sweep must only consider servers the record claims are running. A server already
// recorded as stopped needs no correcting, and touching it would churn updated_at on
// every tick.
func TestStateReconcilerIgnoresServersAlreadyStopped(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-recon-stopped")

	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4, updated_at = $1 WHERE id = $2`,
		time.Unix(0, 0), id); err != nil {
		t.Fatalf("pre-setting the stopped state: %v", err)
	}
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	var before time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT updated_at FROM instances WHERE id = $1`, id).Scan(&before); err != nil {
		t.Fatalf("reading updated_at: %v", err)
	}

	NewStateReconciler(svc, time.Minute).sweep(context.Background())

	var after time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT updated_at FROM instances WHERE id = $1`, id).Scan(&after); err != nil {
		t.Fatalf("reading updated_at: %v", err)
	}
	if !after.Equal(before) {
		t.Error("an already-stopped server was rewritten; the sweep must consider only servers recorded as running")
	}
}

// T042 (FR-024b): drift is not only a VANISHED guest.
//
// The sweep started life looking for one shape of divergence — a record claiming ACTIVE
// with no guest behind it — and was blind to every other. A host probe found the rest:
// a guest that is still running while the record says SHUTOFF, and a guest that has
// shut down while the record says ACTIVE. In both the domain exists, so the lookup
// succeeds and the old sweep skipped the row; in the second the record is not ACTIVE,
// so the row was never even selected.
func TestStateReconcilerCorrectsAGuestThatDisagrees(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	// Recorded stopped, actually running — what an optimistic os-stop used to leave
	// behind, and what no sweep could see because it selected only ACTIVE rows.
	runningGuest, _ := seedLifecycleInstance(t, mgr, "spec025-recon-up")
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, runningGuest); err != nil {
		t.Fatalf("pre-setting the diverged record: %v", err)
	}

	// Recorded running, actually off — a guest shut down from inside the VM. The
	// domain still exists, so the vanished-guest check does not fire.
	stoppedGuest, stoppedDomain := seedLifecycleInstance(t, mgr, "spec025-recon-down")
	if err := mgr.StopVM(t.Context(), stoppedDomain, 0); err != nil {
		t.Fatalf("stopping the stub guest: %v", err)
	}

	NewStateReconciler(svc, time.Minute).sweep(context.Background())

	if status, power := readInstanceState(t, runningGuest); status != "ACTIVE" || power != 1 {
		t.Errorf("running guest recorded as stopped: status=%q power=%d; want ACTIVE/1 — "+
			"the sweep only ever looked for the opposite divergence", status, power)
	}
	if status, power := readInstanceState(t, stoppedGuest); status != "SHUTOFF" || power != 4 {
		t.Errorf("stopped guest recorded as running: status=%q power=%d; want SHUTOFF/4 — "+
			"a domain that exists but is off was skipped because the lookup succeeded", status, power)
	}
}

// The sweep judges only the two statuses it can judge. A server that is building,
// shelved or in error says something about itself that the hypervisor cannot confirm or
// deny, and rewriting it to SHUTOFF would destroy information rather than correct it.
func TestStateReconcilerLeavesUnjudgeableStatusesAlone(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	for _, status := range []string{"BUILD", "ERROR", "SHELVED"} {
		t.Run(status, func(t *testing.T) {
			id, domainID := seedLifecycleInstance(t, mgr, "spec025-recon-"+status)
			if _, err := testPool.Exec(t.Context(),
				`UPDATE instances SET status = $1, power_state = 0 WHERE id = $2`, status, id); err != nil {
				t.Fatalf("pre-setting %s: %v", status, err)
			}
			// The guest is gone AND the record disagrees — both triggers at once, so
			// only the status gate can hold the sweep back.
			if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
				t.Fatalf("removing the stub guest: %v", err)
			}

			NewStateReconciler(svc, time.Minute).sweep(context.Background())

			if got, _ := readInstanceState(t, id); got != status {
				t.Errorf("status = %q; a %s server must be left alone — the hypervisor has no "+
					"opinion on it", got, status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Run and the failure paths. Branch review (SRC-4) found Run and sweepAll at 0%
// coverage: every existing test called sweep() directly, so the loop that decides WHEN a
// sweep happens, and the cancellation that stops it, were never executed.

// waitForStatus polls until the record reaches want, or fails. Polling rather than a
// fixed sleep: the reconciler runs on its own goroutine and a sleep long enough to be
// reliable is long enough to be slow.
func waitForStatus(t *testing.T, instanceID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var status string
		if err := testPool.QueryRow(context.Background(),
			`SELECT status FROM instances WHERE id = $1`, instanceID).Scan(&status); err != nil {
			t.Fatalf("reading status: %v", err)
		}
		if status == want {
			return
		}
		last = status
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status stayed %q; want %q within 3s", last, want)
}

// TestStateReconcilerRunSweepsAtStartup covers the startup sweep ON ITS OWN, with an
// interval long enough that no tick can fire inside the test.
//
// That separation is the point. A first attempt used a 20ms interval and asserted the
// pre-existing divergence was corrected — which passed with the startup sweep DELETED,
// because the first tick corrected it 20ms later. The two halves of Run's contract have
// to be pinned by two tests with different intervals, or neither is pinned.
//
// The startup sweep is not a nicety: if the process was down while a guest crashed,
// nothing in the request path will ever notice, and the record would claim the server
// runs until someone tried an action on it.
func TestStateReconcilerRunSweepsAtStartup(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	id, domainID := seedLifecycleInstance(t, mgr, "spec025-run-startup")
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		// An hour: only the sweep Run performs BEFORE entering its loop can correct
		// anything within this test.
		NewStateReconciler(svc, time.Hour).Run(ctx)
		close(returned)
	}()

	waitForStatus(t, id, "SHUTOFF")

	// And it must stop when told: a reconciler that ignores cancellation outlives the
	// process's shutdown and keeps writing.
	cancel()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestStateReconcilerRunSweepsOnEachTick covers the other half: drift that appears while
// the process is up. The divergence is created AFTER Run has started and had time to
// perform its startup sweep, so only a tick can account for the correction.
func TestStateReconcilerRunSweepsOnEachTick(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	returned := make(chan struct{})
	go func() {
		NewStateReconciler(svc, 20*time.Millisecond).Run(ctx)
		close(returned)
	}()

	// Let the startup sweep run and find nothing to do before creating the drift.
	time.Sleep(100 * time.Millisecond)

	id, domainID := seedLifecycleInstance(t, mgr, "spec025-run-tick")
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}
	waitForStatus(t, id, "SHUTOFF")

	cancel()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestStateReconcilerSurvivesAFailedQuery covers the error paths whose whole purpose is
// that a blip must not kill the goroutine, and must not corrupt the record either.
//
// A cancelled context is the deterministic way to make both statements fail without
// breaking the pool for every other test — the query and the DELETE both return an error
// rather than a result, which is exactly the branch under test.
func TestStateReconcilerSurvivesAFailedQuery(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-recon-dberr")
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	r := NewStateReconciler(svc, time.Minute).WithActionRetention(time.Hour)
	// Neither call may panic, and neither may write anything.
	r.sweep(dead)
	r.reclaimActionHistory(dead)

	if status, _ := readInstanceState(t, id); status != "ACTIVE" {
		t.Errorf("status = %q; a sweep that could not read the database must change nothing, not guess", status)
	}

	// And the reconciler is still usable afterwards: the failure must not have left it
	// in a state where the next sweep is a no-op.
	r.sweep(context.Background())
	if status, _ := readInstanceState(t, id); status != "SHUTOFF" {
		t.Errorf("status after a healthy sweep = %q; want SHUTOFF — a transient failure must not disable the reconciler", status)
	}
}

// TestStateReconcilerVerdictYieldsToANewerWrite: the sweep forms a verdict from a snapshot of the
// row and a hypervisor sample taken a moment later. A start or stop that persists in between must
// win — including one that leaves the status as it was, which a guard on status alone cannot see:
// a server read ACTIVE with its guest down, started meanwhile and recorded ACTIVE again, would be
// written SHUTOFF by the stale verdict. Upstream runs the same sync under a per-instance lock
// shared with stop_instance (nova/compute/manager.py _sync_power_states).
func TestStateReconcilerVerdictYieldsToANewerWrite(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	r := NewStateReconciler(svc, time.Minute)
	ctx := context.Background()

	snapshot := func(id string) reconcileCandidate {
		t.Helper()
		cands, err := r.readCandidates(ctx)
		if err != nil {
			t.Fatalf("readCandidates: %v", err)
		}
		for _, c := range cands {
			if c.instanceID == id {
				return c
			}
		}
		t.Fatalf("instance %s is not a candidate", id)
		return reconcileCandidate{}
	}

	// A verdict formed before a concurrent write that kept the status: refused.
	raced, _ := seedLifecycleInstance(t, mgr, "spec025-reconcile-raced")
	cand := snapshot(raced)
	if _, err := testPool.Exec(ctx,
		`UPDATE instances SET status = 'ACTIVE', power_state = 1, updated_at = updated_at + interval '1 second' WHERE id = $1`,
		raced); err != nil {
		t.Fatalf("concurrent start: %v", err)
	}
	applied, err := r.applyVerdict(ctx, cand, "SHUTOFF", 4)
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if status, power := readInstanceState(t, raced); applied || status != "ACTIVE" || power != 1 {
		t.Errorf("stale verdict after a newer write: applied=%v, record %s/%d; want it refused and ACTIVE/1 kept", applied, status, power)
	}

	// The same verdict with nothing in between: written. Without this the guard could refuse
	// everything and the test above would still pass.
	quiet, _ := seedLifecycleInstance(t, mgr, "spec025-reconcile-quiet")
	applied, err = r.applyVerdict(ctx, snapshot(quiet), "SHUTOFF", 4)
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if status, power := readInstanceState(t, quiet); !applied || status != "SHUTOFF" || power != 4 {
		t.Errorf("verdict with no concurrent write: applied=%v, record %s/%d; want it written as SHUTOFF/4", applied, status, power)
	}

	// A row whose updated_at was never set is still correctable: the guard compares NULL safely.
	unset, _ := seedLifecycleInstance(t, mgr, "spec025-reconcile-null-updated")
	if _, err := testPool.Exec(ctx, `UPDATE instances SET updated_at = NULL WHERE id = $1`, unset); err != nil {
		t.Fatalf("clear updated_at: %v", err)
	}
	applied, err = r.applyVerdict(ctx, snapshot(unset), "SHUTOFF", 4)
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if status, _ := readInstanceState(t, unset); !applied || status != "SHUTOFF" {
		t.Errorf("verdict on a row with no updated_at: applied=%v, status %s; want it written", applied, status)
	}
}
