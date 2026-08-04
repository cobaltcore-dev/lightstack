package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
)

// A second project, so the scoping assertion has a foreign project to name in the URL.
const otherProjectID = "00000000-0000-0000-0000-0000000c0c0c"

func ensureOtherProject(t *testing.T) {
	t.Helper()
	_, err := database.DB.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled, domain_id)
		 VALUES ($1, 'spec022-cinder-other', 'spec022 fixture', true,
		         (SELECT id FROM domains WHERE name = 'Default'))
		 ON CONFLICT (id) DO NOTHING`, otherProjectID)
	if err != nil {
		t.Fatalf("create the other project: %v", err)
	}
}

func insertSpec022Volume(t *testing.T, project, name string) string {
	t.Helper()
	var id string
	err := database.DB.QueryRow(t.Context(),
		`INSERT INTO volumes (project_id, name, size_gb, status) VALUES ($1, $2, 1, 'available') RETURNING id`,
		project, name,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert volume %q for %s: %v", name, project, err)
	}
	t.Cleanup(func() {
		// context.Background(), not t.Context(): the test context is cancelled just
		// before Cleanup-registered functions run (testing.T.Context).
		if _, err := database.DB.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, id); err != nil {
			t.Logf("cleanup volume %s: %v", id, err)
		}
	})
	return id
}

// TestProjectScopedVolumeList is B2 (FR-014, FR-015): the project-in-path form of the
// volume collection answered 405 — POST was registered on it, GET was not — while the
// flat form worked. Both forms must return the same collection for the same caller.
func TestProjectScopedVolumeList(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	ensureOtherProject(t)
	volID := insertSpec022Volume(t, seededProjectID, "spec022-scoped")
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	idsFrom := func(path string) []string {
		t.Helper()
		code, resp := doReq(r, http.MethodGet, path, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", path, code, resp)
		}
		var got struct {
			Volumes []struct{ ID string } `json:"volumes"`
		}
		if err := json.Unmarshal([]byte(resp), &got); err != nil {
			t.Fatalf("GET %s: unmarshal %s: %v", path, resp, err)
		}
		ids := make([]string, 0, len(got.Volumes))
		for _, v := range got.Volumes {
			ids = append(ids, v.ID)
		}
		return ids
	}

	scoped := idsFrom("/v3/" + seededProjectID + "/volumes")
	flat := idsFrom("/v3/volumes")

	// SC-005: the two URL forms return the same collection.
	if len(scoped) != len(flat) {
		t.Fatalf("project-scoped list has %d volumes, flat list has %d — the forms disagree", len(scoped), len(flat))
	}
	found := false
	for _, id := range scoped {
		if id == volID {
			found = true
		}
	}
	if !found {
		t.Fatalf("project-scoped list %v does not contain the caller's volume %s", scoped, volID)
	}

	// FR-015: the path segment is decoration. Every handler in that group reads the
	// caller's project from the token, so naming a foreign project cannot widen what
	// the caller sees — and must not start doing so if someone "fixes" the segment.
	foreign := idsFrom("/v3/" + otherProjectID + "/volumes")
	if len(foreign) != len(scoped) {
		t.Fatalf("naming a foreign project in the path changed the result (%d vs %d volumes) — the segment must not widen scope",
			len(foreign), len(scoped))
	}

	// A non-identifier in the segment is equally inert: it is never parsed.
	if got := idsFrom("/v3/not-a-uuid/volumes"); len(got) != len(scoped) {
		t.Fatalf("a non-identifier project segment changed the result (%d vs %d volumes)", len(got), len(scoped))
	}
}

// TestProjectScopedVolumeListEmpty — a caller with no volumes gets an empty
// collection, not an error and not JSON null (which OpenStack clients fail on).
func TestProjectScopedVolumeListEmpty(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	ensureOtherProject(t)

	code, resp := doReq(r, http.MethodGet, "/v3/"+otherProjectID+"/volumes", "",
		cinderAuthHdr(otherProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("GET project-scoped volumes for an empty project: code=%d body=%s; want 200", code, resp)
	}
	var got struct {
		Volumes *[]json.RawMessage `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", resp, err)
	}
	if got.Volumes == nil {
		t.Fatalf("volumes must be an empty list, not null: %s", resp)
	}
	if len(*got.Volumes) != 0 {
		t.Fatalf("expected no volumes for a fresh project, got %d: %s", len(*got.Volumes), resp)
	}
}

// TestProjectScopedSubResourcesNotShadowed — FR-016. Each of these names sat beside a
// by-identifier sibling in the project-scoped group, so the literal name matched the
// sibling and the request was served as a lookup for a resource called "detail" or
// "default". Two of the three answered 500. This is the failure that made Horizon's
// Create Volume modal abort with nothing in the logs pointing at routing (#533 F8).
func TestProjectScopedSubResourcesNotShadowed(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	base := "/v3/" + seededProjectID

	for _, tc := range []struct {
		path    string
		wantKey string
	}{
		{base + "/volumes/detail", "volumes"},
		{base + "/snapshots/detail", "snapshots"},
		{base + "/types/default", "volume_type"},
		// Found during the o3k#82 review, not by the original hand-derived list: this
		// group registers GET /os-volume-transfer/:id, so `detail` bound to :id and
		// GetVolumeTransfer answered a lookup for a transfer named "detail". Same class
		// as the three above; it had simply been filed as "merely absent".
		{base + "/os-volume-transfer/detail", "transfers"},
	} {
		code, resp := doReq(r, http.MethodGet, tc.path, "", hdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200 (the sub-resource view, not a by-id lookup)", tc.path, code, resp)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(resp), &body); err != nil {
			t.Fatalf("GET %s: unmarshal %s: %v", tc.path, resp, err)
		}
		if _, ok := body[tc.wantKey]; !ok {
			t.Fatalf("GET %s returned %s; want a %q key — a by-identifier lookup answered instead of the sub-resource view",
				tc.path, resp, tc.wantKey)
		}
	}

	// The flat and project-scoped forms of each must agree.
	for _, suffix := range []string{"/volumes/detail", "/snapshots/detail", "/types/default", "/os-volume-transfer/detail"} {
		_, flat := doReq(r, http.MethodGet, "/v3"+suffix, "", hdr)
		_, scoped := doReq(r, http.MethodGet, base+suffix, "", hdr)
		if flat != scoped {
			t.Fatalf("%s: the flat and project-scoped forms disagree\n flat:   %s\n scoped: %s", suffix, flat, scoped)
		}
	}
}

// TestProjectScopedBackupsDetailStaysAbsent guards the R6 finding that corrected the
// specification: `backups/detail` is NOT shadowed. The project-scoped group registers
// no backup route at all, so the path answers an honest 404 from the no-route case
// rather than being served as a lookup for a backup named "detail". It is therefore
// out of scope (FR-016a) — and this test fails if someone adds it as untested surface
// while believing they are fixing a shadowing bug.
func TestProjectScopedBackupsDetailStaysAbsent(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	path := fmt.Sprintf("/v3/%s/backups/detail", seededProjectID)
	code, resp := doReq(r, http.MethodGet, path, "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusNotFound {
		t.Fatalf("GET %s: code=%d body=%s; want 404 — it is absent by decision (FR-016a), not shadowed", path, code, resp)
	}
}
