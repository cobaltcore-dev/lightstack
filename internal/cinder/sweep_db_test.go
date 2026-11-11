package cinder

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// At start, the creates a stopped owner on this node was running go to error (FR-033,
// research R13). Their backing is left in place, as upstream's cleanup leaves it: deleting
// the errored volume or snapshot reclaims it. A live owner's create — a second process
// sharing the node name — is left alone, as are another node's rows, a
// pre-reservation-owner (NULL) row, and anything no longer creating. The stopped owner is
// then forgotten.
func TestSweepInterruptedReservations(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	node := "sweep-" + uuid.NewString()[:8]

	// Two owners on this node — one stopped (heartbeat long stale), one still heartbeating —
	// and a live owner on another node, whose rows are that node's to sweep.
	stopped, alive := uuid.NewString(), uuid.NewString()
	registerOwner(t, stopped, node, time.Now().Add(-time.Hour))
	registerOwner(t, alive, node, time.Now())
	otherOwner := uuid.NewString()
	registerOwner(t, otherOwner, node+"-other", time.Now())

	seed := func(status, reservedBy string, reservedOwner any) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, reserved_by, reserved_owner)
			 VALUES ($1, 'spec028-sweep', $2, $3, 1, $4, $5, $6)`,
			id, project, seededUserID, status, reservedBy, reservedOwner); err != nil {
			t.Fatalf("seed a volume: %v", err)
		}
		return id
	}
	own := seed("creating", node, stopped) // set to error
	live := seed("creating", node, alive)  // left alone: its owner is heartbeating
	otherNode := seed("creating", node+"-other", otherOwner)
	legacy := seed("creating", node, nil) // pre-migration: NULL owner, left alone
	ownDone := seed("available", node, stopped)
	snapshot := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, reserved_by, reserved_owner)
		 VALUES ($1, 'spec028-sweep', $2, $3, 1, 'creating', $4, $5)`, snapshot, ownDone, project, node, stopped); err != nil {
		t.Fatalf("seed a snapshot: %v", err)
	}

	fake := &fakeBacking{}
	svc, _ := backingRouter(t, fake, node)
	svc.SweepInterruptedReservations(ctx)

	for id, want := range map[string]string{
		own: "error", live: "creating",
		otherNode: "creating", legacy: "creating", ownDone: "available",
	} {
		if status, _, _, _, _ := reservedVolume(t, id); status != want {
			t.Errorf("volume %s is %q after the sweep; want %q", id, status, want)
		}
	}
	var snapStatus string
	if err := testPool.QueryRow(ctx, `SELECT status FROM snapshots WHERE id = $1`, snapshot).Scan(&snapStatus); err != nil || snapStatus != "error" {
		t.Errorf("snapshot is %q (%v) after the sweep; want error", snapStatus, err)
	}
	if deleted, deletedSnapshots := fake.calls(&fake.deleted), fake.calls(&fake.deletedSnapshots); len(deleted)+len(deletedSnapshots) > 0 {
		t.Errorf("the sweep deleted backings %v and snapshot backings %v; want none — deleting the errored resource reclaims it",
			deleted, deletedSnapshots)
	}
	// The stopped owner is forgotten; the live one and the other node's are kept.
	if ownerExists(t, stopped) {
		t.Errorf("the stopped owner %s was not pruned", stopped)
	}
	if !ownerExists(t, alive) || !ownerExists(t, otherOwner) {
		t.Errorf("a live or other-node owner was pruned")
	}
}

// TestSweepReclaimsAnOwnerThatGoesStaleBetweenPasses covers a restart within the stale
// window: the first pass leaves a crashed owner's create alone because its heartbeat has not
// yet expired, and a later pass sets it to error once it does. This is why the sweep re-runs
// (RunReservationSweeper), not only at startup.
func TestSweepReclaimsAnOwnerThatGoesStaleBetweenPasses(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	node := "stale-" + uuid.NewString()[:8]
	owner := uuid.NewString()
	registerOwner(t, owner, node, time.Now()) // fresh: looks alive

	vol := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, reserved_by, reserved_owner)
		 VALUES ($1, 'spec028-stale', $2, $3, 1, 'creating', $4, $5)`, vol, project, seededUserID, node, owner); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := &fakeBacking{}
	svc, _ := backingRouter(t, fake, node)

	// First pass: the owner's heartbeat is fresh, so the create is left alone.
	svc.SweepInterruptedReservations(ctx)
	if status, _, _, _, _ := reservedVolume(t, vol); status != "creating" {
		t.Fatalf("volume is %q after the first pass; want it left creating (owner still live)", status)
	}
	if slices.Contains(fake.calls(&fake.deleted), vol) {
		t.Fatalf("the live owner's backing was reclaimed on the first pass")
	}

	// The owner's heartbeat expires — it crashed and sends no more.
	if _, err := testPool.Exec(ctx,
		`UPDATE reservation_owners SET last_heartbeat = $1 WHERE owner_id = $2`,
		time.Now().Add(-2*reservationOwnerStaleAfter), owner); err != nil {
		t.Fatalf("expire the heartbeat: %v", err)
	}

	// A later pass sets it to error, and leaves its backing for the delete of the errored volume.
	svc.SweepInterruptedReservations(ctx)
	if status, _, _, _, _ := reservedVolume(t, vol); status != "error" {
		t.Errorf("volume is %q after the owner went stale; want error", status)
	}
	if slices.Contains(fake.calls(&fake.deleted), vol) {
		t.Errorf("the later pass deleted the stale owner's backing; want it left for the delete of the errored volume")
	}
}

// TestSweepLeavesALiveOwnersCreateAlone covers two processes overlapping on one node: while
// one process is creating a volume, a second process on the same node starts and sweeps.
// The create is live — its owner is heartbeating — so the sweep must leave it creating,
// and the create still succeeds.
func TestSweepLeavesALiveOwnersCreateAlone(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	node := "overlap-" + uuid.NewString()[:8]

	fake := &fakeBacking{}
	first, r := backingRouter(t, fake, node)
	if err := first.RegisterReservationOwner(context.Background()); err != nil {
		t.Fatalf("register the first owner: %v", err)
	}
	t.Cleanup(func() { deleteOwner(first.OwnerID()) })

	// A second process sharing the node, mid-startup: its sweep runs while the first's
	// create is under way (the row is creating, reserved by the first's live owner).
	secondFake := &fakeBacking{}
	second, _ := backingRouter(t, secondFake, node)
	var sweptStatus, createdID string
	fake.during = func(id string) {
		createdID = id
		second.SweepInterruptedReservations(context.Background())
		sweptStatus, _, _, _, _ = reservedVolume(t, id)
	}

	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-overlap", ""), cinderAuthHdr(project, seededUserID))
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %s; want 202", code, body)
	}
	if sweptStatus != "creating" {
		t.Errorf("while the create was live the second process's sweep moved it to %q; want it left creating", sweptStatus)
	}
	if slices.Contains(secondFake.calls(&secondFake.deleted), createdID) {
		t.Errorf("the second process's sweep reclaimed the first's live backing %s", createdID)
	}
	if status, _, _, _, _ := reservedVolume(t, createdID); status != "available" {
		t.Errorf("the create finished %q; want available (the sweep must not have disturbed it)", status)
	}
}

// TestSweepKeepsALapsedSiblingsBacking covers a live process whose heartbeat lapses while it
// creates a volume: a second process on the same node cannot tell it from a stopped one, so
// its sweep sets the create to error — but, as upstream's cleanup does, it leaves the backing
// alone. The create fails and keeps its backing for the errored row, and deleting that volume
// reclaims it.
func TestSweepKeepsALapsedSiblingsBacking(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	project := cinderProject(t, nil)
	node := "lapsed-" + uuid.NewString()[:8]
	fake := &fakeBacking{}
	first, r := backingRouter(t, fake, node)
	secondFake := &fakeBacking{}
	second, _ := backingRouter(t, secondFake, node)
	for _, svc := range []*Service{first, second} {
		if err := svc.RegisterReservationOwner(ctx); err != nil {
			t.Fatalf("register an owner: %v", err)
		}
		t.Cleanup(func() { deleteOwner(svc.OwnerID()) })
	}

	var created, sweptStatus string
	fake.during = func(id string) {
		created = id
		if _, err := testPool.Exec(ctx,
			`UPDATE reservation_owners SET last_heartbeat = $1 WHERE owner_id = $2`,
			time.Now().Add(-2*reservationOwnerStaleAfter), first.OwnerID()); err != nil {
			t.Errorf("expire the heartbeat: %v", err)
		}
		second.SweepInterruptedReservations(ctx)
		sweptStatus, _, _, _, _ = reservedVolume(t, id)
	}
	hdr := cinderAuthHdr(project, seededUserID)
	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-lapsed", ""), hdr)
	if code < 500 {
		t.Errorf("create = %d %s; want a failure (its row went to error while it was built)", code, body)
	}
	if sweptStatus != "error" {
		t.Errorf("the second process's sweep left the lapsed create %q; want error", sweptStatus)
	}
	if slices.Contains(secondFake.calls(&secondFake.deleted), created) || slices.Contains(fake.calls(&fake.deleted), created) {
		t.Fatalf("the backing of the lapsed create %s was deleted; want it kept for the errored row", created)
	}
	if !slices.Contains(fake.calls(&fake.created), created) {
		t.Errorf("the backing of the lapsed create %s was not built", created)
	}

	// Deleting the errored volume reclaims the backing.
	if code, body := doReq(r, http.MethodDelete, "/v3/volumes/"+created, "", hdr); code/100 != 2 {
		t.Fatalf("delete the errored volume = %d %s; want success", code, body)
	}
	if !slices.Contains(fake.calls(&fake.deleted), created) {
		t.Errorf("deleting the errored volume did not reclaim its backing")
	}
}

// TestSweepNeverReclaimsItsOwnCreates covers a missed heartbeat: while this process creates a
// volume or a snapshot, its own heartbeat lapses past the stale window — a database outage, a
// paused process, a clock step — and its own periodic sweep runs. The process running the
// sweep is alive, so its create is left creating and finishes with its backing, and its
// registration survives the prune.
func TestSweepNeverReclaimsItsOwnCreates(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct {
		kind, table, path string
		body              func(t *testing.T, project string) string
	}{
		{"volume", "volumes", "/v3/volumes", func(_ *testing.T, _ string) string {
			return volumeCreateBody("spec028-self", "")
		}},
		{"snapshot", "snapshots", "/v3/snapshots", func(t *testing.T, project string) string {
			return fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"spec028-self"}}`, seedAvailableVolume(t, project))
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			ctx := context.Background()
			project := cinderProject(t, nil)
			fake := &fakeBacking{}
			svc, r := backingRouter(t, fake, "self-"+uuid.NewString()[:8])
			if err := svc.RegisterReservationOwner(ctx); err != nil {
				t.Fatalf("register the owner: %v", err)
			}
			t.Cleanup(func() { deleteOwner(svc.OwnerID()) })

			var created, sweptStatus string
			fake.during = func(id string) {
				created = id
				if _, err := testPool.Exec(ctx,
					`UPDATE reservation_owners SET last_heartbeat = $1 WHERE owner_id = $2`,
					time.Now().Add(-2*reservationOwnerStaleAfter), svc.OwnerID()); err != nil {
					t.Errorf("expire the heartbeat: %v", err)
				}
				svc.SweepInterruptedReservations(ctx)
				if err := testPool.QueryRow(ctx, `SELECT status FROM `+tc.table+` WHERE id = $1`, id).Scan(&sweptStatus); err != nil {
					t.Errorf("read the %s after the sweep: %v", tc.kind, err)
				}
			}

			code, body := doReq(r, http.MethodPost, tc.path, tc.body(t, project), cinderAuthHdr(project, seededUserID))
			if code != http.StatusAccepted {
				t.Errorf("%s create = %d %s; want 202", tc.kind, code, body)
			}
			if sweptStatus != "creating" {
				t.Errorf("this process's own sweep moved its live %s to %q; want it left creating", tc.kind, sweptStatus)
			}
			if slices.Contains(fake.calls(&fake.deleted), created) || slices.Contains(fake.calls(&fake.deletedSnapshots), created) {
				t.Errorf("this process's own sweep reclaimed the backing of its live %s %s", tc.kind, created)
			}
			var final string
			if err := testPool.QueryRow(ctx, `SELECT status FROM `+tc.table+` WHERE id = $1`, created).Scan(&final); err != nil || final != "available" {
				t.Errorf("the %s finished %q (%v); want available", tc.kind, final, err)
			}
			if !ownerExists(t, svc.OwnerID()) {
				t.Errorf("this process's own sweep pruned its registration")
			}
		})
	}
}

// TestHeartbeatRestoresADeletedRegistration covers recovery from a missed heartbeat: this
// process's heartbeat lapses, a second process on the same node takes it for stopped and
// deletes its registration, and this process's next beat registers it again — so a create it
// starts afterwards is protected from the second process's sweep once more.
func TestHeartbeatRestoresADeletedRegistration(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	project := cinderProject(t, nil)
	node := "rereg-" + uuid.NewString()[:8]
	fake := &fakeBacking{}
	first, r := backingRouter(t, fake, node)
	second, _ := backingRouter(t, &fakeBacking{}, node)
	for _, svc := range []*Service{first, second} {
		if err := svc.RegisterReservationOwner(ctx); err != nil {
			t.Fatalf("register an owner: %v", err)
		}
		t.Cleanup(func() { deleteOwner(svc.OwnerID()) })
	}

	// The first process's heartbeat lapses, and the second's sweep deletes its registration.
	if _, err := testPool.Exec(ctx,
		`UPDATE reservation_owners SET last_heartbeat = $1 WHERE owner_id = $2`,
		time.Now().Add(-2*reservationOwnerStaleAfter), first.OwnerID()); err != nil {
		t.Fatalf("expire the heartbeat: %v", err)
	}
	second.SweepInterruptedReservations(ctx)
	if ownerExists(t, first.OwnerID()) {
		t.Fatalf("the second process's sweep kept the stale registration; this test needs it deleted")
	}

	// The first process's next beat registers it again.
	first.heartbeat(ctx)
	if !ownerExists(t, first.OwnerID()) {
		t.Fatalf("the heartbeat did not restore the deleted registration")
	}

	// A create the first process starts afterwards is protected again: the second process's
	// sweep, running while it is under way, leaves it alone.
	var created, sweptStatus string
	fake.during = func(id string) {
		created = id
		second.SweepInterruptedReservations(ctx)
		sweptStatus, _, _, _, _ = reservedVolume(t, id)
	}
	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-rereg", ""), cinderAuthHdr(project, seededUserID))
	if code != http.StatusAccepted {
		t.Errorf("create after the re-registration = %d %s; want 202", code, body)
	}
	if sweptStatus != "creating" {
		t.Errorf("the second process's sweep moved the create to %q; want it left creating", sweptStatus)
	}
	if status, _, _, _, _ := reservedVolume(t, created); status != "available" {
		t.Errorf("the create finished %q; want available", status)
	}
}
