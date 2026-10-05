package nova

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Server show and detail list the server's volumes (SPEC-020 FR-027), as upstream does
// under os-extended-volumes:volumes_attached: {id, delete_on_termination} per attached
// volume, the boot volume included, and an empty list when there is none. Lightstack
// omitted the key, so `openstack server show` listed no volumes even for a server that
// boots from one.
func TestServerShowListsAttachedVolumes(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	server := func(name string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
			 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1) RETURNING id::text`,
			name, seededProjectID, seededUserID).Scan(&id); err != nil {
			t.Fatalf("seeding server %s: %v", name, err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, id) })
		return id
	}
	attach := func(serverID, device string, dot bool, order int) string {
		volumeID := uuid.New().String()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, bootable, attached_to_instance_id, volume_type, created_at, updated_at)
			 VALUES ($1, 'va-vol', $2, $3, 1, 'in-use', true, $4, '__DEFAULT__', now(), now())`,
			volumeID, seededProjectID, seededUserID, serverID); err != nil {
			t.Fatalf("seeding a volume: %v", err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volumeID) })
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volume_attachments (volume_id, instance_id, device, delete_on_termination, attached_at)
			 VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`,
			volumeID, serverID, device, dot, order); err != nil {
			t.Fatalf("seeding an attachment: %v", err)
		}
		return volumeID
	}

	withVolumes := server("va-with-volumes")
	root := attach(withVolumes, "/dev/vda", true, 0)
	data := attach(withVolumes, "/dev/vdb", false, 1)
	without := server("va-without-volumes")

	want := map[string][]any{
		withVolumes: {
			map[string]any{"id": root, "delete_on_termination": true},
			map[string]any{"id": data, "delete_on_termination": false},
		},
		without: {},
	}

	for id, w := range want {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("show %s = %d %s", id, code, body)
		}
		var shown struct {
			Server map[string]any `json:"server"`
		}
		if err := json.Unmarshal([]byte(body), &shown); err != nil {
			t.Fatalf("decoding show: %v", err)
		}
		got, ok := shown.Server["os-extended-volumes:volumes_attached"]
		if !ok || !reflect.DeepEqual(got, w) {
			t.Errorf("show %s: volumes_attached = %v (present %v), want %v", id, got, ok, w)
		}
	}

	code, body := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("detail = %d %s", code, body)
	}
	var listed struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	seen := 0
	for _, s := range listed.Servers {
		w, ok := want[s["id"].(string)]
		if !ok {
			continue
		}
		seen++
		if got := s["os-extended-volumes:volumes_attached"]; !reflect.DeepEqual(got, w) {
			t.Errorf("detail %s: volumes_attached = %v, want %v", s["id"], got, w)
		}
	}
	if seen != len(want) {
		t.Errorf("detail listed %d of the %d seeded servers", seen, len(want))
	}
}
