package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Marker paging over a NON-TOTAL order (branch review, third-party finding).
//
// The listing ordered by `created_at DESC` alone and resolved a marker to `created_at < $n`. Neither
// half is safe on its own:
//
//   - `created_at` is not unique. Images uploaded by a script, a seed, or one Terraform apply share
//     it to the microsecond, and rows sharing the sort key have no defined relative order — so two
//     identical requests can return them in different orders, and a page boundary can land anywhere
//     among them.
//   - `created_at < marker` then EXCLUDES every row sharing the marker's timestamp, not just the
//     rows before it. Walking the pages silently drops the marker's tied siblings.
//
// Nova hit exactly this in #549 and fixed it with a composite keyset over (created_at, id) matching
// a composite ORDER BY. Glance and Cinder kept the broken form.
//
// Seeding rows with an IDENTICAL created_at is what makes it observable; with distinct timestamps
// both the old and the new code pass, which is why it survived this long.
func TestImageMarkerPagingDoesNotDropTiedRows(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newGlanceTestRouter(t)

	// Five images sharing one timestamp — the tie the old predicate could not page through.
	shared := time.Now().UTC().Truncate(time.Microsecond)
	ids := make(map[string]bool, 5)
	for i := range 5 {
		id := uuid.NewString()
		ids[id] = true
		if _, err := testPool.Exec(ctx, `
			INSERT INTO images (id, name, project_id, status, visibility, disk_format, container_format, created_at, updated_at)
			VALUES ($1, $2, $3, 'active', 'private', 'qcow2', 'bare', $4, $4)
		`, id, "pagetie-"+id[:8], seededProjectID, shared); err != nil {
			t.Fatalf("seed image %d: %v", i, err)
		}
		t.Cleanup(func() {
			testPool.Exec(ctx, "DELETE FROM images WHERE id = $1", id) //nolint:errcheck // best-effort
		})
	}

	// Walk one row at a time. Every seeded image must be seen exactly once.
	seen := map[string]int{}
	marker := ""
	for page := 0; page < 12; page++ {
		path := "/images?limit=1"
		if marker != "" {
			path += "&marker=" + marker
		}
		code, body := doReq(r, http.MethodGet, path, hdr(seededProjectID, "member"))
		if code != http.StatusOK {
			t.Fatalf("page %d: code=%d body=%s", page, code, body)
		}
		var resp struct {
			Images []struct {
				ID string `json:"id"`
			} `json:"images"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("page %d: parse: %v", page, err)
		}
		if len(resp.Images) == 0 {
			break
		}
		for _, img := range resp.Images {
			if ids[img.ID] {
				seen[img.ID]++
			}
		}
		marker = resp.Images[len(resp.Images)-1].ID
	}

	for id := range ids {
		switch seen[id] {
		case 1: // exactly once, as a page walk must
		case 0:
			t.Errorf("image %s was SKIPPED by the page walk; a marker on a non-unique sort key "+
				"excludes every row sharing its timestamp, not just the ones before it", id[:8])
		default:
			t.Errorf("image %s was returned %d times by the page walk; the boundary is not stable", id[:8], seen[id])
		}
	}
}

// TestImageMarkerResolvesOnlyAnImageTheCallerMayRead — the marker is resolved as a show resolves an
// image (common.ImageVisibleSQL), as upstream resolves it with _image_get, and one that does not
// resolve is upstream's 400. It was read with no visibility at all, so another project's private
// image narrowed the listing where an absent id did not — telling the caller it exists, and roughly
// when it was created.
func TestImageMarkerResolvesOnlyAnImageTheCallerMayRead(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	f := newGlanceVisibilityFixture(t)

	for _, tc := range []struct {
		name, marker string
		want         int
	}{
		{"another project's private image", f.ids["foreign-private"], http.StatusBadRequest},
		{"an id that names no image", uuid.NewString(), http.StatusBadRequest},
		{"the caller's own image", f.ids["own-private"], http.StatusOK},
		{"another project's community image, readable though not listed", f.ids["foreign-community"], http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := doReq(r, http.MethodGet, "/images?limit=5&marker="+tc.marker, hdr(f.caller, "member")); code != tc.want {
				t.Errorf("code=%d body=%s, want %d", code, body, tc.want)
			}
		})
	}
}
