package nova

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
)

// SPEC-025 US4 (cloud-storage#547 B12): an action whose end state already holds is a
// state problem, not a fault.
//
// Observed on a real-mode host by stopping and starting the same server ten times in a
// row: `os-stop` returned 202 while the guest was still running, and every subsequent
// `os-start` answered 500 "failed to perform os-start" — libvirt refuses to create a
// domain that is already active, and that error had nowhere to go but
// NewInternalServerError. Worse, the error path returned before persisting anything, so
// the record stayed SHUTOFF and every later start failed the same way. The divergence
// latched.
//
// Upstream's api-ref lists conflict(409) for both actions, with preconditions "the
// server status must be ACTIVE or ERROR" for stop and "must be SHUTOFF" for start.
// Lightstack checks those against the HYPERVISOR rather than the record, deliberately: a
// record that is already wrong must not be what decides whether a client is allowed to
// act, or the lie locks the server out of the very action that would clear it.

// T041 (FR-026a): start against a guest that is already up is 409, and the record is
// corrected on the way out.
func TestStartAgainstARunningGuestIsAConflictAndCorrectsTheRecord(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-start-conflict")

	// The exact divergence the host probe produced: the record says stopped, the
	// guest is running. Set directly rather than through os-stop, so the state under
	// test is the one being pinned and not a side effect of another fix.
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting the diverged record: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr())
	if code == http.StatusInternalServerError {
		t.Errorf("os-start against a running guest returned 500; libvirt refusing an already-active "+
			"domain is a state problem, not a fault. body: %s", body)
	}
	if code != http.StatusConflict {
		t.Errorf("os-start against a running guest = %d; want 409. body: %s", code, body)
	}

	// The anti-latch property. Refusing without correcting the record is what made the
	// host probe fail identically eight times running.
	if status, power := readInstanceState(t, id); status != "ACTIVE" || power != 1 {
		t.Errorf("after the refusal: status=%q power=%d; want ACTIVE/1 — a refusal that leaves the "+
			"record lying locks the server out of every future action", status, power)
	}
}

// T041 (FR-026a): stop against a guest that is already off is 409, not 500.
//
// The host probe hit this too, on the one iteration where the guest finished its ACPI
// shutdown after the record had already been written: `os-stop` then reached libvirt
// with a domain that was not running, and answered 500.
func TestStopAgainstAStoppedGuestIsAConflictAndCorrectsTheRecord(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-stop-conflict")

	// Guest off, record still claiming ACTIVE from seeding.
	if err := mgr.StopVM(t.Context(), domainID, 0); err != nil {
		t.Fatalf("stopping the stub guest: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-stop": null}`, lifecycleHdr())
	if code == http.StatusInternalServerError {
		t.Errorf("os-stop against a stopped guest returned 500. body: %s", body)
	}
	if code != http.StatusConflict {
		t.Errorf("os-stop against a stopped guest = %d; want 409. body: %s", code, body)
	}

	if status, power := readInstanceState(t, id); status != "SHUTOFF" || power != 4 {
		t.Errorf("after the refusal: status=%q power=%d; want SHUTOFF/4", status, power)
	}
}

// T041: the probe's loop, in miniature — the correction has to be visible THROUGH THE
// API, and the refusal has to leave the server usable.
//
// The first assertion is what makes this test worth having: the two above read the row
// directly, so neither would notice a correction that never reaches a client. The second
// is a guard against a future regression rather than a fix for the observed one — with
// the precondition read against the hypervisor, a stale record no longer blocks the
// opposite action, and this pins that. Note it does NOT detect a missing correction on
// its own: removing the persist leaves this half passing, which is why the read is here.
func TestAConflictDoesNotLatchTheDivergence(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-no-latch")

	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting the diverged record: %v", err)
	}

	// Refused, because the guest is already up.
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
		`{"os-start": null}`, lifecycleHdr()); code != http.StatusConflict {
		t.Fatalf("os-start = %d; want 409. body: %s", code, body)
	}

	// A client that reads the server after the refusal must be told what it is actually
	// doing. Until it is, the client has been refused a start on a server the API still
	// describes as stopped — which reads as a product that contradicts itself, and is
	// how the host probe looked from outside.
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", lifecycleHdr())
	if code != http.StatusOK {
		t.Fatalf("server read = %d; want 200. body: %s", code, body)
	}
	if !strings.Contains(body, `"status":"ACTIVE"`) {
		t.Errorf("after the refusal the API still describes the server as stopped: %s\n"+
			"a 409 saying \"already running\" over a server reported SHUTOFF is a contradiction", body)
	}

	// Accepted, because the refusal told the truth about what the server is doing.
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
		`{"os-stop": null}`, lifecycleHdr()); code != http.StatusAccepted {
		t.Fatalf("os-stop after the refusal = %d; want 202. body: %s", code, body)
	}
	if status, power := readInstanceState(t, id); status != "SHUTOFF" || power != 4 {
		t.Errorf("after the stop: status=%q power=%d; want SHUTOFF/4", status, power)
	}
}

// A hypervisor that cannot be read must not become a refusal. "I could not tell" is
// not "the end state already holds", and turning it into a 409 would refuse actions
// during a blip — the mirror of the bug being fixed.
func TestAnUnreadableGuestStateDoesNotBecomeAConflict(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-unreadable")

	// An absent guest is the one unreadable case the stub can produce. Stop and
	// reboot must still reach their existing absent-guest answer (409 naming the
	// missing guest) rather than a precondition refusal, and start must still reach
	// the self-heal — all of which is only true if an unreadable state falls through
	// to the action instead of short-circuiting.
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-stop": null}`, lifecycleHdr())
	if code != http.StatusConflict {
		t.Errorf("os-stop with an absent guest = %d; want the existing 409. body: %s", code, body)
	}
	if !strings.Contains(body, "no guest") {
		t.Errorf("os-stop with an absent guest answered %q; it should still name the missing guest "+
			"rather than report a precondition failure", body)
	}
}

// The grace period must fit inside the HTTP write deadline even when it is set on the
// Service directly, bypassing LoadConfig's clamp — which every test does, and which a
// future caller might.
//
// The failure this guards is not a wrong number but a lost answer: a handler that runs
// past the deadline has its connection closed by net/http with no response written, so
// a stop that worked reports nothing at all and the client retries into a 409.
func TestGuestShutdownGraceCannotOutliveTheWriteDeadline(t *testing.T) {
	svc := NewService(testPool, "", "real", nil)
	t.Cleanup(svc.Shutdown)

	t.Run("unset uses the default", func(t *testing.T) {
		if got := svc.guestShutdownGrace(); got != common.DefaultShutdownTimeout {
			t.Errorf("guestShutdownGrace() = %v; want the default %v", got, common.DefaultShutdownTimeout)
		}
	})

	t.Run("a value set past the ceiling is clamped", func(t *testing.T) {
		svc.SetShutdownTimeout(10 * time.Minute)
		if got := svc.guestShutdownGrace(); got != common.MaxShutdownTimeout {
			t.Errorf("guestShutdownGrace() = %v; want it clamped to %v", got, common.MaxShutdownTimeout)
		}
	})

	t.Run("the resulting request deadline fits the write deadline", func(t *testing.T) {
		for _, configured := range []time.Duration{0, time.Second, 30 * time.Second, time.Hour} {
			svc.SetShutdownTimeout(configured)
			deadline := svc.guestShutdownGrace() + common.LifecycleResponseSlack
			if deadline > common.HTTPWriteTimeout {
				t.Errorf("shutdown_timeout %v yields a %v request deadline, past the %v write "+
					"deadline; the response would be lost", configured, deadline, common.HTTPWriteTimeout)
			}
		}
	})
}

// TestConflictCorrectionYieldsToANewerWrite: a refused stop or start corrects the record from
// what it observed — but only if nothing has written the row since the action read it. A
// concurrent stop that finished in between recorded a newer truth, and the stale observation
// ("running") must not overwrite it. Upstream gets this by serialising lifecycle actions per
// instance.
func TestConflictCorrectionYieldsToANewerWrite(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	ctx := context.Background()

	revision := func(id string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := testPool.QueryRow(ctx, `SELECT updated_at FROM instances WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("reading updated_at: %v", err)
		}
		return at
	}

	raced, _ := seedLifecycleInstance(t, mgr, "spec025-conflict-raced")
	readAt := revision(raced)
	if _, err := testPool.Exec(ctx,
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4, updated_at = $2 WHERE id = $1`,
		raced, readAt.Add(time.Second)); err != nil {
		t.Fatalf("concurrent stop: %v", err)
	}
	if svc.persistObservedRunState(ctx, raced, seededProjectID, "ACTIVE", 1, readAt) {
		t.Error("a correction formed before a newer write was applied")
	}
	if status, power := readInstanceState(t, raced); status != "SHUTOFF" || power != 4 {
		t.Errorf("record after the stale correction = %s/%d; want the newer SHUTOFF/4 kept", status, power)
	}

	// With nothing in between it applies — else the guard could refuse everything.
	quiet, _ := seedLifecycleInstance(t, mgr, "spec025-conflict-quiet")
	if !svc.persistObservedRunState(ctx, quiet, seededProjectID, "SHUTOFF", 4, revision(quiet)) {
		t.Error("a correction with no concurrent write was not applied")
	}
	if status, power := readInstanceState(t, quiet); status != "SHUTOFF" || power != 4 {
		t.Errorf("record after the correction = %s/%d; want SHUTOFF/4", status, power)
	}

	// A row that never had an updated_at is still correctable.
	unset, _ := seedLifecycleInstance(t, mgr, "spec025-conflict-null-updated")
	if _, err := testPool.Exec(ctx, `UPDATE instances SET updated_at = NULL WHERE id = $1`, unset); err != nil {
		t.Fatalf("clear updated_at: %v", err)
	}
	if !svc.persistObservedRunState(ctx, unset, seededProjectID, "SHUTOFF", 4, revision(unset)) {
		t.Error("a correction on a row with no updated_at was not applied")
	}
}
