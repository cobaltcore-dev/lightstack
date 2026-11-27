package nova

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
)

// SPEC-033 T199: a server's delete whose guest the hypervisor cannot remove answers 500
// and keeps the server, so a second delete finishes it; forceDelete carries on, the leftover domain being
// hygiene rather than correctness, and the server goes.
func TestDeleteServerWhenTheHypervisorFails(t *testing.T) {
	requireDB(t)
	svc, r, _ := newLifecycleService(t)
	svc.vmManager = hypervisor.NewDisconnectedVMManager("")
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID}
	exists := func(t *testing.T, id string) bool {
		t.Helper()
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM instances WHERE id = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	seed := func(t *testing.T, name string) string {
		var id string
		if err := testPool.QueryRow(context.Background(),
			`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state, libvirt_domain_id)
			 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1, $4) RETURNING id::text`,
			name, seededProjectID, seededUserID, "8f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b").Scan(&id); err != nil {
			t.Fatalf("seed instance: %v", err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id) })
		return id
	}

	deleted := seed(t, "spec033-src27-delete")
	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+deleted, "", hdr); code != http.StatusInternalServerError {
		t.Errorf("delete with the hypervisor down: code=%d body=%s; want 500", code, body)
	}
	if !exists(t, deleted) {
		t.Error("the server went though its guest could not be removed; want it kept for a second delete")
	}

	forced := seed(t, "spec033-src27-force")
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+forced+"/action", `{"forceDelete":null}`, hdr); code/100 != 2 {
		t.Errorf("forceDelete with the hypervisor down: code=%d body=%s; want success", code, body)
	}
	if exists(t, forced) {
		t.Error("forceDelete kept the server; want it gone, the leftover domain hygiene")
	}
}
