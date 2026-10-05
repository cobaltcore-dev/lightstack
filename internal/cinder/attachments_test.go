package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Volume attachments as upstream Cinder reports them (SPEC-020 FR-027).
//
// Every attachment is a row in volume_attachments, whichever API made it, and carries the
// device it was attached at. Upstream's attachment object (cinder/api/v2/views/volumes.py
// _get_attachments) is {id, attachment_id, volume_id, server_id, host_name, device,
// attached_at}, where "id" is the volume's id. Lightstack used to report only server_id
// and a device hardcoded to /dev/vdb, so a boot volume at vda read as vdb.

// seedAttachedVolume records a server in the seeded project and a volume attached to it
// at device, as the compute service records one. It returns the server, volume and
// attachment ids.
func seedAttachedVolume(t *testing.T, device string) (serverID, volumeID, attachmentID string) {
	t.Helper()
	ctx := context.Background()
	serverID = seedServer(t)
	volumeID = uuid.New().String()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, bootable, attached_to_instance_id, volume_type, created_at, updated_at)
		 VALUES ($1, 'att-vol', $2, $3, 1, 'in-use', true, $4, '__DEFAULT__', now(), now())`,
		volumeID, seededProjectID, seededUserID, serverID); err != nil {
		t.Fatalf("seeding the volume: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volumeID) })
	if err := testPool.QueryRow(ctx,
		`INSERT INTO volume_attachments (volume_id, instance_id, device, delete_on_termination, attached_at)
		 VALUES ($1, $2, $3, true, now()) RETURNING id::text`,
		volumeID, serverID, device).Scan(&attachmentID); err != nil {
		t.Fatalf("seeding the attachment: %v", err)
	}
	return serverID, volumeID, attachmentID
}

// seedServer records a server in the seeded project.
func seedServer(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('att-server', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1) RETURNING id::text`,
		seededProjectID, seededUserID).Scan(&id); err != nil {
		t.Fatalf("seeding a server: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, id) })
	return id
}

type volumeEnvelope struct {
	Volume struct {
		ID          string           `json:"id"`
		Status      string           `json:"status"`
		Attachments []map[string]any `json:"attachments"`
	} `json:"volume"`
}

func TestVolumeAttachmentsAreReportedAsUpstream(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	serverID, volumeID, attachmentID := seedAttachedVolume(t, "/dev/vda")

	want := map[string]any{
		"id":            volumeID,
		"attachment_id": attachmentID,
		"volume_id":     volumeID,
		"server_id":     serverID,
		"host_name":     nil,
		"device":        "/dev/vda",
	}
	check := func(where string, attachments []map[string]any) {
		t.Helper()
		if len(attachments) != 1 {
			t.Fatalf("%s: %d attachments, want 1: %v", where, len(attachments), attachments)
		}
		got := attachments[0]
		for k, v := range want {
			if gv, ok := got[k]; !ok || gv != v {
				t.Errorf("%s: attachment[%q] = %v (present %v), want %v", where, k, gv, ok, v)
			}
		}
		if s, _ := got["attached_at"].(string); s == "" {
			t.Errorf("%s: attached_at = %v, want a timestamp", where, got["attached_at"])
		}
	}

	code, body := doReq(r, http.MethodGet, "/v3/volumes/"+volumeID, "", cinderMemberHdr())
	if code != http.StatusOK {
		t.Fatalf("show = %d %s", code, body)
	}
	var shown volumeEnvelope
	if err := json.Unmarshal([]byte(body), &shown); err != nil {
		t.Fatalf("decoding show: %v", err)
	}
	check("show", shown.Volume.Attachments)

	code, body = doReq(r, http.MethodGet, "/v3/volumes/detail", "", cinderMemberHdr())
	if code != http.StatusOK {
		t.Fatalf("detail = %d %s", code, body)
	}
	var listed struct {
		Volumes []struct {
			ID          string           `json:"id"`
			Attachments []map[string]any `json:"attachments"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	found := false
	for _, v := range listed.Volumes {
		if v.ID == volumeID {
			found = true
			check("detail", v.Attachments)
		}
	}
	if !found {
		t.Fatalf("volume %s missing from detail", volumeID)
	}
}

// os-attach records where the volume is attached, so it reads back like any other
// attachment; os-detach and os-force_detach remove it.
func TestAttachActionRecordsTheMountpoint(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	serverID := seedServer(t)

	code, body := doReq(r, http.MethodPost, "/v3/volumes", `{"volume":{"name":"att-action","size":1}}`, cinderMemberHdr())
	if code != http.StatusAccepted {
		t.Fatalf("create volume = %d %s", code, body)
	}
	var created volumeEnvelope
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Volume.ID == "" {
		t.Fatalf("no volume id in %s (%v)", body, err)
	}
	volumeID := created.Volume.ID
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volumeID) })

	action := func(id, payload string) (int, string) {
		return doReq(r, http.MethodPost, "/v3/volumes/"+id+"/action", payload, cinderMemberHdr())
	}
	attach := func(instance, mountpoint string) string {
		return fmt.Sprintf(`{"os-attach":{"instance_uuid":%q,"mountpoint":%q}}`, instance, mountpoint)
	}
	show := func() volumeEnvelope {
		code, body := doReq(r, http.MethodGet, "/v3/volumes/"+volumeID, "", cinderMemberHdr())
		if code != http.StatusOK {
			t.Fatalf("show = %d %s", code, body)
		}
		var v volumeEnvelope
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("decoding show: %v", err)
		}
		return v
	}
	rows := func() int {
		var n int
		if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM volume_attachments WHERE volume_id = $1`, volumeID).Scan(&n); err != nil {
			t.Fatalf("counting attachments: %v", err)
		}
		return n
	}

	// Refusals, none of which records anything.
	var foreignServer string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO projects (name, description) VALUES ($1, 'cinder test fixture') RETURNING id::text`,
		"att-"+uuid.New().String()[:8]).Scan(&foreignServer); err != nil {
		t.Fatalf("creating another project: %v", err)
	}
	foreignProject := foreignServer
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('att-foreign', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1) RETURNING id::text`,
		foreignProject, seededUserID).Scan(&foreignServer); err != nil {
		t.Fatalf("seeding another project's server: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, foreignServer)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, foreignProject)
	})
	for _, tc := range []struct {
		name, id, payload string
		want              int
	}{
		{"no mountpoint", volumeID, fmt.Sprintf(`{"os-attach":{"instance_uuid":%q}}`, serverID), http.StatusBadRequest},
		{"a malformed server id", volumeID, attach("not-a-uuid", "/dev/vdc"), http.StatusBadRequest},
		{"another project's server", volumeID, attach(foreignServer, "/dev/vdc"), http.StatusBadRequest},
		{"an unknown volume", uuid.New().String(), attach(serverID, "/dev/vdc"), http.StatusNotFound},
	} {
		if code, body := action(tc.id, tc.payload); code != tc.want {
			t.Errorf("os-attach with %s = %d %s; want %d", tc.name, code, body, tc.want)
		}
	}
	if n := rows(); n != 0 {
		t.Fatalf("%d attachment rows after refused attaches; want 0", n)
	}

	if code, body := action(volumeID, attach(serverID, "/dev/vdc")); code != http.StatusAccepted {
		t.Fatalf("os-attach = %d %s; want 202", code, body)
	}
	v := show()
	if v.Volume.Status != "in-use" || len(v.Volume.Attachments) != 1 ||
		v.Volume.Attachments[0]["device"] != "/dev/vdc" || v.Volume.Attachments[0]["server_id"] != serverID {
		t.Fatalf("after os-attach: status %q, attachments %v; want in-use at /dev/vdc on %s", v.Volume.Status, v.Volume.Attachments, serverID)
	}
	if code, body := action(volumeID, attach(serverID, "/dev/vdd")); code != http.StatusBadRequest {
		t.Errorf("os-attach of an attached volume = %d %s; want 400", code, body)
	}

	if code, body := action(volumeID, `{"os-detach":{}}`); code != http.StatusAccepted {
		t.Fatalf("os-detach = %d %s; want 202", code, body)
	}
	if v := show(); v.Volume.Status != "available" || len(v.Volume.Attachments) != 0 || rows() != 0 {
		t.Errorf("after os-detach: status %q, attachments %v, %d rows; want available and none", v.Volume.Status, v.Volume.Attachments, rows())
	}
	if code, body := action(uuid.New().String(), `{"os-detach":{}}`); code != http.StatusNotFound {
		t.Errorf("os-detach of an unknown volume = %d %s; want 404", code, body)
	}

	if code, body := action(volumeID, attach(serverID, "/dev/vdc")); code != http.StatusAccepted {
		t.Fatalf("re-attach = %d %s", code, body)
	}
	if code, body := action(volumeID, `{"os-force_detach":{}}`); code != http.StatusAccepted {
		t.Fatalf("os-force_detach = %d %s; want 202", code, body)
	}
	if v := show(); v.Volume.Status != "available" || len(v.Volume.Attachments) != 0 || rows() != 0 {
		t.Errorf("after os-force_detach: status %q, attachments %v, %d rows; want available and none", v.Volume.Status, v.Volume.Attachments, rows())
	}
}
