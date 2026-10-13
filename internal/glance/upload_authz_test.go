package glance

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestUploadImageDataOwnership_DB pins review 2026-10-13 H-1: visibility is a
// read grant, not a write grant. A public image staged by one project must not
// accept bytes from another project's member — that was image poisoning: every
// tenant booting the public base image would run the attacker's data. The
// owner and an admin keep their paths.
func TestUploadImageDataOwnership_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := "00000000-0000-0000-0000-0000000000bb"
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility) VALUES ($1,'pub-poison',$2,'queued','public')`,
		id, seededProjectID); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })

	r := newGlanceTestRouter(t)
	upload := func(hdr map[string]string) int {
		code, _ := doReqBody(r, "PUT", "/images/"+id+"/file", "not really an image", hdr)
		return code
	}

	// A foreign member that can SEE the public image: 403 (visible, not owned).
	if code := upload(hdr(otherProject, "member")); code != 403 {
		t.Errorf("foreign member upload to public image: code=%d; want 403", code)
	}
	// A foreign member that cannot even see a private image: 404.
	if _, err := testPool.Exec(ctx, "UPDATE images SET visibility='private' WHERE id=$1", id); err != nil {
		t.Fatalf("set private: %v", err)
	}
	if code := upload(hdr(otherProject, "member")); code != 404 {
		t.Errorf("foreign member upload to private image: code=%d; want 404", code)
	}
	if _, err := testPool.Exec(ctx, "UPDATE images SET visibility='public' WHERE id=$1", id); err != nil {
		t.Fatalf("set public: %v", err)
	}
	// The owner may still upload (2xx; the stub store accepts anything).
	if code := upload(hdr(seededProjectID, "member")); code < 200 || code >= 300 {
		t.Errorf("owner upload: code=%d; want 2xx", code)
	}
	// An admin may upload to a fresh queued public image.
	id2 := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility) VALUES ($1,'pub-admin',$2,'queued','public')`,
		id2, seededProjectID); err != nil {
		t.Fatalf("insert image 2: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id2) })
	if code, _ := doReqBody(r, "PUT", "/images/"+id2+"/file", "admin bytes", hdr(otherProject, "admin")); code < 200 || code >= 300 {
		t.Errorf("admin upload: code=%d; want 2xx", code)
	}
}

// TestImageCacheManagementAdminOnly_DB pins review 2026-10-13 M-2: cache
// mutation is an operator surface — a member flushing the deployment-wide
// cache (ClearCache) or evicting entries is a denial of service on
// boot-time caching.
func TestImageCacheManagementAdminOnly_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, visibility, cached_at) VALUES ($1,'cached',$2,'active','public',NOW())`,
		id, seededProjectID); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM images WHERE id=$1", id) })

	r := newGlanceTestRouter(t)

	if code, _ := doReq(r, "DELETE", "/cache/images/"+id, hdr(seededProjectID, "member")); code != 403 {
		t.Errorf("member cache eviction: code=%d; want 403", code)
	}
	if code, _ := doReq(r, "DELETE", "/cache/images", hdr(seededProjectID, "member")); code != 403 {
		t.Errorf("member cache clear: code=%d; want 403", code)
	}
	if code, _ := doReq(r, "PUT", "/cache/images/"+id, hdr(seededProjectID, "member")); code != 403 {
		t.Errorf("member cache prefetch: code=%d; want 403", code)
	}
	// The list is part of the same operator surface (round 2 of this review:
	// the write paths were gated, the list was missed).
	if code, _ := doReq(r, "GET", "/cache/images", hdr(seededProjectID, "member")); code != 403 {
		t.Errorf("member cache list: code=%d; want 403", code)
	}
	if code, body := doReq(r, "GET", "/cache/images", hdr(seededProjectID, "admin")); code != 200 || !strings.Contains(body, id) {
		t.Errorf("admin cache list: code=%d body=%s; want 200 listing the cached image", code, body)
	}
	// Still cached after the denied attempts.
	var cached bool
	if err := testPool.QueryRow(ctx, "SELECT cached_at IS NOT NULL FROM images WHERE id=$1", id).Scan(&cached); err != nil || !cached {
		t.Errorf("denied cache clear still took effect (cached=%v err=%v)", cached, err)
	}
}
