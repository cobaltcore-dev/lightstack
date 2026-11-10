package nova

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestVolumeAttachAndDetachRefusals(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	instanceID := seedConsoleInstance(t, seededProjectID)
	inUseID, alreadyAttachedID := uuid.NewString(), uuid.NewString()
	for _, volume := range []struct {
		id, name, status string
		attached         bool
	}{
		{inUseID, "review-in-use-" + inUseID[:8], "in-use", false},
		{alreadyAttachedID, "review-attached-" + alreadyAttachedID[:8], "available", true},
	} {
		query := "INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1, $2, $3, $4, 1, $5)"
		args := []any{volume.id, volume.name, seededProjectID, seededUserID, volume.status}
		if volume.attached {
			query = "INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, attached_to_instance_id) VALUES ($1, $2, $3, $4, 1, $5, $6)"
			args = append(args, instanceID)
		}
		if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
			t.Fatalf("seed volume %s: %v", volume.id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_attachments WHERE volume_id IN ($1, $2)", inUseID, alreadyAttachedID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id IN ($1, $2)", inUseID, alreadyAttachedID)
	})

	headers := testAuthHdr(seededProjectID, seededUserID)
	attach := func(volumeID string) (int, string) {
		body, err := json.Marshal(map[string]any{"volumeAttachment": map[string]string{"volumeId": volumeID}})
		if err != nil {
			t.Fatalf("marshal attach request: %v", err)
		}
		return doReq(r, http.MethodPost, "/v2.1/servers/"+instanceID+"/os-volume_attachments", string(body), headers)
	}
	for _, tc := range []struct {
		name, volumeID string
		want           int
	}{
		{"unknown volume", uuid.NewString(), http.StatusNotFound},
		{"wrong status", inUseID, http.StatusBadRequest},
		{"already attached", alreadyAttachedID, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := attach(tc.volumeID); code != tc.want {
				t.Errorf("attach %s: code=%d body=%s; want %d", tc.name, code, body, tc.want)
			}
		})
	}
	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+instanceID+"/os-volume_attachments/"+uuid.NewString(), "", headers); code != http.StatusNotFound {
		t.Errorf("detach without an attachment: code=%d body=%s; want 404", code, body)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM volume_attachments WHERE volume_id IN ($1, $2)", inUseID, alreadyAttachedID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("refused requests created %d volume attachments", count)
	}
}
