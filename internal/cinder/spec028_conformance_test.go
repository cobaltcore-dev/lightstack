package cinder

// SPEC-028 conformance fixes for the volume surface, found by the CLI
// conformance suites (issue #105).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedSpec028Volume inserts an available volume owned by the seeded project
// and returns its id. The caller owns cleanup.
func seedSpec028Volume(t *testing.T, name string, bootable bool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, bootable, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 1, 'available', $5, now(), now())`,
		id, name, seededProjectID, seededUserID, bootable); err != nil {
		t.Fatalf("seed volume %s: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM volume_metadata WHERE volume_id=$1", id)
		testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id=$1", id)
	})
	return id
}

// TestUpdateVolumeV2Aliases — python-cinderclient still PUTs the legacy v2
// field names (display_name / display_description) for `volume set
// --name/--description`; only the v3 names were bound, so the request
// succeeded but persisted nothing (SPEC-028).
func TestUpdateVolumeV2Aliases(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Volume(t, "spec028-v2alias", false)

	code, body := doReq(r, http.MethodPut, "/v3/volumes/"+id,
		`{"volume":{"display_name":"spec028-v2alias-renamed","display_description":"set via v2 alias"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("update with v2 aliases: code=%d body=%s", code, body)
	}

	var name, description string
	if err := testPool.QueryRow(t.Context(),
		"SELECT name, COALESCE(description,'') FROM volumes WHERE id=$1", id).Scan(&name, &description); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "spec028-v2alias-renamed" {
		t.Errorf("display_name was not persisted: name=%q", name)
	}
	if description != "set via v2 alias" {
		t.Errorf("display_description was not persisted: description=%q", description)
	}

	// The v3 names keep precedence when both shapes are sent.
	code, _ = doReq(r, http.MethodPut, "/v3/volumes/"+id,
		`{"volume":{"name":"v3-wins","display_name":"v2-loses"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("update with both shapes: code=%d", code)
	}
	if err := testPool.QueryRow(t.Context(),
		"SELECT name FROM volumes WHERE id=$1", id).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "v3-wins" {
		t.Errorf("v3 name must win over the v2 alias: name=%q", name)
	}
}

// TestVolumeMetadataOnObject — volume show and list-detail serialized
// metadata: {} however much was set, so 'volume show' always reported empty
// properties (SPEC-028).
func TestVolumeMetadataOnObject(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Volume(t, "spec028-meta", false)

	code, body := doReq(r, http.MethodPost, "/v3/volumes/"+id+"/metadata",
		`{"metadata":{"spec028key":"spec028value"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("set metadata: code=%d body=%s", code, body)
	}

	code, body = doReq(r, http.MethodGet, "/v3/volumes/"+id, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"metadata":{"spec028key":"spec028value"}`) {
		t.Errorf("show response lacks the volume's metadata: %s", body)
	}

	code, body = doReq(r, http.MethodGet, "/v3/volumes/detail", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list detail: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"metadata":{"spec028key":"spec028value"}`) {
		t.Errorf("list-detail response lacks the volume's metadata: %s", body)
	}
}

// TestVolumeSetBootableAction — `volume set --bootable/--non-bootable` PUTs
// os-set_bootable, which answered 400 unknown action (SPEC-028).
func TestVolumeSetBootableAction(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Volume(t, "spec028-bootable", false)

	code, body := doReq(r, http.MethodPost, "/v3/volumes/"+id+"/action",
		`{"os-set_bootable":{"bootable":true}}`, hdr)
	if code != http.StatusAccepted {
		t.Fatalf("os-set_bootable: code=%d body=%s; want 202", code, body)
	}
	_, body = doReq(r, http.MethodGet, "/v3/volumes/"+id, "", hdr)
	if !strings.Contains(body, `"bootable":"true"`) {
		t.Errorf("bootable was not persisted: %s", body)
	}

	code, body = doReq(r, http.MethodPost, "/v3/volumes/"+id+"/action",
		`{"os-set_bootable":{"bootable":false}}`, hdr)
	if code != http.StatusAccepted {
		t.Fatalf("os-set_bootable false: code=%d body=%s; want 202", code, body)
	}
	_, body = doReq(r, http.MethodGet, "/v3/volumes/"+id, "", hdr)
	if !strings.Contains(body, `"bootable":"false"`) {
		t.Errorf("bootable was not reset: %s", body)
	}

	// Malformed payloads are client errors, and an unknown volume 404s.
	code, _ = doReq(r, http.MethodPost, "/v3/volumes/"+id+"/action",
		`{"os-set_bootable":{"bootable":"yes"}}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("non-boolean bootable: code=%d; want 400", code)
	}
	code, _ = doReq(r, http.MethodPost, "/v3/volumes/"+uuid.NewString()+"/action",
		`{"os-set_bootable":{"bootable":true}}`, hdr)
	if code != http.StatusNotFound {
		t.Errorf("unknown volume: code=%d; want 404", code)
	}
}

// TestSnapshotMetadataKey — snapshot create/show/list omitted the metadata
// key entirely, and the pinned OSC 7 formatter crashes with KeyError
// 'metadata' on a snapshot without one (SPEC-028).
func TestSnapshotMetadataKey(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	volID := seedSpec028Volume(t, "spec028-snapmeta-vol", false)

	code, body := doReq(r, http.MethodPost, "/v3/snapshots",
		`{"snapshot":{"name":"spec028-snapmeta","volume_id":"`+volID+`"}}`, hdr)
	if code != http.StatusAccepted {
		t.Fatalf("create snapshot: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"metadata":{}`) {
		t.Errorf("create response lacks the metadata key: %s", body)
	}
	var snapID string
	if err := testPool.QueryRow(t.Context(),
		"SELECT id::text FROM snapshots WHERE name='spec028-snapmeta' AND volume_id=$1", volID).Scan(&snapID); err != nil {
		t.Fatalf("resolve snapshot: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM snapshot_metadata WHERE snapshot_id=$1", snapID)
		testPool.Exec(context.Background(), "DELETE FROM snapshots WHERE id=$1", snapID)
	})

	// Set metadata through the endpoint, then read it back on the object.
	code, body = doReq(r, http.MethodPost, "/v3/snapshots/"+snapID+"/metadata",
		`{"metadata":{"snapkey":"snapval"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("set snapshot metadata: code=%d body=%s", code, body)
	}
	_, body = doReq(r, http.MethodGet, "/v3/snapshots/"+snapID, "", hdr)
	if !strings.Contains(body, `"metadata":{"snapkey":"snapval"}`) {
		t.Errorf("show response lacks the snapshot's metadata: %s", body)
	}
	_, body = doReq(r, http.MethodGet, "/v3/snapshots/detail", "", hdr)
	if !strings.Contains(body, `"metadata":{"snapkey":"snapval"}`) {
		t.Errorf("list-detail response lacks the snapshot's metadata: %s", body)
	}
}

// TestVolumeTypeConformance — three defects in the type surface (SPEC-028):
//   - a type without a description (NULL) made GET /v3/types fail with 500
//     for every caller until the type was deleted;
//   - the type object omitted extra_specs, so 'volume type show' always
//     reported properties: {} however much was set;
//   - a non-UUID reference (a name) 500'd the uuid-typed lookups instead of
//     answering 404.
func TestVolumeTypeConformance(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	// No description — that is the row shape that broke the listing.
	typeID := uuid.NewString()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO volume_types (id, name, is_public, extra_specs, created_at)
		 VALUES ($1, 'spec028-type-nodesc', true, '{"spec028k":"spec028v"}'::jsonb, now())`,
		typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM volume_types WHERE id=$1", typeID) })

	code, body := doReq(r, http.MethodGet, "/v3/types", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list types with a NULL-description member: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"name":"spec028-type-nodesc"`) {
		t.Errorf("listing dropped the NULL-description type: %s", body)
	}

	code, body = doReq(r, http.MethodGet, "/v3/types/"+typeID, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("show type: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"extra_specs":{"spec028k":"spec028v"}`) {
		t.Errorf("show response lacks the type's extra_specs: %s", body)
	}

	// A non-UUID reference and an unknown UUID both 404 — in show and delete.
	for _, ref := range []string{"spec028-type-nodesc", uuid.NewString()} {
		code, _ = doReq(r, http.MethodGet, "/v3/types/"+ref, "", hdr)
		if code != http.StatusNotFound {
			t.Errorf("show type by %q: code=%d; want 404", ref, code)
		}
		code, _ = doReq(r, http.MethodDelete, "/v3/types/"+ref, "", hdr)
		if code != http.StatusNotFound {
			t.Errorf("delete type by %q: code=%d; want 404", ref, code)
		}
	}
}

// TestBackupNonUUIDIs404 — cinderclient resolves backup show/delete by name
// with no list fallback, so the name itself reached the uuid-typed lookup
// and surfaced the cast error as 500; clients need the 404 (SPEC-028).
func TestBackupNonUUIDIs404(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	for _, ref := range []string{"my-backup-name", "not-a-uuid", uuid.NewString()} {
		code, _ := doReq(r, http.MethodGet, "/v3/backups/"+ref, "", hdr)
		if code != http.StatusNotFound {
			t.Errorf("show backup by %q: code=%d; want 404", ref, code)
		}
		code, _ = doReq(r, http.MethodDelete, "/v3/backups/"+ref, "", hdr)
		if code != http.StatusNotFound {
			t.Errorf("delete backup by %q: code=%d; want 404", ref, code)
		}
	}
}
