package nova

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// SPEC-025 US7 (cloud-storage#547 B7): action history outlives the server, and its
// growth is bounded by age.
//
// Deleting a server has never cascaded to instance_actions, so every delete orphaned
// its rows — 850 were observed after a single multi-tenancy run. A cascade is the wrong
// fix: upstream retains action records for deleted instances, so this is a retention
// question, not a missing foreign key. The rows stay; they are reclaimed once they are
// old enough.

// seedAction inserts an action record with a chosen age, for an instance that may or may
// not exist.
func seedAction(t *testing.T, instanceID, projectID, action string, age time.Duration) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message)
		 VALUES ($1, $2, $3, $4, $5, $6, 'seeded') RETURNING id::text`,
		instanceID, action, "req-"+action+"-"+instanceID[:8], seededUserID, projectID,
		time.Now().Add(-age),
	).Scan(&id); err != nil {
		t.Fatalf("seeding action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instance_actions WHERE id = $1`, id)
	})
	return id
}

func actionExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM instance_actions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting action: %v", err)
	}
	return n == 1
}

// T082 (FR-042, FR-042a, FR-042c): old records of a DELETED server are reclaimed; a live
// server's records are kept whatever their age; deletion itself keeps the history.
func TestActionHistoryRetention(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	// A live server with a very old action record.
	liveID, _ := seedLifecycleInstance(t, mgr, "spec025-retain-live")
	liveOld := seedAction(t, liveID, seededProjectID, "create", 400*24*time.Hour)

	// A deleted server: rows exist, the instance does not. This is what a delete
	// leaves behind, by design.
	const goneID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeee1"
	goneOld := seedAction(t, goneID, seededProjectID, "stop", 400*24*time.Hour)
	goneNew := seedAction(t, goneID, seededProjectID, "start", time.Hour)

	NewStateReconciler(svc, time.Minute).
		WithActionRetention(90 * 24 * time.Hour).
		reclaimActionHistory(context.Background())

	if !actionExists(t, liveOld) {
		t.Error("a LIVE server's old action record was reclaimed; the history of an existing server must be kept in full whatever its age (FR-042c)")
	}
	if actionExists(t, goneOld) {
		t.Error("an over-age record of a deleted server survived; growth is unbounded (FR-042a)")
	}
	if !actionExists(t, goneNew) {
		t.Error("a recent record of a deleted server was reclaimed; deletion must not delete the history (FR-042)")
	}
}

// Retention disabled must reclaim nothing — a caller that wants only state
// reconciliation must not silently start deleting audit records.
func TestActionHistoryRetentionDisabledReclaimsNothing(t *testing.T) {
	requireDB(t)
	svc, _, _ := newLifecycleService(t)

	const goneID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeee2"
	ancient := seedAction(t, goneID, seededProjectID, "stop", 1000*24*time.Hour)

	NewStateReconciler(svc, time.Minute).reclaimActionHistory(context.Background())

	if !actionExists(t, ancient) {
		t.Error("records were reclaimed with retention unset; that must be an explicit choice")
	}
}

// T090 (FR-042): every delete path leaves the history alone. Asserted rather than
// assumed — the point of the retention decision is that NO change was needed at these
// three sites, and a later "fix" adding a cascade would be caught here.
func TestDeletingAServerKeepsItsActionHistory(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)

	id, _ := seedLifecycleInstance(t, mgr, "spec025-retain-delete")
	recorded := seedAction(t, id, seededProjectID, "create", time.Minute)

	code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+id, "", lifecycleHdr())
	if code != http.StatusNoContent && code != http.StatusAccepted {
		t.Fatalf("delete = %d; want it to succeed. body: %s", code, body)
	}

	// The instance is gone...
	var instances int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM instances WHERE id = $1`, id).Scan(&instances); err != nil {
		t.Fatalf("counting instances: %v", err)
	}
	if instances != 0 {
		t.Fatalf("the server was not deleted")
	}

	// ...and its history is not.
	if !actionExists(t, recorded) {
		t.Error("deleting a server deleted its action history; upstream retains it, and so must Lightstack (FR-042)")
	}
}
