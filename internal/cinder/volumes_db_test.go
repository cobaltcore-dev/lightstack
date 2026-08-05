package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// testDBAvailable is true when O3K_TEST_DB_URL is set and migrated; DB-backed cinder
// tests skip otherwise (Constitution: real dependencies over mocks). Mirrors
// internal/nova/maindb_test.go so cinder handler tests run against a real Postgres.
var testDBAvailable bool

func TestMain(m *testing.M) {
	if dbURL := os.Getenv("O3K_TEST_DB_URL"); dbURL != "" {
		ctx := context.Background()
		if err := database.Connect(ctx, dbURL, &database.PoolConfig{
			MaxConns: 5, MinConns: 1, MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 15 * time.Minute, HealthCheckPeriod: time.Minute,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL is set but connecting failed: %v\n", err)
			os.Exit(1)
		}
		if err := database.MigrateUp(dbURL, "../../migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL is set but migrations failed: %v\n", err)
			database.Close()
			os.Exit(1)
		}
		testDBAvailable = true
	}
	code := m.Run()
	if testDBAvailable {
		database.Close()
	}
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set O3K_TEST_DB_URL to run DB-backed cinder tests")
	}
}

// Seeded scope from migration 002_seed_data.
const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

// newCinderTestRouter wires the real Cinder routes behind a stub auth middleware that
// sets project_id/user_id/roles from X-Test-* headers.
func newCinderTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	NewService("stub", "", "").RegisterRoutes(&r.RouterGroup)
	return r
}

func doReq(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
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

func cinderAuthHdr(project, user string) map[string]string {
	return map[string]string{"X-Test-Project": project, "X-Test-User": user, "X-Test-Roles": "admin"}
}

// TestVolumeCreateReadback_DB exercises the F9 wiring end-to-end against Postgres:
// CreateVolume must persist the requested volume_type, and GetVolume must SELECT it
// back and serialize the full non-null field set Horizon's table renders. Catches a
// regression where the SELECT drops user_id/volume_type — which the pure serializer
// test cannot see.
func TestVolumeCreateReadback_DB(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodPost, "/v3/volumes", `{"volume":{"name":"dbvol","size":2,"volume_type":"gold"}}`, hdr)
	if code != http.StatusAccepted {
		t.Fatalf("create volume: code=%d body=%s; want 202", code, body)
	}
	var created struct {
		Volume struct {
			ID string `json:"id"`
		} `json:"volume"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Volume.ID == "" {
		t.Fatalf("create response has no id: err=%v body=%s", err, body)
	}
	id := created.Volume.ID
	t.Cleanup(func() { database.DB.Exec(context.Background(), "DELETE FROM volumes WHERE id=$1", id) })

	code, body = doReq(r, http.MethodGet, "/v3/volumes/"+id, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get volume: code=%d body=%s; want 200", code, body)
	}
	var resp struct {
		Volume map[string]any `json:"volume"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal show: %v", err)
	}
	v := resp.Volume
	if v["volume_type"] != "gold" {
		t.Errorf("volume_type = %v, want gold (persisted at create, read back)", v["volume_type"])
	}
	if v["user_id"] != seededUserID {
		t.Errorf("user_id = %v, want %s (SELECTed back)", v["user_id"], seededUserID)
	}
	for _, k := range []string{"availability_zone", "encrypted", "multiattach", "consumes_quota", "shared_targets", "os-vol-host-attr:host", "os-vol-tenant-attr:tenant_id", "metadata"} {
		if v[k] == nil {
			t.Errorf("field %q is null in show (Horizon renders it)", k)
		}
	}
}

// volumeFieldsNonNull is the set of keys Horizon's volume table dereferences; every one
// must be present and non-null on both the show and list-detail responses (F9).
var volumeFieldsNonNull = []string{
	"id", "name", "description", "tenant_id", "user_id", "size", "status", "bootable",
	"created_at", "updated_at", "attachments", "metadata", "volume_type", "volume_type_id",
	"availability_zone", "encrypted", "multiattach", "consumes_quota", "shared_targets",
	"os-vol-tenant-attr:tenant_id", "os-vol-host-attr:host",
}

// seedVolume inserts a volume directly and returns its id, cleaning up afterwards.
func seedVolume(t *testing.T, name, volumeType, description string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := database.DB.Exec(context.Background(),
		`INSERT INTO volumes (id, name, description, project_id, user_id, size_gb, status, bootable, volume_type)
		 VALUES ($1,$2,$3,$4,$5,3,'available',false,$6)`,
		id, name, description, seededProjectID, seededUserID, volumeType); err != nil {
		t.Fatalf("insert volume: %v", err)
	}
	t.Cleanup(func() { database.DB.Exec(context.Background(), "DELETE FROM volumes WHERE id=$1", id) })
	return id
}

// TestListVolumesDetail_DB covers GET /v3/volumes/detail — the endpoint Horizon populates
// its volume table from, and therefore the actual F9 crash path (the 500 was in Horizon's
// get_rows rendering _data_table.html). The single-volume show test cannot catch a
// regression in this SELECT projection, so this asserts the full field set here too.
func TestListVolumesDetail_DB(t *testing.T) {
	requireDB(t)
	id := seedVolume(t, "detailvol", "gold", "a description")
	r := newCinderTestRouter()

	code, body := doReq(r, http.MethodGet, "/v3/volumes/detail", "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("list volumes detail: code=%d body=%s; want 200", code, body)
	}
	var resp struct {
		Volumes []map[string]any `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var got map[string]any
	for _, v := range resp.Volumes {
		if v["id"] == id {
			got = v
		}
	}
	if got == nil {
		t.Fatalf("volume %s not in list-detail response: %s", id, body)
	}
	for _, k := range volumeFieldsNonNull {
		if val, ok := got[k]; !ok || val == nil {
			t.Errorf("list-detail field %q missing or nil (Horizon's table renders it)", k)
		}
	}
	if got["volume_type"] != "gold" {
		t.Errorf("list-detail volume_type = %v, want gold (SELECT projection must carry it)", got["volume_type"])
	}
	if got["description"] != "a description" {
		t.Errorf("list-detail description = %v, want %q", got["description"], "a description")
	}
	for _, k := range []string{"consistencygroup_id", "migration_status", "replication_status", "snapshot_id", "source_volid"} {
		if _, ok := got[k]; !ok {
			t.Errorf("list-detail field %q absent; upstream Cinder always emits it (as null)", k)
		}
	}
}

// TestListVolumesBrief_DB covers GET /v3/volumes: the brief list deliberately returns only
// id/name/size, so this guards the envelope and that projection rather than the full shape.
func TestListVolumesBrief_DB(t *testing.T) {
	requireDB(t)
	id := seedVolume(t, "briefvol", "", "")
	r := newCinderTestRouter()

	code, body := doReq(r, http.MethodGet, "/v3/volumes", "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("list volumes: code=%d body=%s; want 200", code, body)
	}
	var resp struct {
		Volumes []map[string]any `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, v := range resp.Volumes {
		if v["id"] != id {
			continue
		}
		for _, k := range []string{"id", "name", "size"} {
			if val, ok := v[k]; !ok || val == nil {
				t.Errorf("brief list field %q missing or nil", k)
			}
		}
		if v["name"] != "briefvol" {
			t.Errorf("brief list name = %v, want briefvol", v["name"])
		}
		return
	}
	t.Fatalf("volume %s not in brief list response: %s", id, body)
}

// TestDeleteVolumeWithSnapshot_DB guards the snapshot check in DeleteVolume: a volume
// that still has a snapshot must be refused with 409 and must survive. The guard
// previously read `err == nil && snapshotCount > 0`, so any error on the count query
// skipped it and the volume (plus its backing store) was deleted anyway.
func TestDeleteVolumeWithSnapshot_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	volID, snapID := uuid.NewString(), uuid.NewString()
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1,'snapvol',$2,$3,1,'available')`,
		volID, seededProjectID, seededUserID); err != nil {
		t.Fatalf("insert volume: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status) VALUES ($1,'snap',$2,$3,1,'available')`,
		snapID, volID, seededProjectID); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM snapshots WHERE id=$1", snapID)
		database.DB.Exec(ctx, "DELETE FROM volumes WHERE id=$1", volID)
	})

	r := newCinderTestRouter()
	code, body := doReq(r, http.MethodDelete, "/v3/volumes/"+volID, "", cinderAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusConflict {
		t.Errorf("delete volume with snapshot: code=%d body=%s; want 409", code, body)
	}
	var n int
	if err := database.DB.QueryRow(ctx, "SELECT count(*) FROM volumes WHERE id=$1", volID).Scan(&n); err != nil {
		t.Fatalf("count volume: %v", err)
	}
	if n != 1 {
		t.Error("volume was deleted despite having a snapshot")
	}
}

// TestDeleteVolumeSnapshotCountError_DB proves the snapshot guard fails closed. It
// induces a real error on the count query (the snapshots table is renamed away for the
// duration) and asserts the delete is refused and the volume survives. Under the old
// `err == nil && snapshotCount > 0` form the error short-circuited the guard and the
// volume was deleted, so this test fails against that version.
func TestDeleteVolumeSnapshotCountError_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	volID := uuid.NewString()
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1,'errvol',$2,$3,1,'available')`,
		volID, seededProjectID, seededUserID); err != nil {
		t.Fatalf("insert volume: %v", err)
	}
	t.Cleanup(func() { database.DB.Exec(ctx, "DELETE FROM volumes WHERE id=$1", volID) })

	if _, err := database.DB.Exec(ctx, "ALTER TABLE snapshots RENAME TO snapshots_faultinject"); err != nil {
		t.Fatalf("rename snapshots away: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			database.DB.Exec(ctx, "ALTER TABLE snapshots_faultinject RENAME TO snapshots")
			restored = true
		}
	}
	t.Cleanup(restore)

	r := newCinderTestRouter()
	code, body := doReq(r, http.MethodDelete, "/v3/volumes/"+volID, "", cinderAuthHdr(seededProjectID, seededUserID))
	restore()

	if code != http.StatusInternalServerError {
		t.Errorf("delete with failing snapshot count: code=%d body=%s; want 500 (fail closed)", code, body)
	}
	var n int
	if err := database.DB.QueryRow(ctx, "SELECT count(*) FROM volumes WHERE id=$1", volID).Scan(&n); err != nil {
		t.Fatalf("count volume: %v", err)
	}
	if n != 1 {
		t.Error("volume was deleted even though the snapshot count query failed")
	}
}
