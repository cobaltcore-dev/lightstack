package nova

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// A server response must always carry an "image" member: an {id} object for an
// image-booted server, and "" for a boot-from-volume server. Horizon's instance detail
// view reads server.image unconditionally, so omitting it (the old single-server bug)
// made novaclient raise "AttributeError: image".
func TestServerImageField(t *testing.T) {
	img := serverImageField(sql.NullString{String: "img-1", Valid: true})
	m, ok := img.(gin.H)
	if !ok || m["id"] != "img-1" {
		t.Errorf("image-boot: serverImageField = %#v, want gin.H{id: img-1}", img)
	}
	if got := serverImageField(sql.NullString{}); got != "" {
		t.Errorf("boot-from-volume: serverImageField = %#v, want \"\" (present, empty)", got)
	}
}

// The same through the handlers: GET /servers/{id} and /servers/detail both carry "image"
// for a server booted from an image and for one without (image_id NULL, as a
// boot-from-volume server has). The single-server handler used to drop the key in the
// second case, which is what broke Horizon's instance detail page.
func TestServerResponsesCarryImage(t *testing.T) {
	requireDB(t)
	var imageID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('imgfield', $1, 'active', 'private', 'raw', 'bare') RETURNING id::text`,
		seededProjectID).Scan(&imageID); err != nil {
		t.Fatalf("seeding an image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, imageID)
	})
	r := newNovaTestRouter()

	// server id -> the image member its responses must carry
	want := map[string]any{
		createImageFieldServer(t, r, imageID): map[string]any{"id": imageID},
		createImageFieldServer(t, r, ""):      "",
	}
	hdr := testAuthHdr(seededProjectID, seededUserID)
	for id, image := range want {
		code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("show %s: code=%d body=%s", id, code, body)
		}
		assertServerImage(t, "show", serverBody(t, body), image)
	}

	code, body := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s", code, body)
	}
	listed := 0
	for _, s := range serversBody(t, body) {
		id, _ := s["id"].(string)
		if image, ok := want[id]; ok {
			listed++
			assertServerImage(t, "detail", s, image)
		}
	}
	if listed != len(want) {
		t.Errorf("detail listed %d of the %d servers", listed, len(want))
	}
}

// createImageFieldServer creates a server in the seeded project, booted from imageRef
// when it is not empty, and returns its id. The row is removed when the test ends.
func createImageFieldServer(t *testing.T, r *gin.Engine, imageRef string) string {
	t.Helper()
	name := "imgfield-" + uuid.New().String()[:8]
	cleanupServerNamed(t, name)
	server := map[string]any{"name": name, "flavorRef": "m1.small"}
	if imageRef != "" {
		server["imageRef"] = imageRef
	}
	req, err := json.Marshal(map[string]any{"server": server})
	if err != nil {
		t.Fatalf("encoding the create body: %v", err)
	}
	code, body := createServerAs(t, r, seededProjectID, seededUserID, string(req))
	if code != http.StatusAccepted {
		t.Fatalf("create %s: code=%d body=%s; want 202", name, code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)
	if id == "" {
		t.Fatalf("create %s: no id in body=%s", name, body)
	}
	return id
}

// assertServerImage checks that a server response carries an "image" member, with the
// given value.
func assertServerImage(t *testing.T, where string, server map[string]any, want any) {
	t.Helper()
	got, ok := server["image"]
	if !ok {
		t.Errorf("%s %v: no image member; want %#v", where, server["id"], want)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s %v: image = %#v; want %#v", where, server["id"], got, want)
	}
}
