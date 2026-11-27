package nova

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T209: a server's deletion locks its volume attachments before it touches their
// volumes, the order a volume's detach takes them in — the attachment it deletes, then the volume — so the
// two cannot deadlock on PostgreSQL. With an attachment locked by another transaction, the deletion waits
// holding no volume, and goes through when the lock is released. PostgreSQL only.
func TestServerDeleteLocksAttachmentsBeforeVolumes(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t)
	ctx := context.Background()
	r, _, _ := newNovaWithNetwork(t)
	tenant := tenantProject(t)
	server, _ := spec033Server(t, r, tenant)
	volume, attachment := uuid.NewString(), uuid.NewString()
	mustExecNova(t, `INSERT INTO volumes (id, name, project_id, size_gb, status, attached_to_instance_id)
		VALUES ($1, 'spec033-src32', $2, 1, 'in-use', $3)`, volume, tenant, server)
	t.Cleanup(func() { mustExecNova(t, "DELETE FROM volumes WHERE id = $1", volume) })
	mustExecNova(t, `INSERT INTO volume_attachments (id, volume_id, instance_id, device, delete_on_termination, attached_at)
		VALUES ($1, $2, $3, '/dev/vdb', false, now())`, attachment, volume, server)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op once committed
	holder := dbtest.HolderPID(t, tx)
	if _, err := tx.Exec(ctx, "SELECT id FROM volume_attachments WHERE id = $1 FOR UPDATE", attachment); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := doReq(r, http.MethodDelete, "/v2.1/servers/"+server, "", tenantHdr(tenant))
		done <- code
	}()
	dbtest.WaitUntilBlocked(t, testPool, holder)
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	//sqlcheck:postgres-only
	_, probeErr := probe.Exec(ctx, "SELECT id FROM volumes WHERE id = $1 FOR UPDATE NOWAIT", volume)
	_ = probe.Rollback(ctx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code/100 != 2 {
		t.Errorf("the deletion once the lock was released: code=%d; want success", code)
	}
	if probeErr != nil {
		t.Errorf("the volume was locked while the deletion waited for its attachment (%v): the detach's opposite order deadlocks", probeErr)
	}
}
