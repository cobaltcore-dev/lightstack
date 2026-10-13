package cinder

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// These tests drive the defensive error branches — the ones that log and answer 500/503
// when a query fails — by making a table unavailable under the request (dbtest.Unavailable
// renames it away on SQLite, locks it on PostgreSQL). They assert the handler surfaces the
// failure rather than proceeding on a zero value.

// CreateSnapshot's volume lookup failing (not merely ErrNoRows) answers 500.
func TestCreateSnapshotVolumeLookupError(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	volume := seedAvailableVolume(t, project)
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	release := dbtest.Unavailable(t, testPool, "volumes")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodPost, "/v3/snapshots",
		fmt.Sprintf(`{"snapshot":{"volume_id":%q,"name":"s"}}`, volume), cinderAuthHdr(project, seededUserID))
	if code != http.StatusInternalServerError {
		t.Errorf("snapshot create with volumes unavailable = %d %s; want 500", code, body)
	}
}

// ManageSnapshot's volume lookup failing answers 500.
func TestManageSnapshotVolumeLookupError(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	volume := seedAvailableVolume(t, project)
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	release := dbtest.Unavailable(t, testPool, "volumes")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodPost, "/v3/os-snapshot-manage",
		fmt.Sprintf(`{"snapshot":{"volume_id":%q,"ref":{"source-name":"s"}}}`, volume), cinderAuthHdr(project, seededUserID))
	if code != http.StatusInternalServerError {
		t.Errorf("manage snapshot with volumes unavailable = %d %s; want 500", code, body)
	}
}

// DeleteSnapshot's lookup failing (not merely ErrNoRows) answers 500, rather than
// proceeding to delete a backing with an empty volume id.
func TestDeleteSnapshotLookupError(t *testing.T) {
	requireDB(t)
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	release := dbtest.Unavailable(t, testPool, "snapshots")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodDelete, "/v3/snapshots/"+uuid.NewString(), "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusInternalServerError {
		t.Errorf("delete snapshot with snapshots unavailable = %d %s; want 500", code, body)
	}
}

// The startup sweep logs and continues when its UPDATE … RETURNING cannot run; it must not
// panic, and it must leave rows untouched.
func TestSweepContinuesWhenTablesAreUnavailable(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	project := cinderProject(t, nil)
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, reserved_by)
		 VALUES ($1, 'spec028-sweepfail', $2, $3, 1, 'creating', $4)`, id, project, seededUserID, testNodeName); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc, _ := backingRouter(t, &fakeBacking{}, testNodeName)

	releaseSnap := dbtest.Unavailable(t, testPool, "snapshots")
	releaseVol := dbtest.Unavailable(t, testPool, "volumes")
	sweepCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	svc.SweepInterruptedReservations(sweepCtx) // must not panic despite both queries failing
	cancel()
	releaseVol()
	releaseSnap()

	if status, _, _, _, _ := reservedVolume(t, id); status != "creating" {
		t.Errorf("volume status = %q after a failed sweep; want it untouched at creating", status)
	}
}
