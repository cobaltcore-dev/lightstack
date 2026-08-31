package hypervisor

import (
	"context"
	"errors"
	"time"
)

// RunState is what the hypervisor says about whether a guest is up — with "it gave no
// usable answer" kept distinct from "it said no".
//
// Three values rather than a bool, because collapsing the third is exactly how
// cloud-storage#547 B12 went wrong in both directions: treating an unreadable domain as
// stopped writes a lie into the record, and treating it as running leaves one there.
// Every caller has to choose what to do about RunStateUnknown, which is the point.
type RunState int

const (
	// RunStateUnknown means the hypervisor could not be asked, or answered with a
	// state that carries no information (libvirt's NOSTATE). Never grounds for
	// changing a record.
	RunStateUnknown RunState = iota
	// RunStateUp means the guest exists and is not off — running, blocked, paused,
	// suspended, or on its way down. A stop has work to do; a start does not.
	RunStateUp
	// RunStateDown means the guest exists and is off. A start has work to do; a stop
	// does not.
	RunStateDown
)

// String makes a RunState readable in a log line.
func (r RunState) String() string {
	switch r {
	case RunStateUp:
		return "up"
	case RunStateDown:
		return "down"
	default:
		return "unknown"
	}
}

// RunStateOf is the single place that decides what a libvirt domain state MEANS.
//
// SHUTDOWN is deliberately Up: libvirt's VIR_DOMAIN_SHUTDOWN is "being shut down", not
// "shut down", and reading it as terminal is how a forced power-off lands on a guest
// that is still flushing its disks.
func RunStateOf(state VMState) RunState {
	switch state {
	case VMStateRunning, VMStateBlocked, VMStatePaused, VMStateShutdown, VMStatePMSuspended:
		return RunStateUp
	case VMStateShutoff, VMStateCrashed:
		return RunStateDown
	default:
		return RunStateUnknown
	}
}

// GuestRunState reports whether the hypervisor's guest for vmUUID is up.
//
// Wraps GetVMState because that returns libvirt's vocabulary and every caller here
// wants the decision, not the enum — and a caller that maps the states itself is a
// caller that can disagree with RunStateOf. Returns ErrDomainNotFound unwrapped-by-Is
// for an absent guest: "there is no guest" is not "the guest is off", and the two have
// different answers (a self-heal versus a refusal).
func (m *VMManager) GuestRunState(ctx context.Context, vmUUID string) (RunState, error) {
	state, _, err := m.GetVMState(ctx, vmUUID)
	if err != nil {
		return RunStateUnknown, err
	}
	return RunStateOf(VMState(state)), nil
}

// shutoffPollInterval is how often a pending shutdown is re-checked. Short enough that
// a guest which shuts down quickly is not made to wait for a tick, long enough that a
// minute-long grace period is ~120 round trips rather than thousands.
const shutoffPollInterval = 500 * time.Millisecond

// waitForShutoff polls readState until the guest is no longer up or the grace period
// expires, reporting whether it stopped of its own accord.
//
// This is the policy half of stopVMReal, split out because the other half needs a
// libvirt connection and this needs to be tested. false means "force it": the caller
// must not report a clean stop it did not observe, which is the whole of B12.
func waitForShutoff(ctx context.Context, grace, poll time.Duration, readState func() (VMState, error)) bool {
	if grace <= 0 {
		// No opportunity for guest OS clean-up — upstream's documented meaning for
		// shutdown_timeout = 0. Do not even look.
		return false
	}

	deadline := time.Now().Add(grace)
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}

		state, err := readState()
		switch {
		case errors.Is(err, ErrDomainNotFound):
			// The domain went away while shutting down. It is off, and there is
			// nothing left to force off.
			return true
		case err == nil && RunStateOf(state) == RunStateDown:
			return true
		}
		// Anything else — still up, or unreadable — is not evidence of a clean stop.

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer.Reset(min(poll, remaining))
	}
}

// novaPowerState maps a libvirt domain state onto the power_state a client reads.
//
// The two enums are NOT the same, which is the bug this replaces: libvirt's SHUTOFF is
// 5, while upstream nova's InstancePowerState leaves 2 and 5 unused and reports a
// stopped server as 4. getVMStateReal returned libvirt's number directly, so a stopped
// guest would have been published as power_state 5 — a value no client has a name for.
// Nothing consumed it, so it never surfaced.
func novaPowerState(libvirtState int32) int {
	switch libvirtState {
	case 1, 2: // RUNNING, BLOCKED — blocked is running, just not scheduled
		return 1
	case 3: // PAUSED
		return 3
	case 4, 5: // SHUTDOWN, SHUTOFF — both are nova's SHUTDOWN
		return 4
	case 6: // CRASHED
		return 6
	case 7: // PMSUSPENDED — nova's SUSPENDED
		return 7
	default: // NOSTATE, and anything libvirt adds later
		return 0
	}
}
