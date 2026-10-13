package cinder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Volume and snapshot creates reserve their row before the backing store is touched, do
// the backing work outside any transaction, then finalise or clean up (FR-032 – FR-036,
// research R13, contracts/block-storage-reservation.md).

func volumeCreateBody(name, image string) string {
	if image == "" {
		return fmt.Sprintf(`{"volume":{"name":%q,"size":1}}`, name)
	}
	return fmt.Sprintf(`{"volume":{"name":%q,"size":1,"imageRef":%q}}`, name, image)
}

func createdVolumeID(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Volume struct {
			ID string `json:"id"`
		} `json:"volume"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Volume.ID == "" {
		t.Fatalf("no volume id in %s (%v)", body, err)
	}
	return out.Volume.ID
}

// During the backing call the row is visible as creating, reserved under this node's
// name; afterwards it is available — bootable, with its source image, when made from one.
func TestVolumeCreateReservesThenFinalises(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	var image string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('spec028-img', $1, 'active', 'private', 'raw', 'bare') RETURNING id::text`, project).Scan(&image); err != nil {
		t.Fatalf("seed an image: %v", err)
	}
	type seen struct {
		status     string
		reservedBy *string
		found      bool
	}
	var during seen
	fake := &fakeBacking{}
	fake.during = func(id string) {
		var s seen
		s.status, s.reservedBy, _, _, s.found = reservedVolume(t, id)
		during = s
	}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-reserve", image), cinderAuthHdr(project, seededUserID))
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %s; want 202", code, body)
	}
	id := createdVolumeID(t, body)
	if !during.found || during.status != "creating" || during.reservedBy == nil || *during.reservedBy != "node-a" {
		t.Errorf("during the backing call the row was found=%v status=%q reserved_by=%v; want creating, reserved by node-a",
			during.found, during.status, during.reservedBy)
	}
	status, _, bootable, source, _ := reservedVolume(t, id)
	if status != "available" || !bootable || source == nil || *source != image {
		t.Errorf("after the create: status=%q bootable=%v source_image_id=%v; want available, bootable, %s", status, bootable, source, image)
	}
}

// A volume in creating counts towards the quota, so a create racing one whose backing is
// still under way cannot take the project past its limit.
func TestVolumeReservationsCountTowardsTheQuota(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, map[string]int{"volumes": 1})
	started := make(chan struct{})
	fake := &fakeBacking{hold: make(chan struct{})}
	fake.during = func(string) { close(started) }
	_, r := backingRouter(t, fake, "node-a")
	hdr := cinderAuthHdr(project, seededUserID)

	first := make(chan int, 1)
	go func() {
		code, _ := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-first", ""), hdr)
		first <- code
	}()
	<-started
	fake.during = nil
	// The second create must be refused by the quota BEFORE it reaches the backing. Run it off
	// the test goroutine: if the reservation stopped counting it would instead block on fake.hold,
	// and a synchronous call would hang the suite until -timeout rather than failing here.
	second := make(chan int, 1)
	go func() {
		code, _ := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-second", ""), hdr)
		second <- code
	}()
	select {
	case code := <-second:
		close(fake.hold)
		if code != http.StatusRequestEntityTooLarge {
			t.Errorf("second create while the first is creating = %d; want 413, the reservation counts", code)
		}
	case <-time.After(5 * time.Second):
		close(fake.hold) // unblock the second create, which wrongly proceeded to the backing
		t.Fatal("second create did not return within 5s; the reservation is not counting toward the quota")
	}
	if code := <-first; code != http.StatusAccepted {
		t.Errorf("first create = %d; want 202", code)
	}
}

// A create whose backing fails leaves nothing: no row, and the partial backing removed.
// The answer is today's.
func TestVolumeBackingFailureLeavesNothing(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	fake := &fakeBacking{fail: errors.New("backing store unavailable")}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-fails", ""), cinderAuthHdr(project, seededUserID))
	if code != http.StatusServiceUnavailable {
		t.Errorf("create = %d %s; want 503", code, body)
	}
	var rows int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM volumes WHERE project_id = $1`, project).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d volume rows after a failed backing; want 0", rows)
	}
	attempted := fake.calls(&fake.attempted)
	if len(attempted) != 1 || !slices.Contains(fake.calls(&fake.deleted), attempted[0]) {
		t.Errorf("attempted %v, deleted %v; want the partial backing removed", attempted, fake.calls(&fake.deleted))
	}
}

// A row removed while its backing is created — the create no longer has a volume — is
// not resurrected: the backing is reclaimed and the request fails.
func TestVolumeRemovedWhileCreatingIsReclaimed(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	fake := &fakeBacking{}
	fake.during = func(id string) {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, id)
	}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-vanishes", ""), cinderAuthHdr(project, seededUserID))
	if code < 500 {
		t.Errorf("create = %d %s; want a failure", code, body)
	}
	attempted := fake.calls(&fake.attempted)
	if len(attempted) != 1 || !slices.Contains(fake.calls(&fake.deleted), attempted[0]) {
		t.Errorf("attempted %v, deleted %v; want the backing reclaimed", attempted, fake.calls(&fake.deleted))
	}
	if _, _, _, _, found := reservedVolume(t, attempted[0]); found {
		t.Errorf("the removed volume came back")
	}
}

// A volume moved out of `creating` while its backing is created (an admin os-reset_status,
// a detach racing the create) belongs to that surviving row, not the create: the create's
// abandon must keep the backing rather than delete it under the live row.
func TestVolumeLeftCreatingKeepsItsBacking(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	fake := &fakeBacking{}
	fake.during = func(id string) {
		_, _ = testPool.Exec(context.Background(),
			`UPDATE volumes SET status = 'available' WHERE id = $1`, id)
	}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-reset", ""), cinderAuthHdr(project, seededUserID))
	if code < 500 {
		t.Errorf("create = %d %s; want a failure (the finalise found no creating row)", code, body)
	}
	attempted := fake.calls(&fake.attempted)
	if len(attempted) != 1 {
		t.Fatalf("attempted %v; want one create", attempted)
	}
	if slices.Contains(fake.calls(&fake.deleted), attempted[0]) {
		t.Errorf("the backing of the surviving volume was deleted; want it kept")
	}
	if status, _, _, _, found := reservedVolume(t, attempted[0]); !found || status != "available" {
		t.Errorf("surviving volume is found=%v status=%q; want available", found, status)
	}
}

// A snapshot is available before the create answers, and the answer is still upstream's
// 202 with status creating.
func TestSnapshotAvailableBeforeTheResponse(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	_, r := backingRouter(t, &fakeBacking{}, "node-a")
	hdr := cinderAuthHdr(project, seededUserID)
	code, body := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-snapsource", ""), hdr)
	if code != http.StatusAccepted {
		t.Fatalf("create volume = %d %s", code, body)
	}
	volume := createdVolumeID(t, body)

	code, body = doReq(r, http.MethodPost, "/v3/snapshots", fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"spec028-snap"}}`, volume), hdr)
	if code != http.StatusAccepted {
		t.Fatalf("create snapshot = %d %s; want 202", code, body)
	}
	var out struct {
		Snapshot struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Snapshot.Status != "creating" {
		t.Errorf("response status = %q; want creating, as upstream answers", out.Snapshot.Status)
	}
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM snapshots WHERE id = $1`, out.Snapshot.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "available" {
		t.Errorf("stored status right after the response = %q; want available", status)
	}
}

// seedAvailableVolume inserts a finished volume directly, so a snapshot test can drive the
// snapshot create alone without the volume create going through the same fake.
func seedAvailableVolume(t *testing.T, project string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1, 'spec028-vol', $2, $3, 1, 'available')`,
		id, project, seededUserID); err != nil {
		t.Fatalf("seed available volume: %v", err)
	}
	return id
}

// A snapshot whose backing cannot be created answers 503, and leaves no snapshot row; its
// partial backing is reclaimed.
func TestSnapshotBackingFailureLeavesNothing(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	volume := seedAvailableVolume(t, project)
	fake := &fakeBacking{fail: errors.New("backing store unavailable")}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/snapshots",
		fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"spec028-snap"}}`, volume), cinderAuthHdr(project, seededUserID))
	if code != http.StatusServiceUnavailable {
		t.Errorf("snapshot create = %d %s; want 503", code, body)
	}
	var rows int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM snapshots WHERE volume_id = $1`, volume).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d snapshot rows after a failed backing; want 0", rows)
	}
	if attempted := fake.calls(&fake.attempted); len(attempted) != 1 || !slices.Contains(fake.calls(&fake.deletedSnapshots), attempted[0]) {
		t.Errorf("attempted %v, deletedSnapshots %v; want the partial backing reclaimed", attempted, fake.calls(&fake.deletedSnapshots))
	}
}

// A snapshot row removed while its backing is created — the create no longer has a row —
// fails and reclaims the backing rather than resurrecting the row.
func TestSnapshotRemovedWhileCreatingIsReclaimed(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	volume := seedAvailableVolume(t, project)
	fake := &fakeBacking{}
	fake.during = func(snapshotID string) {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM snapshots WHERE id = $1`, snapshotID)
	}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/snapshots",
		fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"spec028-snap"}}`, volume), cinderAuthHdr(project, seededUserID))
	if code < 500 {
		t.Errorf("snapshot create = %d %s; want a failure (finalise found no creating row)", code, body)
	}
	if attempted := fake.calls(&fake.attempted); len(attempted) != 1 || !slices.Contains(fake.calls(&fake.deletedSnapshots), attempted[0]) {
		t.Errorf("attempted %v, deletedSnapshots %v; want the backing reclaimed", attempted, fake.calls(&fake.deletedSnapshots))
	}
}

// A snapshot moved out of creating while its backing is made — an admin os-reset_status to
// available, say — keeps its backing: the surviving row owns it. This mirrors
// TestVolumeLeftCreatingKeepsItsBacking for the snapshot abandon path.
func TestSnapshotLeftCreatingKeepsItsBacking(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	volume := seedAvailableVolume(t, project)
	fake := &fakeBacking{}
	fake.during = func(snapshotID string) {
		_, _ = testPool.Exec(context.Background(), `UPDATE snapshots SET status = 'available' WHERE id = $1`, snapshotID)
	}
	_, r := backingRouter(t, fake, "node-a")

	code, body := doReq(r, http.MethodPost, "/v3/snapshots",
		fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"spec028-snap"}}`, volume), cinderAuthHdr(project, seededUserID))
	if code < 500 {
		t.Errorf("snapshot create = %d %s; want a failure (the finalise found no creating row)", code, body)
	}
	attempted := fake.calls(&fake.attempted)
	if len(attempted) != 1 {
		t.Fatalf("attempted %v; want one create", attempted)
	}
	if slices.Contains(fake.calls(&fake.deletedSnapshots), attempted[0]) {
		t.Errorf("the backing of the surviving snapshot was deleted; want it kept")
	}
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM snapshots WHERE id = $1`, attempted[0]).Scan(&status); err != nil || status != "available" {
		t.Errorf("surviving snapshot is status=%q (err %v); want available", status, err)
	}
}

// SC-006: while a volume's backing is created — an image copy can take minutes — no
// transaction is open, so another project's write completes at once.
func TestNoTransactionHeldDuringTheCopy(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	other := cinderProject(t, nil)
	// SC-006 names the image-copy path specifically ("can take minutes"), so the create carries
	// an image ref: the fake then blocks in CreateVolumeFromImage, not the blank-volume path.
	var image string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('spec028-copy-img', $1, 'active', 'private', 'raw', 'bare') RETURNING id::text`, project).Scan(&image); err != nil {
		t.Fatalf("seed an image: %v", err)
	}
	started := make(chan struct{})
	fake := &fakeBacking{hold: make(chan struct{})}
	fake.during = func(string) { close(started) }
	_, r := backingRouter(t, fake, "node-a")

	done := make(chan int)
	go func() {
		code, _ := doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody("spec028-slow-copy", image), cinderAuthHdr(project, seededUserID))
		done <- code
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	start := time.Now()
	_, err := testPool.Exec(ctx, `UPDATE projects SET description = 'written during a copy' WHERE id = $1`, other)
	elapsed := time.Since(start)
	cancel()
	close(fake.hold)
	<-done
	if err != nil || elapsed > time.Second {
		t.Errorf("another project's write during the copy took %v (err %v); want under 1s", elapsed, err)
	}
}
