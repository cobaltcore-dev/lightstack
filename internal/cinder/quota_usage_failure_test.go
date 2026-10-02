package cinder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveWithTableLocked serves one request while another connection holds an exclusive lock on
// table, so any statement touching it blocks until the request's deadline and fails — a
// stand-in for a database that cannot answer. Statements on other tables still succeed, which
// is what lets a test get past the project and limit lookups to the usage counts. Other
// packages' statements on the table only wait for the lock; none of them is failed.
func serveWithTableLocked(t *testing.T, r http.Handler, table, method, path string, headers map[string]string) (int, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // only releases the lock
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(method, path, nil).WithContext(reqCtx)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestUsageFailureIs500 — A usage count that fails is an error, not zero. Every quota and limits view used to report
// a failed count as zero usage with a 200, so during a database outage the dashboard showed a
// project consuming nothing and an operator could size limits from invented numbers. Raised on
// PR #79; enforcement (CheckQuota) already failed closed.
func TestUsageFailureIs500(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)
	for _, path := range []string{
		"/v3/quota-sets/" + seededProjectID + "?usage=true",
		"/v3/limits",
	} {
		if code, body := serveWithTableLocked(t, r, "volumes", http.MethodGet, path, hdr); code != http.StatusInternalServerError {
			t.Errorf("GET %s with the usage count failing: code=%d body=%.160s; want 500, not invented usage", path, code, body)
		}
	}
}
