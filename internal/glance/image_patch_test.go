package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/google/uuid"

	// Imported for its side effect ONLY, and it is load-bearing: this package's init()
	// sets binding.EnableDecoderUseNumber, which makes every JSON number bind as
	// json.Number rather than float64. The production binary links it through main.go;
	// this test harness did not, so these tests bound numbers as float64 and asserted a
	// shape production never produces.
	//
	// The whole of R3 failed on the real-mode host while every test here passed —
	// `openstack image set --min-disk 10` answered 400 because the handler saw a
	// json.Number it did not accept. Setting the flag directly would work too, but
	// importing the package means the tests inherit production's ACTUAL decoder
	// configuration rather than a copy of it that can drift.
	_ "github.com/cobaltcore-dev/lightstack/internal/middleware"
)

// SPEC-025-followup US2: the image patch applies what it accepts, or says why not.
//
// UpdateImage applied only `replace`, discarded every other operation and every
// non-allowlisted path, then answered 200 with the unchanged image. `openstack image set
// --min-disk N` sends `add`, so the documented way to set a property reported success and
// changed nothing — silently in both directions, since the handler's only log line fires
// when a `replace` UPDATE errors.
//
// EVERY TEST HERE READS THE VALUE BACK. A test asserting the response status passes
// against the discard behaviour, which is how this survived: the status was always 200.

const patchOtherProject = "11111111-1111-1111-1111-111111111111"

// doPatch issues a JSON-Patch request. The package's existing doReq takes no body, and a
// patch is nothing but a body.
func doPatch(r *gin.Engine, path, body string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/openstack-images-v2.1-json-patch")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// seedPatchImage creates an image owned by the seeded project with min_disk/min_ram unset.
func seedPatchImage(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	// disk_format and container_format are set deliberately: GetImage scans both into
	// plain strings, so a NULL makes the read fail and the patch answer 500 instead of
	// whatever it was going to answer. CreateImage always defaults them, so a NULL is not
	// reachable through the API — only through a fixture like this one.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility, disk_format, container_format, min_disk_gb, min_ram_mb)
		 VALUES ($1, 'patch-target', $2, 'active', 'private', 'qcow2', 'bare', 0, 0)`, id, seededProjectID); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", id) })
	return id
}

// imageColumn reads a column straight from the database, so a test cannot be satisfied by
// a response body that merely echoes the request.
func imageColumn(t *testing.T, id, column string) string {
	t.Helper()
	var v *string
	if err := testPool.QueryRow(context.Background(),
		"SELECT "+column+"::text FROM images WHERE id = $1", id).Scan(&v); err != nil {
		t.Fatalf("reading %s: %v", column, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

// TestImagePatchAdd is T025: FR-010. `add` on a writable property writes it — the CLI path
// for setting a property that has no value yet.
func TestImagePatchAdd(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	code, body := doPatch(r, "/images/"+id, `[{"op":"add","path":"/min_disk","value":7}]`, hdr(seededProjectID, "member"))
	if code != http.StatusOK {
		t.Fatalf("add: code=%d body=%s; want 200", code, body)
	}

	// The response must report the applied state (FR-016)...
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshalling %s: %v", body, err)
	}
	if got, ok := resp["min_disk"]; !ok || int(got.(float64)) != 7 {
		t.Errorf("add: response min_disk = %v; want 7", resp["min_disk"])
	}
	// ...and the database must agree. This is the assertion the defect survived.
	if got := imageColumn(t, id, "min_disk_gb"); got != "7" {
		t.Errorf("add: min_disk_gb in the database = %q; want 7 — the operation was accepted and discarded", got)
	}
}

// TestImagePatchReplace is T026: today's behaviour, preserved. A CONTROL — it passes before
// and after, and its job is to catch a rewrite that breaks the one operation that worked.
func TestImagePatchReplace(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	for _, tc := range []struct{ path, value, column, want string }{
		{"/min_disk", "10", "min_disk_gb", "10"},
		{"/min_ram", "512", "min_ram_mb", "512"},
		{"/name", `"renamed"`, "name", "renamed"},
		{"/visibility", `"public"`, "visibility", "public"},
	} {
		code, body := doPatch(r, "/images/"+id,
			`[{"op":"replace","path":"`+tc.path+`","value":`+tc.value+`}]`, hdr(seededProjectID, "member"))
		if code != http.StatusOK {
			t.Fatalf("replace %s: code=%d body=%s; want 200", tc.path, code, body)
		}
		if got := imageColumn(t, id, tc.column); got != tc.want {
			t.Errorf("replace %s: %s = %q; want %q", tc.path, tc.column, got, tc.want)
		}
	}
}

// TestImagePatchRemoveRefused is T027: FR-011. All four writable paths are base properties
// carrying a schema default, so none has an absent state to be removed to — and resetting
// one to its default would make `remove` a synonym for a write the client never asked for.
// Upstream answers 403 for a base property.
func TestImagePatchRemoveRefused(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	// Give min_disk a value first, so "unchanged" is distinguishable from "was already 0".
	if code, body := doPatch(r, "/images/"+id, `[{"op":"replace","path":"/min_disk","value":10}]`, hdr(seededProjectID, "member")); code != http.StatusOK {
		t.Fatalf("setup replace: code=%d body=%s", code, body)
	}

	for _, path := range []string{"/min_disk", "/min_ram", "/name", "/visibility"} {
		code, body := doPatch(r, "/images/"+id, `[{"op":"remove","path":"`+path+`"}]`, hdr(seededProjectID, "member"))
		if code != http.StatusForbidden {
			t.Errorf("remove %s: code=%d body=%s; want 403", path, code, body)
		}
		if !strings.Contains(body, path) {
			t.Errorf("remove %s: body=%s; the refusal must name the path", path, body)
		}
	}
	if got := imageColumn(t, id, "min_disk_gb"); got != "10" {
		t.Errorf("after refused removes, min_disk_gb = %q; want 10 unchanged — a refusal must not write", got)
	}
	if got := imageColumn(t, id, "name"); got != "patch-target" {
		t.Errorf("after refused removes, name = %q; want it unchanged", got)
	}
}

// TestImagePatchUnsupportedOperation is T028: FR-012. An operation RFC 6902 does not define
// is a malformed document, answered 400 naming the operation — not 200 with no change.
func TestImagePatchUnsupportedOperation(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	for _, op := range []string{"move", "copy", "test", "frobnicate"} {
		code, body := doPatch(r, "/images/"+id,
			`[{"op":"`+op+`","path":"/min_disk","value":1}]`, hdr(seededProjectID, "member"))
		if code != http.StatusBadRequest {
			t.Errorf("op %s: code=%d body=%s; want 400", op, code, body)
		}
		if !strings.Contains(body, op) {
			t.Errorf("op %s: body=%s; the refusal must name the operation", op, body)
		}
	}
	if got := imageColumn(t, id, "min_disk_gb"); got != "0" {
		t.Errorf("min_disk_gb = %q after refused operations; want 0 unchanged", got)
	}
}

// TestImagePatchUnwritablePath is T029: FR-013. The handler discarded non-allowlisted paths
// as silently as it discarded non-replace operations — so `image set --property foo=bar`
// reported success too. Upstream answers 403 for a service-managed property.
func TestImagePatchUnwritablePath(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	for _, path := range []string{"/status", "/id", "/checksum", "/os_glance_import_task", "/hw_disk_bus", "/arbitrary_property"} {
		code, body := doPatch(r, "/images/"+id,
			`[{"op":"replace","path":"`+path+`","value":"x"}]`, hdr(seededProjectID, "member"))
		if code != http.StatusForbidden {
			t.Errorf("path %s: code=%d body=%s; want 403", path, code, body)
		}
		if !strings.Contains(body, path) {
			t.Errorf("path %s: body=%s; the refusal must name the path", path, body)
		}
	}
	if got := imageColumn(t, id, "status"); got != "active" {
		t.Errorf("status = %q after a refused patch; want it untouched", got)
	}
}

// TestImagePatchIsAtomic is T030: FR-014. The loop applied each operation with its own
// UPDATE and continued past failures, so a document could half-apply. Either all of it
// lands or none does.
func TestImagePatchIsAtomic(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	// First operation is valid, second cannot be applied.
	code, body := doPatch(r, "/images/"+id,
		`[{"op":"replace","path":"/min_disk","value":42},{"op":"remove","path":"/name"}]`,
		hdr(seededProjectID, "member"))
	if code == http.StatusOK {
		t.Fatalf("a document with an inapplicable member returned 200: %s", body)
	}
	if got := imageColumn(t, id, "min_disk_gb"); got != "0" {
		t.Errorf("min_disk_gb = %q; want 0 — the valid member must NOT have been applied", got)
	}

	// And the all-valid multi-operation case still applies completely.
	code, body = doPatch(r, "/images/"+id,
		`[{"op":"replace","path":"/min_disk","value":3},{"op":"add","path":"/min_ram","value":256}]`,
		hdr(seededProjectID, "member"))
	if code != http.StatusOK {
		t.Fatalf("all-valid document: code=%d body=%s; want 200", code, body)
	}
	if got := imageColumn(t, id, "min_disk_gb"); got != "3" {
		t.Errorf("min_disk_gb = %q; want 3", got)
	}
	if got := imageColumn(t, id, "min_ram_mb"); got != "256" {
		t.Errorf("min_ram_mb = %q; want 256", got)
	}
}

// TestImagePatchZeroRows is T031: FR-015. A write matching no row was answered 200 with the
// unchanged image, because RowsAffected was never checked.
func TestImagePatchZeroRows(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	// An id that does not exist.
	if code, body := doPatch(r, "/images/"+uuid.NewString(),
		`[{"op":"replace","path":"/min_disk","value":5}]`, hdr(seededProjectID, "member")); code != http.StatusNotFound {
		t.Errorf("missing image: code=%d body=%s; want 404", code, body)
	}

	// An image owned by another project.
	if code, body := doPatch(r, "/images/"+id,
		`[{"op":"replace","path":"/min_disk","value":5}]`, hdr(patchOtherProject, "member")); code != http.StatusNotFound {
		t.Errorf("other project's image: code=%d body=%s; want 404", code, body)
	}
	if got := imageColumn(t, id, "min_disk_gb"); got != "0" {
		t.Errorf("min_disk_gb = %q after a cross-project patch; want 0 unchanged", got)
	}
}

// TestImagePatchValueType is T032: a value of the wrong type for its column must be a
// client error, not a database fault.
func TestImagePatchValueType(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	for _, body := range []string{
		`[{"op":"replace","path":"/min_disk","value":"ten"}]`,
		`[{"op":"replace","path":"/min_ram","value":true}]`,
		`[{"op":"replace","path":"/name","value":42}]`,
	} {
		code, resp := doPatch(r, "/images/"+id, body, hdr(seededProjectID, "member"))
		if code >= 500 {
			t.Errorf("%s: code=%d resp=%s; a wrong-typed value must not be a server fault", body, code, resp)
		}
		if code == http.StatusOK {
			t.Errorf("%s: code=200; a wrong-typed value must not be reported as applied", body)
		}
	}
}

// TestImagePatchMalformedDocument covers the document-level shapes: valid JSON that is not
// an array of operations, and an entry missing its op or path.
func TestImagePatchMalformedDocument(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	r := newGlanceTestRouter(t)

	for _, body := range []string{
		`{"op":"replace","path":"/min_disk","value":1}`,
		`[{"path":"/min_disk","value":1}]`,
		`[{"op":"replace","value":1}]`,
		`["not an object"]`,
	} {
		code, resp := doPatch(r, "/images/"+id, body, hdr(seededProjectID, "member"))
		if code != http.StatusBadRequest {
			t.Errorf("%s: code=%d resp=%s; want 400", body, code, resp)
		}
	}
}

// TestImagePatchBindsNumbersAsJSONNumber pins the decoder configuration these tests run
// under, rather than trusting it.
//
// Without it, the blank import above looks like an unused dependency somebody may tidy
// away — and tidying it away would return every value-type test in this file to asserting
// a shape production does not produce. This is the guard on that.
func TestImagePatchBindsNumbersAsJSONNumber(t *testing.T) {
	if !binding.EnableDecoderUseNumber {
		t.Fatal("binding.EnableDecoderUseNumber is false: these tests are binding numbers as float64, " +
			"which production never does. The blank import of internal/middleware sets it — restore it.")
	}

	// And the coercion must handle that shape, which is the defect the host run found.
	var raw any = json.Number("10")
	got, err := coerceImagePatchValue(imageUpdateFields["/min_disk"], "/min_disk", raw)
	if err != nil {
		t.Fatalf("coercing json.Number(10) returned %v; production binds every integer this way", err)
	}
	if got != int64(10) {
		t.Errorf("coercing json.Number(10) = %v (%T); want int64(10)", got, got)
	}
}

// TestImageVisibilityAcceptsOnlyWhatTheProductHonours pins the visibility vocabulary on
// BOTH the patch and the create path (branch review SRC-15).
//
// The value was type-checked and not domain-checked, so any string was stored. An
// unrecognised value is none of upstream's four, so only the owner sees the image, which
// fails closed — but the client was still told 200 for a value nothing honours. That is
// the silent success this whole feature exists to remove — the same reason the server-list
// filters refuse `host` rather than ignoring it, and the same reason `remove` on an image
// property answers 403 rather than 200-with-no-change.
//
// All four upstream values are accepted: `shared` and `community` are honoured on read
// (common.ImageVisibleSQL, SPEC-020 FR-026), so refusing them would refuse working sharing.
func TestImageVisibilityAcceptsOnlyWhatTheProductHonours(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)

	t.Run("patch", func(t *testing.T) {
		for _, v := range []string{"public", "private", "shared", "community"} {
			id := seedPatchImage(t)
			code, body := doPatch(r, "/images/"+id,
				`[{"op":"replace","path":"/visibility","value":"`+v+`"}]`,
				hdr(seededProjectID, "member"))
			if code != http.StatusOK {
				t.Errorf("visibility=%q: code=%d body=%s; want 200", v, code, body)
			}
			if got := imageColumn(t, id, "visibility"); got != v {
				t.Errorf("visibility=%q stored %q", v, got)
			}
		}

		// Refused, and nothing written: none of these is an upstream visibility.
		for _, v := range []string{"nonsense", "PUBLIC", ""} {
			id := seedPatchImage(t)
			code, body := doPatch(r, "/images/"+id,
				`[{"op":"replace","path":"/visibility","value":"`+v+`"}]`,
				hdr(seededProjectID, "member"))
			if code != http.StatusBadRequest {
				t.Errorf("visibility=%q: code=%d body=%s; want 400", v, code, body)
			}
			if got := imageColumn(t, id, "visibility"); got != "private" {
				t.Errorf("visibility=%q was refused but the column changed to %q; a refusal must write nothing", v, got)
			}
		}
	})

	t.Run("create", func(t *testing.T) {
		// The create path had the same hole, and validating only the patch path would
		// leave the two accepting different value sets.
		post := func(v string) (int, string) {
			req := httptest.NewRequest(http.MethodPost, "/images",
				strings.NewReader(`{"name":"vis-probe","visibility":"`+v+`"}`))
			req.Header.Set("Content-Type", "application/json")
			for k, hv := range hdr(seededProjectID, "member") {
				req.Header.Set(k, hv)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Code, w.Body.String()
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE name = 'vis-probe'")
		})

		for _, v := range []string{"public", "private", "shared", "community"} {
			if code, body := post(v); code != http.StatusCreated {
				t.Errorf("create visibility=%q: code=%d body=%s; want 201", v, code, body)
			}
		}
		for _, v := range []string{"nonsense", "PUBLIC"} {
			code, body := post(v)
			if code != http.StatusBadRequest {
				t.Errorf("create visibility=%q: code=%d body=%s; want 400", v, code, body)
			}
			var n int
			if err := testPool.QueryRow(context.Background(),
				`SELECT count(*) FROM images WHERE name = 'vis-probe' AND visibility = $1`, v).Scan(&n); err != nil {
				t.Fatalf("counting: %v", err)
			}
			if n != 0 {
				t.Errorf("create visibility=%q was refused but %d image(s) were stored with it", v, n)
			}
		}
	})
}
