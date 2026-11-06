package glance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Records the largest read buffer without allocating a large request body.
type stageReader struct {
	remaining int64
	maxRead   int
}

func (r *stageReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	clear(p[:n])
	r.remaining -= int64(n)
	return n, nil
}

func TestStageImageStreamsWithinLimit(t *testing.T) {
	requireDB(t)
	const size = 8 << 20
	origCap := maxImageUploadBytes
	maxImageUploadBytes = size
	t.Cleanup(func() { maxImageUploadBytes = origCap })
	router := newGlanceTestRouter(t)
	for _, extra := range []int64{0, 1} {
		id := uuid.NewString()
		if _, err := testPool.Exec(t.Context(), `INSERT INTO images (id, name, project_id, status, visibility)
			VALUES ($1, 'stage-limit', $2, 'queued', 'private')`, id, seededProjectID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM images WHERE id=$1", id) })
		reader := &stageReader{remaining: size + extra}
		req := httptest.NewRequest(http.MethodPut, "/images/"+id+"/stage", reader)
		for k, v := range hdr(seededProjectID, "member") {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		wantCode, wantStatus := http.StatusNoContent, "uploading"
		if extra != 0 {
			wantCode, wantStatus = http.StatusRequestEntityTooLarge, "queued"
		}
		if w.Code != wantCode {
			t.Errorf("stage: %d %s; want %d", w.Code, w.Body.String(), wantCode)
		}
		if reader.maxRead > 64<<10 {
			t.Errorf("stage allocated a %d-byte read buffer; want bounded streaming", reader.maxRead)
		}
		var status string
		var storedSize int64
		if err := testPool.QueryRow(t.Context(), "SELECT status, COALESCE(size_bytes, 0) FROM images WHERE id=$1", id).Scan(&status, &storedSize); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus {
			t.Errorf("status=%s; want %s", status, wantStatus)
		}
		if extra == 0 && storedSize != size {
			t.Errorf("size=%d; want %d", storedSize, size)
		}
	}
}
