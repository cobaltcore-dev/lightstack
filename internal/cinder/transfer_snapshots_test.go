package cinder

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A volume's snapshots go with it when a transfer is accepted, as upstream's default does
// (cinder/db/api.py transfer_accept): they hold the volume's data, so leaving them behind left
// the sender a copy of what it had handed over, and counted against the wrong project's quota.
// Upstream refuses to create the transfer unless every snapshot is available, moves each only out
// of available, and charges the recipient for them (cinder/transfer/api.py).

// seedVolumeSnapshot inserts a snapshot of volumeID in project with the given status.
func seedVolumeSnapshot(t *testing.T, volumeID, project, status string, sizeGB int) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, created_at)
		 VALUES ($1, 'spec025-transfer-snap', $2, $3, $4, $5, now())`,
		id, volumeID, project, sizeGB, status); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM snapshots WHERE id = $1", id) })
	return id
}

func snapshotProject(t *testing.T, id string) string {
	t.Helper()
	var p string
	if err := testPool.QueryRow(t.Context(), "SELECT project_id::text FROM snapshots WHERE id = $1", id).Scan(&p); err != nil {
		t.Fatalf("reading snapshot %s: %v", id, err)
	}
	return p
}

func TestTransferCarriesTheVolumesSnapshots(t *testing.T) {
	requireDB(t)
	ensureOtherProject(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-with-snaps", "", "")
	snaps := []string{
		seedVolumeSnapshot(t, vol, seededProjectID, "available", 3),
		seedVolumeSnapshot(t, vol, seededProjectID, "available", 3),
	}
	defer testPool.Exec(context.Background(), "DELETE FROM snapshots WHERE volume_id = $1", vol) //nolint:errcheck

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
		cinderAuthHdr(otherProjectID, seededUserID)); code != http.StatusAccepted {
		t.Fatalf("accept = %d; want 202. body: %s", code, body)
	}
	if _, project, _, _ := volumeState(t, vol); project != otherProjectID {
		t.Fatalf("volume owner = %s; want the recipient", project)
	}
	for _, s := range snaps {
		if p := snapshotProject(t, s); p != otherProjectID {
			t.Errorf("snapshot %s stayed with %s; it must go with its volume to %s", s, p, otherProjectID)
		}
	}
}

func TestTransferRefusedWhileASnapshotIsNotAvailable(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-snap-creating", "", "")
	creating := seedVolumeSnapshot(t, vol, seededProjectID, "creating", 3)

	code, _, body := createTransfer(t, r, vol)
	if code != http.StatusBadRequest {
		t.Errorf("transfer of a volume with a snapshot still creating = %d; want 400. body: %s", code, body)
	}
	if !strings.Contains(body, creating) {
		t.Errorf("the refusal does not name the snapshot; body: %s", body)
	}
	if status, _, _, _ := volumeState(t, vol); status != "available" {
		t.Errorf("volume status after the refused transfer = %q; want it still available", status)
	}
}

func TestTransferAcceptChargesTheSnapshotsToTheRecipient(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "snapshots", 1)
	setQuotaLimit(t, "gigabytes", 10000)
	vol := seedSizedVolume(t, quotaOtherProject, 1)
	snaps := []string{seedVolumeSnapshot(t, vol, quotaOtherProject, "available", 1), seedVolumeSnapshot(t, vol, quotaOtherProject, "available", 1)}

	transferID, authKey := uuid.NewString(), "secret-key"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volume_transfers (id, volume_id, name, auth_key, source_project_id, accepted, created_at)
		 VALUES ($1, $2, 'xfer', $3, $4, false, NOW())`, transferID, vol, authKey, quotaOtherProject); err != nil {
		t.Fatalf("seeding transfer: %v", err)
	}
	markAwaitingTransfer(t, vol)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_transfers WHERE id = $1", transferID)
	})

	code, resp := quotaReq(quotaRouter(t), http.MethodPost, "/v3/"+quotaProject+"/os-volume-transfer/"+transferID+"/accept",
		fmt.Sprintf(`{"accept":{"auth_key":%q}}`, authKey), quotaProject)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(resp, "snapshots") {
		t.Errorf("accept of a volume with two snapshots against a one-snapshot allowance: %d %s; want 413 naming snapshots", code, resp)
	}
	if status, project, _, _ := volumeState(t, vol); status != "awaiting-transfer" || project != quotaOtherProject {
		t.Errorf("volume after the refused accept: %s in %s; want it still awaiting transfer with the sender", status, project)
	}
	for _, s := range snaps {
		if p := snapshotProject(t, s); p != quotaOtherProject {
			t.Errorf("snapshot %s moved to %s on a refused accept", s, p)
		}
	}
}

func TestTransferAcceptRefusedWhenASnapshotStoppedBeingAvailable(t *testing.T) {
	requireDB(t)
	ensureOtherProject(t)
	r := newCinderTestRouter()
	vol := seedVolume(t, "spec025-transfer-snap-errored", "", "")
	ok := seedVolumeSnapshot(t, vol, seededProjectID, "available", 3)
	broken := seedVolumeSnapshot(t, vol, seededProjectID, "available", 3)

	code, xfer, body := createTransfer(t, r, vol)
	if code != http.StatusAccepted {
		t.Fatalf("create transfer = %d; want 202. body: %s", code, body)
	}
	if _, err := testPool.Exec(t.Context(), "UPDATE snapshots SET status = 'error' WHERE id = $1", broken); err != nil {
		t.Fatalf("breaking a snapshot: %v", err)
	}
	if code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer/"+xfer.id+"/accept", acceptBody(xfer.authKey),
		cinderAuthHdr(otherProjectID, seededUserID)); code != http.StatusBadRequest {
		t.Errorf("accept with a snapshot no longer available = %d; want 400. body: %s", code, body)
	}
	if _, project, _, _ := volumeState(t, vol); project != seededProjectID {
		t.Errorf("volume moved to %s although one of its snapshots could not; volume and snapshots must move together", project)
	}
	if p := snapshotProject(t, ok); p != seededProjectID {
		t.Errorf("an available snapshot moved to %s on a refused accept", p)
	}
}
