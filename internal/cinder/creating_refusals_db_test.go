package cinder

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// FR-036a: requests against a volume or snapshot that is still being created answer 400
// with upstream's messages (cinder/volume/api.py), rather than acting on a resource whose
// backing is not there yet.
func TestRequestsAgainstCreatingResourcesAreRefused(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	volume, snapVolume, snapshot := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, v := range []struct{ id, status string }{{volume, "creating"}, {snapVolume, "available"}} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, reserved_by)
			 VALUES ($1, 'spec028-creating', $2, $3, 1, $4, 'node-a')`, v.id, project, seededUserID, v.status); err != nil {
			t.Fatalf("seed a volume: %v", err)
		}
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, reserved_by)
		 VALUES ($1, 'spec028-creating', $2, $3, 1, 'creating', 'node-a')`, snapshot, snapVolume, project); err != nil {
		t.Fatalf("seed a snapshot: %v", err)
	}
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	hdr := cinderAuthHdr(project, seededUserID)

	for _, tc := range []struct {
		name, method, path, body, want string
	}{
		{"delete volume", http.MethodDelete, "/v3/volumes/" + volume, "",
			"Invalid volume: Volume status must be available or error or error_restoring or error_extending or error_managing and must not be migrating, attached, belong to a group, have snapshots, awaiting a transfer, or be disassociated from snapshots after volume transfer."},
		{"extend volume", http.MethodPost, "/v3/volumes/" + volume + "/action", `{"os-extend":{"new_size":2}}`,
			fmt.Sprintf("Invalid volume: Volume %s status must be 'available' to extend, currently creating.", volume)},
		{"delete snapshot", http.MethodDelete, "/v3/snapshots/" + snapshot, "",
			"Invalid snapshot: Snapshot status must be available or error and must not be part of a group."},
		{"attach volume", http.MethodPost, "/v3/volumes/" + volume + "/action",
			`{"os-attach":{"instance_uuid":"` + uuid.NewString() + `","mountpoint":"/dev/vdb"}}`, ""},
		{"detach volume", http.MethodPost, "/v3/volumes/" + volume + "/action", `{"os-detach":{}}`,
			"Invalid volume: Volume status must be in-use to detach, currently creating."},
		{"unmanage volume", http.MethodPost, "/v3/volumes/" + volume + "/action", `{"os-unmanage":{}}`,
			"Invalid volume: Volume status must be available or error or error_restoring or error_extending or error_managing and must not be migrating, attached, belong to a group, have snapshots, awaiting a transfer, or be disassociated from snapshots after volume transfer."},
		{"snapshot a creating volume", http.MethodPost, "/v3/snapshots",
			`{"snapshot":{"volume_id":"` + volume + `"}}`,
			fmt.Sprintf("Invalid volume: Volume %s status must be available, but current status is: creating.", volume)},
		{"back up a creating volume", http.MethodPost, "/v3/backups",
			`{"backup":{"volume_id":"` + volume + `"}}`,
			"Invalid volume: Volume to be backed up must be available or in-use, but the current status is "},
		{"manage a snapshot of a creating volume", http.MethodPost, "/v3/os-snapshot-manage",
			`{"snapshot":{"volume_id":"` + volume + `","ref":{"source-name":"s"}}}`,
			fmt.Sprintf("Invalid volume: Volume %s status must be available, but current status is: creating.", volume)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, tc.method, tc.path, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Errorf("code = %d %s; want 400", code, body)
			}
			if tc.want != "" && !strings.Contains(body, `"message":"`+tc.want) {
				t.Errorf("body %s;\n want the message %q", body, tc.want)
			}
		})
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM volumes WHERE id = $1`, volume).Scan(&status); err != nil || status != "creating" {
		t.Errorf("the creating volume is now %q (%v); want it untouched", status, err)
	}
}

// TestSnapshotResetStatusFreesAStuckSnapshot (SRC-40): a creating snapshot whose reserving node
// never returns is stuck — its delete is refused and the sweep cannot reach it. os-reset_status is
// the admin-only way out, as upstream Cinder has.
func TestSnapshotResetStatusFreesAStuckSnapshot(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	volume, snapshot := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status)
		 VALUES ($1, 'spec028-stuck', $2, $3, 1, 'available')`, volume, project, seededUserID); err != nil {
		t.Fatalf("seed a volume: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, reserved_by)
		 VALUES ($1, 'spec028-stuck', $2, $3, 1, 'creating', 'gone-node')`, snapshot, volume, project); err != nil {
		t.Fatalf("seed a snapshot: %v", err)
	}
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	action := "/v3/snapshots/" + snapshot + "/action"
	reset := `{"os-reset_status":{"status":"error"}}`

	// A non-admin cannot reset it.
	nonAdmin := map[string]string{"X-Test-Project": project, "X-Test-User": seededUserID}
	if code, body := doReq(r, http.MethodPost, action, reset, nonAdmin); code != http.StatusForbidden {
		t.Fatalf("non-admin reset: code = %d %s; want 403", code, body)
	}

	// An admin resets it to error, and the previously-refused delete now succeeds.
	admin := cinderAuthHdr(project, seededUserID)
	if code, body := doReq(r, http.MethodPost, action, reset, admin); code != http.StatusAccepted {
		t.Fatalf("admin reset: code = %d %s; want 202", code, body)
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM snapshots WHERE id = $1`, snapshot).Scan(&status); err != nil || status != "error" {
		t.Fatalf("snapshot status = %q (%v); want error", status, err)
	}
	if code, body := doReq(r, http.MethodDelete, "/v3/snapshots/"+snapshot, "", admin); code != http.StatusNoContent {
		t.Fatalf("delete after reset: code = %d %s; want 204", code, body)
	}

	// A reset of a snapshot that does not exist is 404.
	if code, _ := doReq(r, http.MethodPost, "/v3/snapshots/"+uuid.NewString()+"/action", reset, admin); code != http.StatusNotFound {
		t.Fatalf("reset of a missing snapshot: code = %d; want 404", code)
	}
}

// TestManageSnapshotImportsAnExistingSnapshot: os-snapshot-manage against an available volume
// inserts the row and answers 2xx. It used to 500 on a nonexistent snapshots.updated_at column.
func TestManageSnapshotImportsAnExistingSnapshot(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	volume := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status)
		 VALUES ($1, 'spec028-manage-src', $2, $3, 1, 'available')`, volume, project, seededUserID); err != nil {
		t.Fatalf("seed a volume: %v", err)
	}
	r := cinderRouter(NewService(testPool, "stub", "", "", testNodeName))
	body := `{"snapshot":{"volume_id":"` + volume + `","ref":{"source-name":"snap-on-backend"},"name":"imported"}}`
	code, resp := doReq(r, http.MethodPost, "/v3/os-snapshot-manage", body, cinderAuthHdr(project, seededUserID))
	if code < 200 || code >= 300 {
		t.Fatalf("manage snapshot = %d %s; want 2xx", code, resp)
	}
	var n int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE volume_id = $1 AND status = 'available'`, volume).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshots rows = %d (%v); want one available snapshot", n, err)
	}
}
