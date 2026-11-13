package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// insertListFilterImage seeds one image row the filter tests can see. The
// caller owns cleanup.
func insertListFilterImage(t *testing.T, name, diskFormat string, sizeBytes int64, tags ...string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility, disk_format, container_format, size_bytes)
		 VALUES ($1, $2, $3, 'active', 'public', $4, 'bare', $5)`,
		id, name, seededProjectID, diskFormat, sizeBytes); err != nil {
		t.Fatalf("insert image %s: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })
	for _, tag := range tags {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO image_tags (image_id, tag) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			id, tag); err != nil {
			t.Fatalf("insert tag %s: %v", tag, err)
		}
	}
	return id
}

// listedNames parses a /v2/images response and returns the names in it.
func listedNames(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Images []struct {
			Name string `json:"name"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("parse list response %q: %v", body, err)
	}
	names := make([]string, 0, len(resp.Images))
	for _, img := range resp.Images {
		names = append(names, img.Name)
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestListImagesNameFilter — the exact-match `name` query parameter. This is
// the filter the unmodified terraform-provider-openstack data source relies
// on (images.ListOpts{Name} with no client-side fallback), so without it any
// server holding more than one image breaks `data "openstack_images_image_v2"`
// with "Your query returned more than one result" (SPEC-028 terraform
// conformance finding).
func TestListImagesNameFilter(t *testing.T) {
	requireDB(t)
	insertListFilterImage(t, "spec028-name-a", "raw", 1024)
	insertListFilterImage(t, "spec028-name-b", "raw", 2048)
	r := newGlanceTestRouter(t)

	code, body := doReq(r, http.MethodGet, "/images?name=spec028-name-a", hdr(seededProjectID, "admin"))
	if code != http.StatusOK {
		t.Fatalf("list by name: code=%d body=%s; want 200", code, body)
	}
	names := listedNames(t, body)
	if len(names) != 1 || names[0] != "spec028-name-a" {
		t.Errorf("name filter: got %v; want exactly [spec028-name-a]", names)
	}

	// A name that matches nothing must return an empty list, not everything.
	code, body = doReq(r, http.MethodGet, "/images?name=spec028-name-nope", hdr(seededProjectID, "admin"))
	if code != http.StatusOK {
		t.Fatalf("list by missing name: code=%d body=%s; want 200", code, body)
	}
	if names := listedNames(t, body); len(names) != 0 {
		t.Errorf("missing name filter: got %v; want empty", names)
	}
}

// TestListImagesPropertyFilters — the other standard Glance v2 list
// parameters (glance ALLOWED_PARAMS): disk_format, size_min/max, owner, tag.
// Silently ignoring them makes `openstack image list --tag X` /
// `--disk-format qcow2` return unfiltered results (SPEC-028 CLI conformance
// finding).
//
// The assertions are written for a SHARED server (other suites create images
// concurrently): each case asserts the target image is present, the
// non-matching sibling is absent, and — where the response carries the
// filtered field — that no returned image violates the predicate.
func TestListImagesPropertyFilters(t *testing.T) {
	requireDB(t)
	insertListFilterImage(t, "spec028-prop-raw", "raw", 54321, "spec028-tagged")
	insertListFilterImage(t, "spec028-prop-qcow2", "qcow2", 12345)
	r := newGlanceTestRouter(t)

	listImages := func(query string) []map[string]any {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/images?"+query, hdr(seededProjectID, "admin"))
		if code != http.StatusOK {
			t.Fatalf("list %s: code=%d body=%s; want 200", query, code, body)
		}
		var resp struct {
			Images []map[string]any `json:"images"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("parse list response: %v", err)
		}
		return resp.Images
	}
	names := func(images []map[string]any) []string {
		out := make([]string, 0, len(images))
		for _, img := range images {
			if n, _ := img["name"].(string); n != "" {
				out = append(out, n)
			}
		}
		return out
	}

	// disk_format: target present, sibling absent, nothing foreign leaks in
	// with the wrong format.
	if got := names(listImages("disk_format=raw")); !contains(got, "spec028-prop-raw") || contains(got, "spec028-prop-qcow2") {
		t.Errorf("disk_format=raw: got %v; want spec028-prop-raw present, spec028-prop-qcow2 absent", got)
	}
	for _, img := range listImages("disk_format=qcow2") {
		if df, _ := img["disk_format"].(string); df != "qcow2" {
			t.Errorf("disk_format=qcow2 returned image %v with disk_format %q", img["name"], df)
		}
	}

	// size bounds chosen to exclude both the sibling and the 64KiB fixtures
	// other suites create.
	if got := names(listImages("size_min=50000&size_max=60000")); len(got) != 1 || got[0] != "spec028-prop-raw" {
		t.Errorf("size 50000..60000: got %v; want exactly [spec028-prop-raw]", got)
	}

	// tag: tagged target present, untagged sibling absent; an unused tag
	// matches neither.
	if got := names(listImages("tag=spec028-tagged")); !contains(got, "spec028-prop-raw") || contains(got, "spec028-prop-qcow2") {
		t.Errorf("tag=spec028-tagged: got %v; want spec028-prop-raw present, spec028-prop-qcow2 absent", got)
	}
	if got := names(listImages("tag=spec028-unused")); contains(got, "spec028-prop-raw") || contains(got, "spec028-prop-qcow2") {
		t.Errorf("tag=spec028-unused: got %v; want neither spec028 image", got)
	}

	// owner: our images are owned by the seeded project; an owner that owns
	// nothing of ours must not list them.
	if got := names(listImages("owner=" + seededProjectID)); !contains(got, "spec028-prop-raw") {
		t.Errorf("owner=seeded: got %v; want spec028-prop-raw present", got)
	}
	if got := names(listImages("owner=ffffffff-ffff-ffff-ffff-ffffffffffff")); contains(got, "spec028-prop-raw") || contains(got, "spec028-prop-qcow2") {
		t.Errorf("owner=unknown: got %v; want neither spec028 image", got)
	}
}

// TestCreateImagePersistsTags — `POST /v2/images` accepts a `tags` array and
// echoes it in the 201 response, but the rows were never written: an
// immediate `GET /v2/images/{id}` returned `tags: []`. The create/echo/show
// round-trip must agree (SPEC-028 CLI conformance finding:
// `openstack image create --tag` silently drops the tag).
func TestCreateImagePersistsTags(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	ctx := context.Background()

	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"spec028-tags-img","disk_format":"raw","container_format":"bare","tags":["alpha","beta"]}`,
		hdr(seededProjectID, "member"))
	if code != http.StatusCreated {
		t.Fatalf("create image: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse create response: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", created.ID) })
	if len(created.Tags) != 2 {
		t.Errorf("create response tags: got %v; want [alpha beta]", created.Tags)
	}

	code, body = doReq(r, http.MethodGet, "/images/"+created.ID, hdr(seededProjectID, "member"))
	if code != http.StatusOK {
		t.Fatalf("show image: code=%d body=%s; want 200", code, body)
	}
	var shown struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(body), &shown); err != nil {
		t.Fatalf("parse show response: %v", err)
	}
	if !contains(shown.Tags, "alpha") || !contains(shown.Tags, "beta") {
		t.Errorf("show after create: tags %v; want [alpha beta] persisted", shown.Tags)
	}

	// Duplicate tags in one request must not violate image_tags' UNIQUE
	// constraint — the dedicated PUT uses ON CONFLICT DO NOTHING, and a
	// create request with repeats is legal input.
	code, _ = doReqBody(r, http.MethodPost, "/images",
		`{"name":"spec028-tags-dup","tags":["same","same"]}`,
		hdr(seededProjectID, "member"))
	if code != http.StatusCreated {
		t.Errorf("create image with duplicate tags: code=%d; want 201", code)
	}
}

// TestImageTagLengthValidated — upstream's image schema caps a tag at 255
// characters and answers 400; the image_tags column is VARCHAR(255), so
// without the check an over-long tag surfaced as a 500 after (create path)
// the image row was already committed (SPEC-028).
func TestImageTagLengthValidated(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	longTag := strings.Repeat("t", 256)

	// Create path: refused before anything is written.
	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"spec028-longtag","tags":["`+longTag+`"]}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("create with over-long tag: code=%d; want 400 (body=%s)", code, body)
	}

	// Dedicated PUT path: same refusal.
	id := insertListFilterImage(t, "spec028-longtag-put", "qcow2", 0)
	code, body = doReqBody(r, http.MethodPut, "/images/"+id+"/tags/"+longTag, "", hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("put over-long tag: code=%d; want 400 (body=%s)", code, body)
	}

	// A maximal tag stays legal on both paths.
	code, body = doReqBody(r, http.MethodPost, "/images",
		`{"name":"spec028-maxtag","tags":["`+strings.Repeat("t", 255)+`"]}`, hdr)
	if code != http.StatusCreated {
		t.Errorf("create with 255-char tag: code=%d; want 201 (body=%s)", code, body)
	} else {
		t.Cleanup(func() {
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(body), &created); err == nil && created.ID != "" {
				testPool.Exec(context.Background(),
					"DELETE FROM image_tags WHERE image_id=$1", created.ID)
				testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", created.ID)
			}
		})
	}
	code, _ = doReqBody(r, http.MethodPut, "/images/"+id+"/tags/"+strings.Repeat("t", 255), "", hdr)
	if code != http.StatusNoContent {
		t.Errorf("put 255-char tag: code=%d; want 204", code)
	}
}
