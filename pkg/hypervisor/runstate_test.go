package hypervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// SPEC-025 US4 (cloud-storage#547 B12): libvirt's "shutdown" is a REQUEST to the guest,
// not a transition. The guest decides whether and when to honour it, so reporting
// success the moment libvirt accepts the request is what let a stop persist SHUTOFF
// over a guest that was still running.
//
// These cover the policy — poll for the transition, force it when the grace period
// expires — as a pure function, because the alternative is a libvirt connection no unit
// test can have.

// T040 (FR-022): a guest that shuts down cleanly inside the grace period is not forced.
func TestWaitForShutoffReturnsWhenTheGuestStops(t *testing.T) {
	reads := 0
	stopped := waitForShutoff(context.Background(), time.Second, time.Millisecond,
		func() (VMState, error) {
			reads++
			if reads < 3 {
				return VMStateRunning, nil
			}
			return VMStateShutoff, nil
		})

	if !stopped {
		t.Error("a guest that reached SHUTOFF was reported as not stopped; the caller would force it off needlessly")
	}
	if reads < 3 {
		t.Errorf("polled %d times; it must keep reading until the state actually changes", reads)
	}
}

// A guest still shutting down is still up. VIR_DOMAIN_SHUTDOWN means "being shut down",
// and returning early on it is how a force-off would land on a guest mid-flush.
func TestWaitForShutoffKeepsWaitingWhileTheGuestIsStillShuttingDown(t *testing.T) {
	reads := 0
	stopped := waitForShutoff(context.Background(), time.Second, time.Millisecond,
		func() (VMState, error) {
			reads++
			if reads < 3 {
				return VMStateShutdown, nil
			}
			return VMStateShutoff, nil
		})

	if !stopped {
		t.Error("SHUTDOWN was treated as terminal; it means the guest is still on its way down")
	}
	if reads < 3 {
		t.Errorf("polled %d times; SHUTDOWN must not end the wait", reads)
	}
}

// T040 (FR-022): a guest that ignores the request must be reported as not stopped, so
// the caller forces it off rather than lying about the outcome.
func TestWaitForShutoffGivesUpAfterTheGracePeriod(t *testing.T) {
	const grace = 60 * time.Millisecond
	start := time.Now()
	stopped := waitForShutoff(context.Background(), grace, 5*time.Millisecond,
		func() (VMState, error) { return VMStateRunning, nil })
	elapsed := time.Since(start)

	if stopped {
		t.Error("a guest that never stopped was reported as stopped — this is the B12 divergence itself")
	}
	if elapsed < grace {
		t.Errorf("gave up after %v; the guest is entitled to the whole %v grace period", elapsed, grace)
	}
	if elapsed > 10*grace {
		t.Errorf("waited %v for a %v grace period; the bound is not being honoured", elapsed, grace)
	}
}

// A grace period of zero means "no opportunity for guest OS clean-up" — upstream's
// documented meaning for shutdown_timeout = 0. It must not poll at all.
func TestWaitForShutoffWithNoGraceDoesNotPoll(t *testing.T) {
	reads := 0
	stopped := waitForShutoff(context.Background(), 0, time.Millisecond,
		func() (VMState, error) {
			reads++
			return VMStateShutoff, nil
		})

	if stopped {
		t.Error("a zero grace period reported a clean stop; there was no opportunity for one")
	}
	if reads != 0 {
		t.Errorf("read the state %d times with no grace period; it should go straight to forcing", reads)
	}
}

// A domain that disappeared while shutting down IS off. Forcing a destroy on it would
// fail and turn a successful stop into an error.
func TestWaitForShutoffTreatsAnAbsentDomainAsStopped(t *testing.T) {
	if !waitForShutoff(context.Background(), time.Second, time.Millisecond,
		func() (VMState, error) { return VMStateNoState, ErrDomainNotFound }) {
		t.Error("an absent domain was reported as still running; a guest that is gone is not one to force off")
	}
}

// An unreadable state is not evidence the guest stopped. The wait must run its course
// and let the caller force the issue.
func TestWaitForShutoffDoesNotTrustAnUnreadableState(t *testing.T) {
	stopped := waitForShutoff(context.Background(), 30*time.Millisecond, 5*time.Millisecond,
		func() (VMState, error) { return VMStateNoState, errors.New("libvirt unreachable") })

	if stopped {
		t.Error("an unreadable state was taken as proof of a clean stop")
	}
}

// A cancelled request must not hold the grace period open.
func TestWaitForShutoffHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if waitForShutoff(ctx, time.Minute, time.Millisecond,
		func() (VMState, error) { return VMStateRunning, nil }) {
		t.Error("a cancelled wait reported a clean stop")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a cancelled wait took %v; it must return promptly", elapsed)
	}
}

// RunStateOf is the one place that decides what a libvirt state MEANS, and the
// distinction that matters is three-way: "up", "down", and "the hypervisor did not
// tell us". Collapsing the third into either of the first two is what B12 did —
// treating an unreadable domain as stopped writes a lie, treating it as running leaves
// one in place.
func TestRunStateOf(t *testing.T) {
	for _, tc := range []struct {
		state VMState
		want  RunState
	}{
		{VMStateRunning, RunStateUp},
		{VMStateBlocked, RunStateUp},
		{VMStatePaused, RunStateUp},
		{VMStateShutdown, RunStateUp}, // still on its way down, so still up
		{VMStatePMSuspended, RunStateUp},
		{VMStateShutoff, RunStateDown},
		{VMStateCrashed, RunStateDown},
		{VMStateNoState, RunStateUnknown},
		{VMState("something libvirt added later"), RunStateUnknown},
	} {
		if got := RunStateOf(tc.state); got != tc.want {
			t.Errorf("RunStateOf(%q) = %v; want %v", tc.state, got, tc.want)
		}
	}
}

// getVMStateReal returned libvirt's own enum as the power state, and the two do not
// agree: libvirt's SHUTOFF is 5 while the power_state a client reads for a stopped
// server is 4 (upstream nova's InstancePowerState indices leave 2 and 5 unused).
// Nothing consumed the value, so it never showed — until the reconciler started to.
func TestNovaPowerStateMapsLibvirtsEnum(t *testing.T) {
	for _, tc := range []struct {
		libvirtState int32
		want         int
		why          string
	}{
		{0, 0, "NOSTATE"},
		{1, 1, "RUNNING"},
		{2, 1, "BLOCKED is running, just not scheduled"},
		{3, 3, "PAUSED"},
		{4, 4, "SHUTDOWN"},
		{5, 4, "libvirt SHUTOFF is nova SHUTDOWN — 5 is not a nova power state"},
		{6, 6, "CRASHED"},
		{7, 7, "PMSUSPENDED is nova SUSPENDED"},
		{99, 0, "an unknown state is no state, not a made-up one"},
	} {
		if got := novaPowerState(tc.libvirtState); got != tc.want {
			t.Errorf("novaPowerState(%d) = %d; want %d (%s)", tc.libvirtState, got, tc.want, tc.why)
		}
	}
}
