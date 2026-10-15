package nova

// SPEC-028 conformance fixes for the compute surface, found by the CLI
// conformance suites (issue #105). Each test pins the upstream behaviour the
// corresponding bug diverged from.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedSpec028Instance inserts an ACTIVE instance owned by the seeded project
// and returns its id. The caller owns cleanup.
func seedSpec028Instance(t *testing.T, name string, withImage bool) string {
	t.Helper()
	id := uuid.NewString()
	var err error
	if withImage {
		_, err = testPool.Exec(t.Context(),
			`INSERT INTO instances (id, name, project_id, user_id, flavor_id, image_id, status)
			 VALUES ($1, $2, $3, $4, (SELECT id FROM flavors LIMIT 1),
			         (SELECT id FROM images LIMIT 1), 'ACTIVE')`,
			id, name, seededProjectID, seededUserID)
	} else {
		_, err = testPool.Exec(t.Context(),
			`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status)
			 VALUES ($1, $2, $3, $4, (SELECT id FROM flavors LIMIT 1), 'ACTIVE')`,
			id, name, seededProjectID, seededUserID)
	}
	if err != nil {
		t.Fatalf("seed instance %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id=$1", id)
	})
	return id
}

// TestServerGroupCreatePolicy264 — microversion 2.64 replaced the `policies`
// array with a single `policy` string. The server advertises compute
// microversion 2.90, so every 2.64+ client (OSC 7 sends `policy` whenever the
// negotiated version is ≥2.64) was rejected with 400. The fix accepts both
// request shapes and answers with both fields so legacy and modern clients
// read what they expect.
func TestServerGroupCreatePolicy264(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	name := "spec028-sg-policy-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			"DELETE FROM server_groups WHERE name=$1", name)
	})

	code, body := doReq(r, http.MethodPost, "/v2.1/os-server-groups",
		`{"server_group":{"name":"`+name+`","policy":"anti-affinity"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("create with 2.64 policy body: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"policy":"anti-affinity"`) {
		t.Errorf("create response lacks policy field: %s", body)
	}
	if !strings.Contains(body, `"policies":["anti-affinity"]`) {
		t.Errorf("create response lacks legacy policies field: %s", body)
	}
	var id string
	if err := testPool.QueryRow(t.Context(),
		"SELECT id::text FROM server_groups WHERE name=$1", name).Scan(&id); err != nil {
		t.Fatalf("group not persisted: %v", err)
	}

	code, body = doReq(r, http.MethodGet, "/v2.1/os-server-groups/"+id, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"policy":"anti-affinity"`) {
		t.Errorf("show response lacks policy field: %s", body)
	}

	// Both shapes at once, and neither, are client errors (upstream 400s too).
	code, _ = doReq(r, http.MethodPost, "/v2.1/os-server-groups",
		`{"server_group":{"name":"x","policy":"affinity","policies":["affinity"]}}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("policy and policies together: code=%d; want 400", code)
	}
	code, _ = doReq(r, http.MethodPost, "/v2.1/os-server-groups",
		`{"server_group":{"name":"x"}}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("neither policy nor policies: code=%d; want 400", code)
	}
	code, _ = doReq(r, http.MethodPost, "/v2.1/os-server-groups",
		`{"server_group":{"name":"x","policy":"nonsense"}}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("invalid policy value: code=%d; want 400", code)
	}
}

// TestAggregateNonUUIDIs404 — an aggregate addressed by a non-UUID path
// segment (openstacksdk's find_aggregate probes by name first) hit the
// `WHERE uuid = $1` cast error and answered 500, so the CLI aborted instead
// of falling back. Upstream answers 404 itemNotFound for an unknown
// reference of any shape.
func TestAggregateNonUUIDIs404(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodGet, "/v2.1/os-aggregates/my-aggregate-name", "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("get by non-uuid name: code=%d body=%s; want 404", code, body)
	}
	code, body = doReq(r, http.MethodDelete, "/v2.1/os-aggregates/my-aggregate-name", "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("delete by non-uuid name: code=%d body=%s; want 404", code, body)
	}
	// A well-formed but unknown UUID is 404 too (was already correct).
	code, _ = doReq(r, http.MethodGet, "/v2.1/os-aggregates/"+uuid.NewString(), "", hdr)
	if code != http.StatusNotFound {
		t.Errorf("get by unknown uuid: code=%d; want 404", code)
	}
}

// TestCreateBackupImageNameExact — upstream Nova names the backup image
// exactly the requested `name`; the `-<type>-<timestamp>` suffix broke every
// client that looks the image up by name afterwards (OSC: "No Image found
// for <name>").
func TestCreateBackupImageNameExact(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Instance(t, "spec028-bak-src", true)
	imageName := "spec028-bak-img-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE name=$1", imageName)
	})

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
		`{"createBackup":{"name":"`+imageName+`","backup_type":"daily","rotation":1}}`, hdr)
	if code != http.StatusAccepted {
		t.Fatalf("createBackup: code=%d body=%s; want 202", code, body)
	}
	var n int
	if err := testPool.QueryRow(t.Context(),
		"SELECT count(*) FROM images WHERE name=$1", imageName).Scan(&n); err != nil {
		t.Fatalf("count backup image: %v", err)
	}
	if n != 1 {
		t.Errorf("backup image named exactly %q: count=%d; want 1", imageName, n)
	}
}

// TestShelveUnshelveReshelve — instance_snapshots has UNIQUE(instance_id),
// but the unshelve path never deleted the row, so shelve → unshelve → shelve
// failed with 500 "failed to create snapshot". Unshelve consumes the
// snapshot (upstream restores from it), and shelve reclaims any stale row a
// crashed run left behind.
func TestShelveUnshelveReshelve(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Instance(t, "spec028-shelve-src", false)
	action := func(name string) (int, string) {
		return doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
			`{"`+name+`":null}`, hdr)
	}

	if code, body := action("shelve"); code != http.StatusAccepted {
		t.Fatalf("first shelve: code=%d body=%s; want 202", code, body)
	}
	if code, body := action("unshelve"); code != http.StatusAccepted {
		t.Fatalf("unshelve: code=%d body=%s; want 202", code, body)
	}
	var n int
	if err := testPool.QueryRow(t.Context(),
		"SELECT count(*) FROM instance_snapshots WHERE instance_id=$1", id).Scan(&n); err != nil {
		t.Fatalf("count snapshots after unshelve: %v", err)
	}
	if n != 0 {
		t.Errorf("unshelve left %d snapshot rows; want 0 (the snapshot is consumed)", n)
	}
	if code, body := action("shelve"); code != http.StatusAccepted {
		t.Fatalf("re-shelve after unshelve: code=%d body=%s; want 202 (was 500: stale UNIQUE(instance_id) row)", code, body)
	}
}

// TestServerShowLockedField — `locked` was invisible in server show: the
// lock/unlock actions maintained the column, but the detail responses never
// carried it, so `openstack server show` printed null (upstream exposes the
// bool since microversion 2.9).
func TestServerShowLockedField(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	id := seedSpec028Instance(t, "spec028-locked-src", false)

	code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("show unlocked: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"locked":false`) {
		t.Errorf("show must carry locked:false, got: %s", body)
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"lock":null}`, hdr); code != http.StatusAccepted {
		t.Fatalf("lock: code=%d body=%s; want 202", code, body)
	}
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("show locked: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"locked":true`) {
		t.Errorf("show must carry locked:true after lock, got: %s", body)
	}
}
