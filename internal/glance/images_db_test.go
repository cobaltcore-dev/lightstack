package glance

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the services the tests construct (SPEC-027).
var testPool database.DB

func TestMain(m *testing.M) {
	// Skip DB-backed tests only when no test DB is configured. A configured database
	// that cannot be opened or migrated fails the run instead (dbtest.Open), so a broken
	// test database in CI fails the job rather than quietly hiding the DB-backed tests.
	testPool, testDBAvailable = dbtest.Open("../../migrations")
	code := m.Run()
	dbtest.Close(testPool)
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed glance tests")
	}
}

const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

func newGlanceTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	return newGlanceTestRouterForService(t, newGlanceTestService(t))
}

func newGlanceTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(testPool, "stub", "", "", storage.S3Options{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func newGlanceTestRouterForService(t *testing.T, svc *Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if userID := c.GetHeader("X-Test-User"); userID != "" {
			c.Set("user_id", userID)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc.RegisterRoutes(&r.RouterGroup)
	return r
}

func doReq(r *gin.Engine, method, path string, headers map[string]string) (int, string) {
	return doReqBody(r, method, path, "", headers)
}

func doReqBody(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func imageExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM images WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count image: %v", err)
	}
	return n > 0
}

func hdr(project, roles string) map[string]string {
	return map[string]string{"X-Test-Project": project, "X-Test-Roles": roles}
}

func userHdr(userID, roles string) map[string]string {
	return map[string]string{"X-Test-User": userID, "X-Test-Roles": roles}
}

// TestDeleteImageAuthz_DB exercises the F6 wiring against Postgres for a public image
// whose owner is retained: a non-owner non-admin gets 403 (image stays), a missing id
// gets 404, and the owner (no admin role) deletes it (204, row gone via rows-affected).
// Catches a regression the pure imageDeleteAuthz test cannot — e.g. the DELETE not
// removing the row, or the wrong status wiring.
func TestDeleteImageAuthz_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility) VALUES ($1,'dbimg',$2,'active','public')`,
		id, seededProjectID); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })
	r := newGlanceTestRouter(t)

	// Non-owner, non-admin: can see the public image but may not delete it → 403, kept.
	if code, body := doReq(r, http.MethodDelete, "/images/"+id, hdr("11111111-1111-1111-1111-111111111111", "member")); code != http.StatusForbidden {
		t.Errorf("non-owner delete: code=%d body=%s; want 403", code, body)
	}
	if !imageExists(t, id) {
		t.Error("image was deleted by a non-owner non-admin")
	}

	// Missing id → 404.
	if code, body := doReq(r, http.MethodDelete, "/images/"+uuid.NewString(), hdr(seededProjectID, "member")); code != http.StatusNotFound {
		t.Errorf("delete missing id: code=%d body=%s; want 404", code, body)
	}

	// Owner (no admin role) may delete → 204, row gone.
	if code, body := doReq(r, http.MethodDelete, "/images/"+id, hdr(seededProjectID, "member")); code != http.StatusNoContent {
		t.Errorf("owner delete: code=%d body=%s; want 204", code, body)
	}
	if imageExists(t, id) {
		t.Error("image still present after owner delete (rows-affected wiring)")
	}
}

// TestDeleteImageAdminBypass_DB proves the admin bypass is wired end-to-end: the gin
// context's roles slice must reach imageDeleteAuthz as isAdmin. An admin in a *different*
// project deletes another project's public image → 204 and the row is gone. The pure
// imageDeleteAuthz test passes isAdmin as a bare bool, so it cannot catch a wiring
// regression (e.g. HasRole reading the wrong context key).
func TestDeleteImageAdminBypass_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility) VALUES ($1,'adminimg',$2,'active','public')`,
		id, seededProjectID); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })

	r := newGlanceTestRouter(t)
	const otherProject = "22222222-2222-2222-2222-222222222222"

	// Same caller WITHOUT the admin role is refused — isolating the role as the variable.
	if code, _ := doReq(r, http.MethodDelete, "/images/"+id, hdr(otherProject, "member")); code != http.StatusForbidden {
		t.Fatalf("non-admin in another project: code=%d; want 403 (precondition)", code)
	}
	if code, body := doReq(r, http.MethodDelete, "/images/"+id, hdr(otherProject, "admin")); code != http.StatusNoContent {
		t.Errorf("admin cross-project delete: code=%d body=%s; want 204", code, body)
	}
	if imageExists(t, id) {
		t.Error("image survived an admin delete (roles context -> HasRole -> isAdmin wiring)")
	}
}

// TestDeleteImageInUse_DB covers the imageInUse guard: an image is pinned both by an
// instance booted from it (instances.image_id) and by a volume cloned from it
// (volumes.source_image_id). Each arm alone must yield 409 and leave the image intact,
// so narrowing the UNION ALL to just one table is caught.
func TestDeleteImageInUse_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newGlanceTestRouter(t)
	owner := hdr(seededProjectID, "member")

	newImage := func(t *testing.T) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO images (id, name, project_id, status, visibility) VALUES ($1,'inuse',$2,'active','private')`,
			id, seededProjectID); err != nil {
			t.Fatalf("insert image: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })
		return id
	}

	t.Run("pinned by an instance", func(t *testing.T) {
		imgID := newImage(t)
		instID := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO instances (id, name, project_id, user_id, flavor_id, image_id, status)
			 VALUES ($1,'pin-inst',$2,$3,(SELECT id FROM flavors WHERE name = 'm1.small'),$4,'ACTIVE')`,
			instID, seededProjectID, seededUserID, imgID); err != nil {
			t.Fatalf("insert instance: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM instances WHERE id=$1", instID) })

		if code, body := doReq(r, http.MethodDelete, "/images/"+imgID, owner); code != http.StatusConflict {
			t.Errorf("delete image booted by an instance: code=%d body=%s; want 409", code, body)
		}
		if !imageExists(t, imgID) {
			t.Error("image deleted despite an instance booted from it")
		}
	})

	t.Run("pinned by a volume clone", func(t *testing.T) {
		imgID := newImage(t)
		volID := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volumes (id, name, project_id, size_gb, status, source_image_id) VALUES ($1,'pin-vol',$2,1,'available',$3)`,
			volID, seededProjectID, imgID); err != nil {
			t.Fatalf("insert volume: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM volumes WHERE id=$1", volID) })

		if code, body := doReq(r, http.MethodDelete, "/images/"+imgID, owner); code != http.StatusConflict {
			t.Errorf("delete image cloned to a volume: code=%d body=%s; want 409", code, body)
		}
		if !imageExists(t, imgID) {
			t.Error("image deleted despite a volume cloned from it")
		}
	})
}

// TestCreateImagePublicRetainsOwner_DB exercises the F6 create side through the handler:
// a public image created via POST /images records the caller's project as its owner, so
// that same caller can delete it again. The other DB tests seed images.project_id with
// raw SQL, so none of them would notice a regression to the old
// `Valid: visibility == "private"` ownership condition — which stores public images
// ownerless and silently breaks delete (the original F6 root cause).
func TestCreateImagePublicRetainsOwner_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newGlanceTestRouter(t)

	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"dbimg-public","visibility":"public","disk_format":"qcow2","container_format":"bare"}`,
		hdr(seededProjectID, "member"))
	if code != http.StatusCreated {
		t.Fatalf("create public image: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create response: %v (body=%s)", err, body)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", created.ID) })
	if created.Visibility != "public" {
		t.Fatalf("created visibility=%q; want public", created.Visibility)
	}

	var owner sql.NullString
	if err := testPool.QueryRow(ctx, "SELECT project_id FROM images WHERE id=$1", created.ID).Scan(&owner); err != nil {
		t.Fatalf("read back owner: %v", err)
	}
	if !owner.Valid || owner.String != seededProjectID {
		t.Fatalf("images.project_id=%+v; want %s — a public image must keep its owner", owner, seededProjectID)
	}

	// Ownership is what delete authorization reads, so verify the whole chain: the
	// creator (no admin role) deletes their own public image.
	if code, body := doReq(r, http.MethodDelete, "/images/"+created.ID, hdr(seededProjectID, "member")); code != http.StatusNoContent {
		t.Errorf("owner delete: code=%d body=%s; want 204", code, body)
	}
	if imageExists(t, created.ID) {
		t.Error("image still present after its creator deleted it")
	}
}
