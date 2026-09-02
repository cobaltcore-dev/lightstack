package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// A volume transfer holds the volume in upstream's awaiting-transfer state from create until it is
// accepted or cancelled (cinder/db/api.py transfer_create, transfer_accept, transfer_destroy).
//
// Nothing held it before: creating a transfer left the volume available, accept moved it with an
// unconditional UPDATE, and nothing re-checked that the sender still owned it. So a sender could
// create two transfers, hand one over, and take the volume back with the other — 202 throughout —
// and two accepts of one transfer into different projects both succeeded, the last one keeping it.

const transferAcceptorUser = "00000000-0000-0000-0000-0000000c0c0d"

// ensureTransferAcceptor creates a user for the accepting side, so the test can tell the accepting
// user from the sending one.
func ensureTransferAcceptor(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO users (id, name, password_hash, enabled, domain_id)
		 VALUES ($1, 'spec025-transfer-acceptor', 'x', true, (SELECT id FROM domains WHERE name = 'Default'))
		 ON CONFLICT (id) DO NOTHING`, transferAcceptorUser); err != nil {
		t.Fatalf("create the accepting user: %v", err)
	}
}

// markAwaitingTransfer puts a volume in the state CreateVolumeTransfer leaves it in, for a test that
// builds its pending transfer row by hand.
func markAwaitingTransfer(t *testing.T, volumeID string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(),
		"UPDATE volumes SET status = 'awaiting-transfer' WHERE id = $1", volumeID); err != nil {
		t.Fatalf("mark volume %s awaiting transfer: %v", volumeID, err)
	}
}

type transferRef struct{ id, authKey string }

func createTransfer(t *testing.T, r *gin.Engine, volumeID string) (int, transferRef, string) {
	t.Helper()
	code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer",
		fmt.Sprintf(`{"transfer":{"volume_id":%q}}`, volumeID), cinderAuthHdr(seededProjectID, seededUserID))
	var resp struct {
		Transfer struct {
			ID      string `json:"id"`
			AuthKey string `json:"auth_key"`
		} `json:"transfer"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	if resp.Transfer.ID != "" {
		id := resp.Transfer.ID
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_transfers WHERE id = $1", id)
		})
	}
	return code, transferRef{resp.Transfer.ID, resp.Transfer.AuthKey}, body
}

func volumeState(t *testing.T, volumeID string) (status, project, user string, size int) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(),
		`SELECT status, project_id::text, COALESCE(user_id::text, ''), size_gb FROM volumes WHERE id = $1`,
		volumeID).Scan(&status, &project, &user, &size); err != nil {
		t.Fatalf("reading volume %s: %v", volumeID, err)
	}
	return status, project, user, size
}

func acceptBody(key string) string { return fmt.Sprintf(`{"accept":{"auth_key":%q}}`, key) }

// TestTransferHoldsTheVolumeUntilAcceptedOrCancelled: create moves the volume to awaiting-transfer and
// refuses a second transfer of it; while it waits, nothing may change, consume or attach it; cancel
// gives it back as available.
func TestTransferHoldsTheVolumeUntilAcceptedOrCancelled(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	vol := seedVolume(t, "spec025-transfer-held", "", "")

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	if status, _, _, _ := volumeState(t, vol); status != "awaiting-transfer" {
		t.Errorf("volume status after create = %q; want awaiting-transfer", status)
	}

	// A second transfer of the same volume is what let a sender take a transferred volume back.
	if code, _, body := createTransfer(t, r, vol); code != http.StatusBadRequest {
		t.Errorf("second transfer of a volume already awaiting one = %d; want 400. body: %s", code, body)
	}

	// Backups of another volume, for the restore-onto case.
	src := seedVolume(t, "spec025-transfer-backup-src", "", "")
	code, body = doReq(r, http.MethodPost, "/v3/backups", fmt.Sprintf(`{"backup":{"volume_id":%q,"name":"b"}}`, src), hdr)
	var backup struct {
		Backup struct {
			ID string `json:"id"`
		} `json:"backup"`
	}
	if err := json.Unmarshal([]byte(body), &backup); err != nil || backup.Backup.ID == "" {
		t.Fatalf("seed backup: code=%d body=%s", code, body)
	}
	t.Cleanup(func() {
		for _, id := range []string{src, vol} {
			_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_backups WHERE volume_id = $1", id)
			_, _ = testPool.Exec(context.Background(), "DELETE FROM snapshots WHERE volume_id = $1", id)
		}
	})

	for _, op := range []struct{ name, method, path, body string }{
		{"delete", http.MethodDelete, "/v3/volumes/" + vol, ""},
		{"extend", http.MethodPost, "/v3/volumes/" + vol + "/action", `{"os-extend":{"new_size":4}}`},
		{"retype", http.MethodPost, "/v3/volumes/" + vol + "/action", `{"os-retype":{"new_type":"gold"}}`},
		{"unmanage", http.MethodPost, "/v3/volumes/" + vol + "/action", `{"os-unmanage":{}}`},
		{"readonly", http.MethodPost, "/v3/volumes/" + vol + "/action", `{"os-update_readonly_flag":{"readonly":true}}`},
		{"reimage", http.MethodPost, "/v3/volumes/" + vol + "/action", fmt.Sprintf(`{"os-reimage":{"image_id":%q}}`, uuid.NewString())},
		{"attach", http.MethodPost, "/v3/volumes/" + vol + "/action", fmt.Sprintf(`{"os-attach":{"instance_uuid":%q,"mountpoint":"/dev/vdb"}}`, uuid.NewString())},
		{"snapshot", http.MethodPost, "/v3/snapshots", fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"s"}}`, vol)},
		{"backup", http.MethodPost, "/v3/backups", fmt.Sprintf(`{"backup":{"volume_id":%q,"name":"b"}}`, vol)},
		{"restore onto", http.MethodPost, "/v3/backups/" + backup.Backup.ID + "/action", fmt.Sprintf(`{"restore":{"volume_id":%q}}`, vol)},
	} {
		if code, body := doReq(r, op.method, op.path, op.body, hdr); code != http.StatusBadRequest {
			t.Errorf("%s while awaiting transfer = %d; want 400. body: %.200s", op.name, code, body)
		}
	}
	if status, project, _, size := volumeState(t, vol); status != "awaiting-transfer" || project != seededProjectID || size != 3 {
		t.Errorf("volume after the refused operations: status=%q project=%s size=%d; want it untouched", status, project, size)
	}

	// Reading and renaming stay allowed, as upstream.
	if code, body := doReq(r, http.MethodPut, "/v3/volumes/"+vol, `{"volume":{"name":"renamed"}}`, hdr); code != http.StatusOK {
		t.Errorf("rename while awaiting transfer = %d; want 200. body: %.200s", code, body)
	}

	if code, body := doReq(r, http.MethodDelete, "/v3/os-volume-transfer/"+xfer.id, "", hdr); code != http.StatusAccepted {
		t.Fatalf("cancel = %d; want 202. body: %s", code, body)
	}
	if status, _, _, _ := volumeState(t, vol); status != "available" {
		t.Errorf("volume status after cancel = %q; want available again", status)
	}
}

// TestConcurrentTransferCreatesHoldTheVolumeOnce: the status read before create's transaction cannot
// stop two creates that run at once — both read available. The conditional update inside it is what
// does, so the test makes both creates pass the read: it holds the volume row, which the read does not
// wait for and the update does, and releases it once both are queued behind it.
func TestConcurrentTransferCreatesHoldTheVolumeOnce(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-create-race", "", "")

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // releases the row if the test stops early
	if _, err := tx.Exec(ctx, "SELECT 1 FROM volumes WHERE id = $1 FOR UPDATE", vol); err != nil {
		t.Fatalf("hold the volume row: %v", err)
	}

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = createTransfer(t, r, vol)
		}(i)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		var queued int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE volumes SET status = $1%'`,
		).Scan(&queued); err != nil {
			t.Fatalf("reading lock waits: %v", err)
		}
		if queued >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d creates queued on the volume row; want both", queued)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release the volume row: %v", err)
	}
	wg.Wait()

	accepted := 0
	for _, c := range codes {
		if c == http.StatusAccepted {
			accepted++
		}
	}
	var pending int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM volume_transfers WHERE volume_id = $1 AND accepted = false", vol).Scan(&pending); err != nil {
		t.Fatalf("counting transfers: %v", err)
	}
	if accepted != 1 || pending != 1 {
		t.Errorf("two concurrent creates: %d answered 202 (codes %v), %d pending transfers; want exactly 1 of each",
			accepted, codes, pending)
	}
}

// TestTransferAcceptMovesTheVolumeOnce: accept hands the volume over as available, owned by the
// accepting project and user, and the transfer cannot be used a second time — by the recipient or by
// the sender.
func TestTransferAcceptMovesTheVolumeOnce(t *testing.T) {
	requireDB(t)
	ensureOtherProject(t)
	ensureTransferAcceptor(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-once", "", "")

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
		cinderAuthHdr(otherProjectID, transferAcceptorUser)); code != http.StatusAccepted {
		t.Fatalf("accept = %d; want 202. body: %s", code, body)
	}
	status, project, user, _ := volumeState(t, vol)
	if status != "available" || project != otherProjectID || user != transferAcceptorUser {
		t.Errorf("after accept: status=%q project=%s user=%s; want available, %s, %s",
			status, project, user, otherProjectID, transferAcceptorUser)
	}

	for _, who := range []struct{ name, project, user string }{
		{"recipient again", otherProjectID, transferAcceptorUser},
		{"sender", seededProjectID, seededUserID},
	} {
		if code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
			cinderAuthHdr(who.project, who.user)); code != http.StatusNotFound {
			t.Errorf("accept by the %s after it was accepted = %d; want 404. body: %.200s", who.name, code, body)
		}
	}
	if _, project, _, _ := volumeState(t, vol); project != otherProjectID {
		t.Errorf("volume owner after the repeated accepts = %s; want it still with %s", project, otherProjectID)
	}
}

// TestConcurrentAcceptsOfOneTransfer: two projects accepting the same transfer at once take different
// quota locks, so only the volume's own state can decide between them. Exactly one wins.
func TestConcurrentAcceptsOfOneTransfer(t *testing.T) {
	requireDB(t)
	ensureOtherProject(t)
	seedQuotaProject(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-race", "", "")

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	acceptors := []string{otherProjectID, quotaProject}
	codes := make([]int, len(acceptors))
	var wg sync.WaitGroup
	for i, p := range acceptors {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			codes[i], _ = doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
				cinderAuthHdr(p, seededUserID))
		}(i, p)
	}
	wg.Wait()

	winner := ""
	for i, c := range codes {
		if c == http.StatusAccepted {
			if winner != "" {
				t.Fatalf("both concurrent accepts answered 202 (codes %v); exactly one may", codes)
			}
			winner = acceptors[i]
		}
	}
	if winner == "" {
		t.Fatalf("no concurrent accept succeeded (codes %v); exactly one must", codes)
	}
	if _, project, _, _ := volumeState(t, vol); project != winner {
		t.Errorf("volume owner = %s; want the accept that answered 202, %s", project, winner)
	}
}

// TestAcceptRefusesAVolumeNoLongerAwaitingTransfer: what accept moves is decided by the volume's
// state, not by the transfer row alone, so a volume taken out of awaiting-transfer (an administrator's
// reset-status) cannot be moved by its old transfer.
func TestAcceptRefusesAVolumeNoLongerAwaitingTransfer(t *testing.T) {
	requireDB(t)
	ensureOtherProject(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-reset", "", "")

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	if _, err := testPool.Exec(t.Context(), "UPDATE volumes SET status = 'available' WHERE id = $1", vol); err != nil {
		t.Fatalf("reset status: %v", err)
	}
	if code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
		cinderAuthHdr(otherProjectID, seededUserID)); code != http.StatusBadRequest {
		t.Errorf("accept of a volume no longer awaiting transfer = %d; want 400. body: %.200s", code, body)
	}
	if _, project, _, _ := volumeState(t, vol); project != seededProjectID {
		t.Errorf("volume owner = %s; want it still with the sender", project)
	}
	var accepted bool
	if err := testPool.QueryRow(t.Context(), "SELECT accepted FROM volume_transfers WHERE id = $1", xfer.id).Scan(&accepted); err != nil {
		t.Fatalf("reading transfer: %v", err)
	}
	if accepted {
		t.Error("a refused accept consumed the transfer")
	}
}
