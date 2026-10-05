package nova

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Image visibility at the compute API (SPEC-020 FR-026).
//
// A server may only be made from an image the caller can read, by Glance's rule: upstream
// Nova asks Glance with the caller's token, so another project's private image and an id
// that names no image are the same answer, 400 "Can not find requested image". Before,
// Lightstack used whatever id it was given: a project that knew the id of another
// project's private image could boot it, and its server then kept the owner from deleting
// the image.

// imageVisibilityFixture is an image owned by another project, plus that project.
type imageVisibilityFixture struct {
	ownerProject string
	private      string // the other project's private image
	public       string // the other project's public image
}

func newImageVisibilityFixture(t *testing.T) imageVisibilityFixture {
	t.Helper()
	ctx := context.Background()
	var f imageVisibilityFixture
	if err := testPool.QueryRow(ctx,
		`INSERT INTO projects (name, description) VALUES ($1, 'nova test fixture') RETURNING id::text`,
		"imgvis-"+uuid.New().String()[:8]).Scan(&f.ownerProject); err != nil {
		t.Fatalf("creating the owning project: %v", err)
	}
	image := func(visibility string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
			 VALUES ($1, $2, 'active', $3, 'raw', 'bare') RETURNING id::text`,
			"imgvis-"+visibility, f.ownerProject, visibility).Scan(&id); err != nil {
			t.Fatalf("seeding a %s image: %v", visibility, err)
		}
		return id
	}
	f.private = image("private")
	f.public = image("public")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM instances WHERE image_id IN ($1, $2)`, f.private, f.public)
		_, _ = testPool.Exec(ctx, `DELETE FROM images WHERE project_id = $1`, f.ownerProject)
		_, _ = testPool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, f.ownerProject)
	})
	return f
}

// memberHdr is a plain member of the seeded project; adminHdr holds the admin role.
func memberHdr() map[string]string {
	return map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
}

func adminHdr() map[string]string {
	return map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "admin"}
}

func TestServerCreateRefusesAnImageTheCallerCannotSee(t *testing.T) {
	requireDB(t)
	f := newImageVisibilityFixture(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	imageBoot := func(name, image string) string {
		return fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q}}`, name, image)
	}
	adoptedBDM := func(name, image string) string {
		return fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":"",
			"block_device_mapping_v2":[{"boot_index":0,"uuid":%q,"source_type":"image"}]}}`, name, image)
	}

	for _, tc := range []struct {
		name     string
		body     func(name, image string) string
		image    string
		hdr      map[string]string
		accepted bool
	}{
		{"imageRef: another project's private image", imageBoot, f.private, memberHdr(), false},
		{"imageRef: an id that names no image", imageBoot, uuid.New().String(), memberHdr(), false},
		{"imageRef: an id an administrator cannot find either", imageBoot, uuid.New().String(), adminHdr(), false},
		{"imageRef: a malformed id", imageBoot, "not-a-uuid", memberHdr(), false},
		{"new boot volume: another project's private image", func(name, image string) string { return wizardBody(name, image, 4, true) }, f.private, memberHdr(), false},
		{"adopted image mapping: another project's private image", adoptedBDM, f.private, memberHdr(), false},
		{"imageRef: another project's public image", imageBoot, f.public, memberHdr(), true},
		{"imageRef: another project's private image, as an administrator", imageBoot, f.private, adminHdr(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "imgvis-" + uuid.New().String()[:8]
			cleanupServerNamed(t, name)
			before := len(fake.calls)

			code, body := doReq(r, http.MethodPost, "/v2.1/servers", tc.body(name, tc.image), tc.hdr)
			if tc.accepted {
				if code != http.StatusAccepted {
					t.Fatalf("create = %d; want 202. body: %s", code, body)
				}
				return
			}
			if code != http.StatusBadRequest || !strings.Contains(body, "Can not find requested image") {
				t.Errorf("create = %d %s; want 400 \"Can not find requested image\"", code, body)
			}
			if n := countServersNamed(t, name); n != 0 {
				t.Errorf("%d instance rows for a refused create; want 0", n)
			}
			if got := len(fake.calls) - before; got != 0 {
				t.Errorf("%d volumes created for a refused create; want 0", got)
			}
		})
	}
}

func TestRebuildRefusesAnImageTheCallerCannotSee(t *testing.T) {
	requireDB(t)
	f := newImageVisibilityFixture(t)
	_, r := newBootVolumeService(t, nil)
	own := seededImageID(t)

	var serverID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, image_id, status, power_state)
		 VALUES ('imgvis-rebuild', $1, $2, (SELECT id FROM flavors LIMIT 1), $3, 'ACTIVE', 1)
		 RETURNING id::text`, seededProjectID, seededUserID, own).Scan(&serverID); err != nil {
		t.Fatalf("seeding the server: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, serverID)
	})

	rebuild := func(image string) (int, string) {
		return doReq(r, http.MethodPost, "/v2.1/servers/"+serverID+"/action",
			fmt.Sprintf(`{"rebuild":{"imageRef":%q}}`, image), memberHdr())
	}
	imageOf := func() string {
		var id string
		if err := testPool.QueryRow(t.Context(),
			`SELECT COALESCE(image_id::text, '') FROM instances WHERE id = $1`, serverID).Scan(&id); err != nil {
			t.Fatalf("reading the server's image: %v", err)
		}
		return id
	}

	for _, image := range []string{f.private, uuid.New().String()} {
		code, body := rebuild(image)
		if code != http.StatusBadRequest || !strings.Contains(body, "Cannot find image for rebuild") {
			t.Errorf("rebuild with %s = %d %s; want 400 \"Cannot find image for rebuild\"", image, code, body)
		}
		if got := imageOf(); got != own {
			t.Errorf("after a refused rebuild the server's image is %q; want it unchanged (%s)", got, own)
		}
	}

	if code, body := rebuild(f.public); code/100 != 2 {
		t.Errorf("rebuild with a public image = %d %s; want it accepted", code, body)
	}
}

// Only an active image can be booted: one whose data was never uploaded, or that was
// deactivated, is refused with upstream's 400 "Image <id> is not active.", on create
// (directly or through a new boot volume) and on rebuild.
func TestServerCreateRefusesAnImageThatIsNotActive(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	var queued string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('imgvis-queued', $1, 'queued', 'private', 'raw', 'bare') RETURNING id::text`,
		seededProjectID).Scan(&queued); err != nil {
		t.Fatalf("seeding a queued image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE image_id = $1`, queued)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, queued)
	})
	want := "Image " + queued + " is not active."

	for _, tc := range []struct {
		name string
		body func(name string) string
	}{
		{"imageRef", func(name string) string {
			return fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q}}`, name, queued)
		}},
		{"new boot volume", func(name string) string { return wizardBody(name, queued, 4, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "imgvis-" + uuid.New().String()[:8]
			cleanupServerNamed(t, name)
			before := len(fake.calls)
			code, body := doReq(r, http.MethodPost, "/v2.1/servers", tc.body(name), memberHdr())
			if code != http.StatusBadRequest || !strings.Contains(body, want) {
				t.Errorf("create = %d %s; want 400 %q", code, body, want)
			}
			if n := countServersNamed(t, name); n != 0 {
				t.Errorf("%d instance rows for a refused create; want 0", n)
			}
			if got := len(fake.calls) - before; got != 0 {
				t.Errorf("%d volumes created for a refused create; want 0", got)
			}
		})
	}

	var serverID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('imgvis-rebuild-queued', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
		 RETURNING id::text`, seededProjectID, seededUserID).Scan(&serverID); err != nil {
		t.Fatalf("seeding the server: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, serverID)
	})
	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+serverID+"/action",
		fmt.Sprintf(`{"rebuild":{"imageRef":%q}}`, queued), memberHdr())
	if code != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Errorf("rebuild = %d %s; want 400 %q", code, body, want)
	}
}
