package glance

// Round-8 delta review pins for PR #107 (issue #105): the tag-mutation cache
// eviction and the name-length validation, both from the round-7 fix set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// newGlanceCacheRouter builds a router whose service carries a REAL cache —
// newGlanceTestRouter passes nil, and the eviction this file pins only exists
// with one behind the service. Skips when no local Redis is available (the
// CI unit-tests job provides one).
func newGlanceCacheRouter(t *testing.T) (*gin.Engine, *cache.Cache) {
	t.Helper()
	cacheInst, err := cache.NewCache(cache.Config{
		RedisURL: "redis://127.0.0.1:6379", Enabled: true,
		KeyPrefix: "spec028-r8glance:", DefaultTTL: time.Minute,
	})
	if err != nil {
		t.Skipf("no local Redis for the cache regression: %v", err)
	}
	t.Cleanup(func() {
		// DeletePattern prepends the instance's KeyPrefix itself, so the
		// pattern is just "*" — a prefixed pattern would double the prefix
		// and match nothing (round 9).
		cacheInst.DeletePattern(context.Background(), "*")
	})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc, err := NewService(testPool, "stub", "", "", storage.S3Options{}, cacheInst)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.RegisterRoutes(&r.RouterGroup)
	return r, cacheInst
}

// TestAddImageTagEvictsCache — GetImage caches the object INCLUDING its tags;
// the dedicated tag PUT used to skip the eviction every other image mutation
// performs, so a tag added after a show stayed invisible to `image show` for
// the cache's whole TTL (SPEC-028, round-7 F3).
func TestAddImageTagEvictsCache(t *testing.T) {
	requireDB(t)
	r, _ := newGlanceCacheRouter(t)

	hdr := map[string]string{"X-Test-Project": "00000000-0000-0000-0000-000000000002"}
	// The tag is deliberately NOT a substring of the image name: the first
	// version of this test asserted on a tag that matched the name in every
	// response, so the check passed with the eviction deleted (round 9).
	name := "spec028-r8-addtag-" + uuid.NewString()[:8]
	const tag = "round8-evict-pin"
	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"`+name+`","disk_format":"raw","container_format":"bare"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create image: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		t.Fatalf("create response carries no image id: %s", body)
	}
	imageID := created.ID
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM image_tags WHERE image_id=$1", imageID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", imageID)
	})

	// Prime the cache, then mutate via the dedicated tag endpoint.
	if code, body := doReq(r, http.MethodGet, "/images/"+imageID, hdr); code != http.StatusOK {
		t.Fatalf("prime show: code=%d body=%s", code, body)
	}
	code, body = doReq(r, http.MethodPut, "/images/"+imageID+"/tags/"+tag, hdr)
	if code != http.StatusNoContent {
		t.Fatalf("add tag: code=%d body=%s; want 204", code, body)
	}

	// The follow-up show must see the tag — a stale cache would answer the
	// primed body with tags: []. The assertion is the exact single-element
	// array: a loose substring could match unrelated response fields.
	code, body = doReq(r, http.MethodGet, "/images/"+imageID, hdr)
	if code != http.StatusOK {
		t.Fatalf("post-tag show: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"tags":["`+tag+`"]`) {
		t.Errorf("post-tag show does not see the tag — cache not evicted: %s", body)
	}
}

// TestDeleteImageTagEvictsCache — the same pin for the DELETE half.
func TestDeleteImageTagEvictsCache(t *testing.T) {
	requireDB(t)
	r, _ := newGlanceCacheRouter(t)

	hdr := map[string]string{"X-Test-Project": "00000000-0000-0000-0000-000000000002"}
	name := "spec028-r8-untag-" + uuid.NewString()[:8]
	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"`+name+`","disk_format":"raw","container_format":"bare","tags":["spec028-r8-gone"]}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create image: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		t.Fatalf("create response carries no image id: %s", body)
	}
	imageID := created.ID
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM image_tags WHERE image_id=$1", imageID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", imageID)
	})

	if code, body := doReq(r, http.MethodGet, "/images/"+imageID, hdr); code != http.StatusOK {
		t.Fatalf("prime show: code=%d body=%s", code, body)
	}
	code, body = doReq(r, http.MethodDelete, "/images/"+imageID+"/tags/spec028-r8-gone", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete tag: code=%d body=%s; want 204", code, body)
	}

	code, body = doReq(r, http.MethodGet, "/images/"+imageID, hdr)
	if code != http.StatusOK {
		t.Fatalf("post-untag show: code=%d body=%s", code, body)
	}
	// Exact-array form, symmetric with the add-tag pin above.
	if strings.Contains(body, `"tags":["spec028-r8-gone"]`) {
		t.Errorf("post-untag show still sees the tag — cache not evicted: %s", body)
	}
}

// TestCreateImageNameTooLong — the name column is VARCHAR(255) and upstream's
// schema answers 400; the overflow used to surface as a 500 from the INSERT
// (SPEC-028, round-7 F7).
func TestCreateImageNameTooLong(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	hdr := map[string]string{"X-Test-Project": "00000000-0000-0000-0000-000000000002"}

	long := strings.Repeat("n", 256)
	code, body := doReqBody(r, http.MethodPost, "/images",
		`{"name":"`+long+`","disk_format":"raw","container_format":"bare"}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("create with 256-char name: code=%d body=%s; want 400", code, body)
	}
}

func TestImageNameAndTagLengthCountCharacters(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	hdr := map[string]string{"X-Test-Project": "00000000-0000-0000-0000-000000000002"}
	value := strings.Repeat("é", 255)
	payload, _ := json.Marshal(map[string]any{"name": value, "disk_format": "raw", "container_format": "bare", "tags": []string{value}})
	code, body := doReqBody(r, http.MethodPost, "/images", string(payload), hdr)
	if code != http.StatusCreated {
		t.Fatalf("255 Unicode characters: %d %s", code, body)
	}
	var image map[string]any
	if err := json.Unmarshal([]byte(body), &image); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", image["id"]) })
}
