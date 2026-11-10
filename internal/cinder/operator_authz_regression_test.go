package cinder

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestOperatorUpdateDeleteAndUnmanageRoutesRequireAdmin(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "admin"}

	typeID, qosID := uuid.NewString(), uuid.NewString()
	volumeID, unmanageVolumeID, resetVolumeID, snapshotID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(t.Context(), "INSERT INTO volume_types (id, name, description, is_public) VALUES ($1, $2, '', false)", typeID, "review-type-"+typeID[:8]); err != nil {
		t.Fatalf("seed volume type: %v", err)
	}
	if _, err := testPool.Exec(t.Context(), "INSERT INTO qos_specs (id, name, consumer, specs, project_id) VALUES ($1, $2, 'back-end', '{}', $3)", qosID, "review-qos-"+qosID[:8], seededProjectID); err != nil {
		t.Fatalf("seed QoS spec: %v", err)
	}
	for _, volume := range []struct{ id, name string }{
		{volumeID, "review-managed-snapshot-" + volumeID[:8]},
		{unmanageVolumeID, "review-unmanage-" + unmanageVolumeID[:8]},
		{resetVolumeID, "review-reset-" + resetVolumeID[:8]},
	} {
		if _, err := testPool.Exec(t.Context(), "INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1, $2, $3, $4, 1, 'available')", volume.id, volume.name, seededProjectID, seededUserID); err != nil {
			t.Fatalf("seed volume: %v", err)
		}
	}
	if _, err := testPool.Exec(t.Context(), "INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status) VALUES ($1, 'review-snapshot-reset', $2, $3, 1, 'creating')", snapshotID, resetVolumeID, seededProjectID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM snapshots WHERE id = $1 OR volume_id = $2", snapshotID, volumeID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id IN ($1, $2, $3)", volumeID, unmanageVolumeID, resetVolumeID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM qos_specs WHERE id = $1", qosID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_types WHERE id = $1", typeID)
	})

	manageSnapshotBody := fmt.Sprintf("{\"snapshot\":{\"volume_id\":%q,\"ref\":{\"source-name\":\"source\"},\"name\":\"review-import-%s\"}}", volumeID, volumeID[:8])
	memberCases := []struct{ name, method, path, body string }{
		{"type update", http.MethodPut, "/v3/types/" + typeID, "{}"},
		{"type delete", http.MethodDelete, "/v3/types/" + typeID, ""},
		{"extra specs create", http.MethodPost, "/v3/types/" + typeID + "/extra_specs", "{}"},
		{"extra spec update", http.MethodPut, "/v3/types/" + typeID + "/extra_specs/review", "{}"},
		{"extra spec delete", http.MethodDelete, "/v3/types/" + typeID + "/extra_specs/review", ""},
		{"type access grant", http.MethodPost, "/v3/types/" + typeID + "/action", "{}"},
		{"QoS create", http.MethodPost, "/v3/qos-specs", "{}"},
		{"QoS update", http.MethodPut, "/v3/qos-specs/" + qosID, "{}"},
		{"QoS delete", http.MethodDelete, "/v3/qos-specs/" + qosID, ""},
		{"manage volume", http.MethodPost, "/v3/os-volume-manage", "{}"},
		{"list manageable volumes", http.MethodGet, "/v3/manageable_volumes?host=h1", ""},
		{"manage snapshot", http.MethodPost, "/v3/os-snapshot-manage", manageSnapshotBody},
		{"unmanage volume", http.MethodPost, "/v3/volumes/" + unmanageVolumeID + "/action", `{"os-unmanage":{}}`},
		{"reset volume status", http.MethodPost, "/v3/volumes/" + resetVolumeID + "/action", `{"os-reset_status":{"status":"available"}}`},
		{"reset snapshot status", http.MethodPost, "/v3/snapshots/" + snapshotID + "/action", `{"os-reset_status":{"status":"available"}}`},
	}
	for _, tc := range memberCases {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := doReq(r, tc.method, tc.path, tc.body, member); code != http.StatusForbidden {
				t.Errorf("member %s %s: code=%d body=%s; want 403", tc.method, tc.path, code, body)
			}
		})
	}

	checkAdmin := func(method, path, body string, want int) {
		t.Helper()
		if code, response := doReq(r, method, path, body, admin); code != want {
			t.Errorf("admin %s %s: code=%d body=%s; want %d", method, path, code, response, want)
		}
	}
	checkAdmin(http.MethodPut, "/v3/types/"+typeID, "{\"volume_type\":{\"description\":\"admin\"}}", http.StatusOK)
	checkAdmin(http.MethodPost, "/v3/types/"+typeID+"/extra_specs", "{\"extra_specs\":{\"review\":\"admin\"}}", http.StatusOK)
	checkAdmin(http.MethodPut, "/v3/types/"+typeID+"/extra_specs/review", "{\"review\":\"updated\"}", http.StatusOK)
	checkAdmin(http.MethodDelete, "/v3/types/"+typeID+"/extra_specs/review", "", http.StatusNoContent)
	checkAdmin(http.MethodPost, "/v3/types/"+typeID+"/action", fmt.Sprintf("{\"addProjectAccess\":{\"project\":%q}}", seededProjectID), http.StatusAccepted)
	checkAdmin(http.MethodPost, "/v3/types/"+typeID+"/action", fmt.Sprintf("{\"removeProjectAccess\":{\"project\":%q}}", seededProjectID), http.StatusAccepted)
	checkAdmin(http.MethodPut, "/v3/qos-specs/"+qosID, "{\"qos_specs\":{\"iops\":\"10\"}}", http.StatusOK)
	checkAdmin(http.MethodDelete, "/v3/qos-specs/"+qosID, "", http.StatusAccepted)
	checkAdmin(http.MethodGet, "/v3/manageable_volumes?host=h1", "", http.StatusOK)
	checkAdmin(http.MethodPost, "/v3/os-snapshot-manage", manageSnapshotBody, http.StatusAccepted)
	checkAdmin(http.MethodPost, "/v3/volumes/"+unmanageVolumeID+"/action", "{\"os-unmanage\":{}}", http.StatusAccepted)
	checkAdmin(http.MethodPost, "/v3/volumes/"+resetVolumeID+"/action", "{\"os-reset_status\":{\"status\":\"available\"}}", http.StatusAccepted)
	checkAdmin(http.MethodPost, "/v3/snapshots/"+snapshotID+"/action", "{\"os-reset_status\":{\"status\":\"available\"}}", http.StatusAccepted)
	checkAdmin(http.MethodDelete, "/v3/types/"+typeID, "", http.StatusAccepted)
	checkAdmin(http.MethodPost, "/v3/os-volume-manage", "{\"volume\":{\"host\":\"h1\",\"ref\":{\"source-name\":\"x\"},\"name\":\"review-managed\"}}", http.StatusAccepted)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE name = 'review-managed'")
	})
}
