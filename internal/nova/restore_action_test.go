package nova

import (
	"context"
	"net/http"
	"testing"
)

// SPEC-025-followup US5: `restore` answers as upstream does at its default reclaim interval
// (FR-035 – FR-037).
//
// Upstream routes `restore` at every configuration (nova/api/openstack/compute/deferred_delete.py,
// expected errors 403/404/409). With reclaim_instance_interval at its default of 0, deletion is
// immediate: a deleted server is 404, and every server that exists is live, so it is refused with
// 409 — the action accepts only a soft-deleted one. This product never soft-deletes, which is that
// default. Upstream's 400 "There is no such action" is for a name it does not route, and restore
// is not one.

// seedRestoreTarget creates one server in the given status and removes it on cleanup.
func seedRestoreTarget(t *testing.T, status string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('restore-target', $1, $2, (SELECT id FROM flavors WHERE name = 'm1.small'), $3)
		 RETURNING id::text`, seededProjectID, seededUserID, status).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id) })
	return id
}

// TestRestoreAnswersAsUpstream is T091: a live server is refused with 409 and a deleted one is
// 404 — never the unknown-action 400, and never 202.
func TestRestoreAnswersAsUpstream(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	for _, status := range []string{"ACTIVE", "SHUTOFF"} {
		id := seedRestoreTarget(t, status)
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"restore": null}`, hdr)
		if code != http.StatusConflict {
			t.Errorf("restore on a %s server: code=%d body=%s; want 409, upstream's answer for a server that is not soft-deleted",
				status, code, body)
		}
	}

	// A deleted server is gone at once, so restore reaches the dispatcher's 404 — the answer the
	// gophercloud contract test expects after a delete.
	id := seedRestoreTarget(t, "ACTIVE")
	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+id, "", hdr); code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s; want 204", code, body)
	}
	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"restore": null}`, hdr)
	if code != http.StatusNotFound {
		t.Errorf("restore on a deleted server: code=%d body=%s; want 404, as upstream at the default reclaim interval", code, body)
	}
}

// TestDeletionSemanticsUnchanged is T093: keeping `restore` routed does not introduce soft
// delete. A deleted server is gone, immediately, as before.
func TestDeletionSemanticsUnchanged(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := seedRestoreTarget(t, "ACTIVE")

	r := newNovaTestRouter()
	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID)); code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s; want 204", code, body)
	}

	var n int
	if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM instances WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("the row survives deletion (%d rows); deletion must stay immediate — no soft-delete state was introduced", n)
	}
}
