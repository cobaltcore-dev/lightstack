package storage

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestDownloadImageS3_StreamsPastConnectTimeout proves that an S3/RGW download
// is NOT bounded by a total-operation timeout: the fail-fast budget covers only
// connect + time-to-first-byte, so a body that trickles in over much longer than
// that budget still streams to completion (SPEC-014 FR-014 / SC-002).
//
// The fake endpoint flushes response headers immediately (within the first-byte
// budget), then writes the body one byte at a time over ~1s while the configured
// connect/first-byte budget is only 300ms. A total-op timeout would truncate it.
func TestDownloadImageS3_StreamsPastConnectTimeout(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	const body = "streamed-object-body-longer-than-the-first-byte-budget"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		fl.Flush() // headers arrive well within the first-byte budget
		for i := range len(body) {
			_, _ = w.Write([]byte{body[i]})
			fl.Flush()
			time.Sleep(20 * time.Millisecond) // total ~1s, far beyond the 300ms budget
		}
	}))
	defer srv.Close()

	s, err := NewImageStore("s3", "images", "", S3Options{
		Bucket:         "b",
		Region:         "us-east-1",
		Endpoint:       srv.URL,
		ConnectTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewImageStore: %v", err)
	}

	var buf bytes.Buffer
	start := time.Now()
	if err := s.DownloadImage(context.Background(), "obj", &buf); err != nil {
		t.Fatalf("streamed download should complete, got error: %v", err)
	}
	if buf.String() != body {
		t.Fatalf("download body = %q, want %q", buf.String(), body)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("download finished in %v — too fast, the slow-body path was not exercised", elapsed)
	}
}
