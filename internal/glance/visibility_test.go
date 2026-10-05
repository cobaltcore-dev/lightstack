package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Glance's own reads follow upstream's visibility rules (SPEC-020 FR-026).
//
// Show and download use the rule for reading one image (glance/db/utils.py
// is_image_visible): an administrator reads every image; anyone reads a public or community
// image, or one with no owner; the owner reads its own; a project a shared image was shared
// with reads it, whatever the membership's status. The list is narrower
// (glance/db/sqlalchemy/api.py _select_images_query and _image_get_all): by default it holds
// the caller's own images, public ones, and shared ones whose membership is accepted, and
// leaves out other projects' community images; ?visibility= and ?member_status= widen or
// narrow it. Lightstack used `visibility = 'public' OR project_id = caller` for all three,
// so sharing an image had no effect and community images could not be read at all.

type glanceVisibilityFixture struct {
	caller, owner string
	ids           map[string]string // fixture name -> image id
}

func newGlanceVisibilityFixture(t *testing.T) glanceVisibilityFixture {
	t.Helper()
	ctx := context.Background()
	project := func(prefix string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO projects (name, description) VALUES ($1, 'glance test fixture') RETURNING id::text`,
			prefix+"-"+uuid.New().String()[:8]).Scan(&id); err != nil {
			t.Fatalf("creating project: %v", err)
		}
		return id
	}
	f := glanceVisibilityFixture{caller: project("gvis-caller"), owner: project("gvis-owner"), ids: map[string]string{}}
	image := func(name, visibility, owner, status string) {
		var ownerID any
		if owner != "" {
			ownerID = owner
		}
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
			 VALUES ($1, $2, $3, $4, 'raw', 'bare') RETURNING id::text`,
			"gvis-"+name, ownerID, status, visibility).Scan(&id); err != nil {
			t.Fatalf("seeding image %s: %v", name, err)
		}
		f.ids[name] = id
	}
	member := func(name, memberID, status string) {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO image_members (image_id, member_id, status) VALUES ($1, $2, $3)`,
			f.ids[name], memberID, status); err != nil {
			t.Fatalf("adding member to %s: %v", name, err)
		}
	}
	image("own-private", "private", f.caller, "active")
	image("own-community", "community", f.caller, "active")
	image("own-queued", "private", f.caller, "queued")
	image("foreign-private", "private", f.owner, "active")
	image("foreign-public", "public", f.owner, "active")
	image("foreign-community", "community", f.owner, "active")
	image("ownerless", "private", "", "active")
	image("shared-accepted", "shared", f.owner, "active")
	member("shared-accepted", f.caller, "accepted")
	image("shared-pending", "shared", f.owner, "active")
	member("shared-pending", f.caller, "pending")
	image("shared-rejected", "shared", f.owner, "active")
	member("shared-rejected", strings.ReplaceAll(f.caller, "-", ""), "rejected")
	image("shared-elsewhere", "shared", f.owner, "active")
	member("shared-elsewhere", uuid.New().String(), "accepted")
	image("private-stray-member", "private", f.owner, "active")
	member("private-stray-member", f.caller, "accepted")

	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range f.ids {
			_, _ = testPool.Exec(ctx, `DELETE FROM image_members WHERE image_id = $1`, id)
			_, _ = testPool.Exec(ctx, `DELETE FROM images WHERE id = $1`, id)
		}
		_, _ = testPool.Exec(ctx, `DELETE FROM projects WHERE id IN ($1, $2)`, f.caller, f.owner)
	})
	return f
}

// names maps image ids in a list response back to fixture names, ignoring other images.
func (f glanceVisibilityFixture) names(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Images []struct {
			ID string `json:"id"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decoding list %s: %v", body, err)
	}
	byID := map[string]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	var got []string
	for _, img := range resp.Images {
		if name, ok := byID[img.ID]; ok {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	return got
}

func TestImageShowFollowsGlanceVisibility(t *testing.T) {
	requireDB(t)
	f := newGlanceVisibilityFixture(t)
	r := newGlanceTestRouter(t)

	visible := map[string]bool{
		"own-private": true, "own-community": true, "own-queued": true,
		"foreign-public": true, "foreign-community": true, "ownerless": true,
		"shared-accepted": true, "shared-pending": true, "shared-rejected": true,
	}
	for name, id := range f.ids {
		code, body := doReq(r, http.MethodGet, "/images/"+id, hdr(f.caller, "member"))
		want := http.StatusNotFound
		if visible[name] {
			want = http.StatusOK
		}
		if code != want {
			t.Errorf("show %s as a member = %d %s; want %d", name, code, body, want)
		}
		if code, body := doReq(r, http.MethodGet, "/images/"+id, hdr(f.caller, "admin")); code != http.StatusOK {
			t.Errorf("show %s as an administrator = %d %s; want 200", name, code, body)
		}
	}
}

func TestImageListFollowsGlanceVisibility(t *testing.T) {
	requireDB(t)
	f := newGlanceVisibilityFixture(t)
	r := newGlanceTestRouter(t)

	sorted := func(names ...string) []string { sort.Strings(names); return names }
	for _, tc := range []struct {
		query string
		roles string
		want  []string
	}{
		{"", "member", sorted("own-private", "own-community", "own-queued", "foreign-public", "shared-accepted")},
		{"visibility=community", "member", sorted("own-community", "foreign-community")},
		{"visibility=shared", "member", sorted("shared-accepted")},
		{"visibility=shared&member_status=pending", "member", sorted("shared-pending")},
		{"visibility=shared&member_status=all", "member", sorted("shared-accepted", "shared-pending", "shared-rejected")},
		{"visibility=all", "member", sorted("own-private", "own-community", "own-queued", "foreign-public", "foreign-community", "shared-accepted")},
		{"status=active", "member", sorted("own-private", "own-community", "foreign-public", "shared-accepted")},
		{"status=active&visibility=community", "member", sorted("own-community", "foreign-community")},
		{"", "admin", sorted("own-private", "own-community", "own-queued", "foreign-private", "foreign-public",
			"ownerless", "shared-accepted", "shared-pending", "shared-rejected", "shared-elsewhere", "private-stray-member")},
	} {
		code, body := doReq(r, http.MethodGet, "/images?"+tc.query, hdr(f.caller, tc.roles))
		if code != http.StatusOK {
			t.Fatalf("list ?%s as %s = %d %s", tc.query, tc.roles, code, body)
		}
		if got := f.names(t, body); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("list ?%s as %s = %v; want %v", tc.query, tc.roles, got, tc.want)
		}
	}

	for query, want := range map[string]string{
		"visibility=everything": "Invalid visibility value: everything",
		"member_status=maybe":   "Invalid status: maybe",
	} {
		code, body := doReq(r, http.MethodGet, "/images?"+query, hdr(f.caller, "member"))
		if code != http.StatusBadRequest || !strings.Contains(body, url.QueryEscape(want)) && !strings.Contains(body, want) {
			t.Errorf("list ?%s = %d %s; want 400 %q", query, code, body, want)
		}
	}
}

func TestImageDownloadFollowsGlanceVisibility(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newGlanceTestRouter(t)
	f := newGlanceVisibilityFixture(t)

	// An image with data, made through the API so the store holds it, then shared with
	// the caller.
	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"gvis-download","disk_format":"raw","container_format":"bare"}`, hdr(f.owner, "member"))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		t.Fatalf("no image id in %s (%v)", body, err)
	}
	f.ids["download"] = created.ID
	upload := httptest.NewRequest(http.MethodPut, "/images/"+created.ID+"/file", strings.NewReader("image-bytes"))
	upload.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range hdr(f.owner, "member") {
		upload.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, upload)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s", w.Code, w.Body.String())
	}
	if _, err := testPool.Exec(ctx, `UPDATE images SET visibility = 'shared' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("sharing the image: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO image_members (image_id, member_id, status) VALUES ($1, $2, 'accepted')`, created.ID, f.caller); err != nil {
		t.Fatalf("adding the member: %v", err)
	}

	// The stub store hands back zeros of the uploaded length.
	if code, body := doReq(r, http.MethodGet, "/images/"+created.ID+"/file", hdr(f.caller, "member")); code != http.StatusOK || len(body) != len("image-bytes") {
		t.Errorf("download as a member of the share = %d %q; want 200 with the image's %d bytes", code, body, len("image-bytes"))
	}
	if code, _ := doReq(r, http.MethodGet, "/images/"+created.ID+"/file", hdr(uuid.New().String(), "member")); code != http.StatusNotFound {
		t.Errorf("download by a project it is not shared with = %d; want 404", code)
	}
}
