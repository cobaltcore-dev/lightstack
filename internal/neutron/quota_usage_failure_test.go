package neutron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

// serveWithTableLocked serves one request while table is unavailable (dbtest.Unavailable), so
// any statement touching it fails — a stand-in for a database that cannot answer. Statements on
// other tables still succeed, which is what lets a test get past the project and limit lookups
// to the usage counts.
func serveWithTableLocked(t *testing.T, r http.Handler, table, method, path string, headers map[string]string) (int, string) {
	t.Helper()
	ctx := context.Background()
	release := dbtest.Unavailable(t, testPool, table)
	defer release()
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
	r := newNeutronTestRouter()
	path := "/v2.0/quotas/" + seededProjectID + "/details"
	if code, body := serveWithTableLocked(t, r, "networks", http.MethodGet, path, neutronAuthHdr(seededProjectID, true)); code != http.StatusInternalServerError {
		t.Errorf("GET %s with the usage count failing: code=%d body=%.160s; want 500, not invented usage", path, code, body)
	}
}
