package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Image visibility at the block-storage API (SPEC-020 FR-026).
//
// A volume may only be made from, or re-imaged with, an image the caller can read, by
// Glance's rule. Upstream Cinder asks Glance with the caller's token: volume create answers
// 400 "Invalid image identifier or unable to access requested image." and os-reimage 404
// "Image <id> could not be found.", the same for another project's private image as for an
// id that names no image.

type cinderImageFixture struct {
	ownerProject    string
	private, public string
}

func newCinderImageFixture(t *testing.T) cinderImageFixture {
	t.Helper()
	ctx := context.Background()
	var f cinderImageFixture
	if err := testPool.QueryRow(ctx,
		`INSERT INTO projects (name, description) VALUES ($1, 'cinder test fixture') RETURNING id::text`,
		"imgvis-"+uuid.New().String()[:8]).Scan(&f.ownerProject); err != nil {
		t.Fatalf("creating the owning project: %v", err)
	}
	image := func(visibility string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO images (name, project_id, status, visibility) VALUES ($1, $2, 'active', $3) RETURNING id::text`,
			"imgvis-"+visibility, f.ownerProject, visibility).Scan(&id); err != nil {
			t.Fatalf("seeding a %s image: %v", visibility, err)
		}
		return id
	}
	f.private = image("private")
	f.public = image("public")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM volumes WHERE source_image_id IN ($1, $2)`, f.private, f.public)
		_, _ = testPool.Exec(ctx, `DELETE FROM images WHERE project_id = $1`, f.ownerProject)
		_, _ = testPool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, f.ownerProject)
	})
	return f
}

func cinderMemberHdr() map[string]string {
	return map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
}

func TestVolumeCreateRefusesAnImageTheCallerCannotSee(t *testing.T) {
	requireDB(t)
	f := newCinderImageFixture(t)
	r := newCinderTestRouter()

	for _, tc := range []struct {
		name     string
		image    string
		hdr      map[string]string
		accepted bool
	}{
		{"another project's private image", f.private, cinderMemberHdr(), false},
		{"an id that names no image", uuid.New().String(), cinderMemberHdr(), false},
		{"a malformed id", "not-a-uuid", cinderMemberHdr(), false},
		{"another project's public image", f.public, cinderMemberHdr(), true},
		{"another project's private image, as an administrator", f.private, cinderAuthHdr(seededProjectID, seededUserID), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "imgvis-" + uuid.New().String()[:8]
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE name = $1`, name)
			})
			code, body := doReq(r, http.MethodPost, "/v3/volumes",
				fmt.Sprintf(`{"volume":{"name":%q,"size":1,"imageRef":%q}}`, name, tc.image), tc.hdr)
			if tc.accepted {
				if code != http.StatusAccepted {
					t.Fatalf("create = %d %s; want 202", code, body)
				}
				return
			}
			if code != http.StatusBadRequest || !strings.Contains(body, "Invalid image identifier or unable to access requested image.") {
				t.Errorf("create = %d %s; want upstream's 400", code, body)
			}
			var n int
			if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM volumes WHERE name = $1`, name).Scan(&n); err != nil {
				t.Fatalf("counting volumes: %v", err)
			}
			if n != 0 {
				t.Errorf("%d volumes recorded for a refused create; want 0", n)
			}
		})
	}
}

func TestReimageRefusesAnImageTheCallerCannotSee(t *testing.T) {
	requireDB(t)
	f := newCinderImageFixture(t)
	r := newCinderTestRouter()

	code, body := doReq(r, http.MethodPost, "/v3/volumes", `{"volume":{"name":"imgvis-reimage","size":1}}`, cinderMemberHdr())
	if code != http.StatusAccepted {
		t.Fatalf("create volume = %d %s", code, body)
	}
	var created struct {
		Volume struct {
			ID string `json:"id"`
		} `json:"volume"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Volume.ID == "" {
		t.Fatalf("no volume id in %s (%v)", body, err)
	}
	volumeID := created.Volume.ID
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_metadata WHERE volume_id = $1`, volumeID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volumeID)
	})

	reimage := func(image string) (int, string) {
		return doReq(r, http.MethodPost, "/v3/volumes/"+volumeID+"/action",
			fmt.Sprintf(`{"os-reimage":{"image_id":%q}}`, image), cinderMemberHdr())
	}
	recorded := func() string {
		var v string
		_ = testPool.QueryRow(t.Context(),
			`SELECT meta_value FROM volume_metadata WHERE volume_id = $1 AND meta_key = 'volume_image_id'`, volumeID).Scan(&v)
		return v
	}

	for _, image := range []string{f.private, uuid.New().String()} {
		code, body := reimage(image)
		want := "Image " + image + " could not be found."
		if code != http.StatusNotFound || !strings.Contains(body, want) {
			t.Errorf("reimage with %s = %d %s; want upstream's 404 %q", image, code, body, want)
		}
		if got := recorded(); got != "" {
			t.Errorf("a refused reimage recorded image %q on the volume", got)
		}
	}

	if code, body := reimage(f.public); code != http.StatusAccepted {
		t.Errorf("reimage with a public image = %d %s; want 202", code, body)
	}
}

// Only an active image can be copied into a volume: upstream's check_image_metadata answers
// 400 "Invalid input received: Image <id> is not active." for volume create and os-reimage.
func TestVolumeFromAnImageThatIsNotActiveIsRefused(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	var queued string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO images (name, project_id, status, visibility) VALUES ('imgvis-queued', $1, 'queued', 'private') RETURNING id::text`,
		seededProjectID).Scan(&queued); err != nil {
		t.Fatalf("seeding a queued image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE source_image_id = $1`, queued)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, queued)
	})
	want := "Invalid input received: Image " + queued + " is not active."

	code, body := doReq(r, http.MethodPost, "/v3/volumes",
		fmt.Sprintf(`{"volume":{"name":"imgvis-queued-vol","size":1,"imageRef":%q}}`, queued), cinderMemberHdr())
	if code != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Errorf("volume create = %d %s; want 400 %q", code, body, want)
	}

	code, body = doReq(r, http.MethodPost, "/v3/volumes", `{"volume":{"name":"imgvis-reimage-queued","size":1}}`, cinderMemberHdr())
	if code != http.StatusAccepted {
		t.Fatalf("create volume = %d %s", code, body)
	}
	var created struct {
		Volume struct {
			ID string `json:"id"`
		} `json:"volume"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Volume.ID == "" {
		t.Fatalf("no volume id in %s (%v)", body, err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_metadata WHERE volume_id = $1`, created.Volume.ID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, created.Volume.ID)
	})
	code, body = doReq(r, http.MethodPost, "/v3/volumes/"+created.Volume.ID+"/action",
		fmt.Sprintf(`{"os-reimage":{"image_id":%q}}`, queued), cinderMemberHdr())
	if code != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Errorf("os-reimage = %d %s; want 400 %q", code, body, want)
	}
}
