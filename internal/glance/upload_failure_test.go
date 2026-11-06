package glance

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/storage"
)

func TestUploadFailsWhenStateCannotBeRecorded(t *testing.T) {
	requireDB(t)
	for _, status := range []string{"saving", "active"} {
		t.Run(status, func(t *testing.T) {
			id := seedPatchImage(t)
			if _, err := testPool.Exec(t.Context(), "UPDATE images SET status='queued' WHERE id=$1", id); err != nil {
				t.Fatal(err)
			}
			// A trigger injects a real write failure for only this image and state.
			name := "reject_upload_" + strings.ReplaceAll(id, "-", "")
			_, err := testPool.Exec(t.Context(), fmt.Sprintf(`
				CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
				  IF NEW.id = '%s'::uuid AND NEW.status = '%s' THEN
				    RAISE EXCEPTION 'injected image write failure';
				  END IF;
				  RETURN NEW;
				END $$;
				CREATE TRIGGER %s BEFORE UPDATE ON images FOR EACH ROW EXECUTE FUNCTION %s();`, name, id, status, name, name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER %s ON images; DROP FUNCTION %s();", name, name))
			})
			code, body := doReqBody(newGlanceTestRouter(t), http.MethodPut, "/images/"+id+"/file", "test image", hdr(seededProjectID, "member"))
			if code < 500 {
				t.Errorf("failed state write returned %d %s; want 5xx", code, body)
			}
			if strings.Contains(body, "injected") {
				t.Errorf("database details leaked: %s", body)
			}
			if got := imageColumn(t, id, "status"); got != "queued" {
				t.Errorf("status=%s; want queued for retry", got)
			}
		})
	}
}

func TestUnreadableNormalizedImageCannotBecomeActive(t *testing.T) {
	requireDB(t)
	id := seedPatchImage(t)
	if _, err := testPool.Exec(t.Context(), "UPDATE images SET status='saving' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(testPool, "stub", "", "", storage.S3Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No local object exists for this fresh UUID. StoredChecksum performs a real
	// filesystem read and fails; the upload-stream hash must not be substituted.
	if err := svc.activateUploadedImage(t.Context(), id, 1024, true, "original-stream-checksum"); err == nil {
		t.Fatal("activated an unreadable normalized image")
	}
	if got := imageColumn(t, id, "status"); got != "saving" {
		t.Errorf("status=%s; checksum failure must not activate the image", got)
	}
	if got := imageColumn(t, id, "checksum"); got != "" {
		t.Errorf("recorded an unverified checksum %q", got)
	}
}
