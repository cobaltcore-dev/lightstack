package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestImageWithUnsetFormatsStaysReadable — images.disk_format and container_format are
// nullable and upstream Glance reports an unset one as null (glance/api/v2/images.py: both
// are ['null', 'string']). Every Lightstack writer sets them, but a row without them — a
// hand-edited one, or another package's test fixture — must not break a read: scanning NULL
// into a plain string makes pgx end the whole result set, so one such public image turned
// every caller's image list into a 500.
func TestImageWithUnsetFormatsStaysReadable(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	ctx := context.Background()

	var caller, owner, imageID string
	for _, p := range []*string{&caller, &owner} {
		if err := testPool.QueryRow(ctx,
			`INSERT INTO projects (name, description) VALUES ($1, 'glance test fixture') RETURNING id::text`,
			"unset-formats-"+uuid.New().String()[:8]).Scan(p); err != nil {
			t.Fatalf("creating project: %v", err)
		}
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO images (name, project_id, status, visibility) VALUES ('unset-formats', $1, 'active', 'public')
		 RETURNING id::text`, owner).Scan(&imageID); err != nil {
		t.Fatalf("seeding an image without formats: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, imageID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id IN ($1, $2)`, caller, owner)
	})

	wantNullFormats := func(t *testing.T, how string, image map[string]any) {
		t.Helper()
		for _, field := range []string{"disk_format", "container_format"} {
			if v, ok := image[field]; !ok || v != nil {
				t.Errorf("%s: %s = %v (present=%v), want null", how, field, v, ok)
			}
		}
	}

	code, body := doReq(r, http.MethodGet, "/images", hdr(caller, "member"))
	if code != http.StatusOK {
		t.Errorf("list = %d %s; want 200", code, body)
	} else {
		var list struct{ Images []map[string]any }
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatalf("decoding list %s: %v", body, err)
		}
		var seen bool
		for _, img := range list.Images {
			if img["id"] == imageID {
				seen = true
				wantNullFormats(t, "list", img)
			}
		}
		if !seen {
			t.Errorf("the public image without formats is missing from the list: %s", body)
		}
	}

	code, body = doReq(r, http.MethodGet, "/images/"+imageID, hdr(caller, "member"))
	if code != http.StatusOK {
		t.Errorf("show = %d %s; want 200", code, body)
	} else {
		var img map[string]any
		if err := json.Unmarshal([]byte(body), &img); err != nil {
			t.Fatalf("decoding show %s: %v", body, err)
		}
		wantNullFormats(t, "show", img)
	}
}
