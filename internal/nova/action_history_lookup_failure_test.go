package nova

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestActionHistoryLookupFailureIsOneResponse — a failed server lookup answers one 500 and stops.
//
// The history read falls back to the action records when the server is not live (SPEC-025 FR-043).
// Its lookup used to report a database failure by writing a 500 AND returning the same answer as an
// absent server, so the handler carried on: the client got the 500 followed by a 404 body, or by the
// history itself when only the first lookup had failed — two JSON documents under one status.
func TestActionHistoryLookupFailureIsOneResponse(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// Another connection holds instances exclusively, so the lookup blocks until the request's
	// deadline and fails — a stand-in for a database that cannot answer.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // only releases the lock
	if _, err := tx.Exec(ctx, "LOCK TABLE instances IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock instances: %v", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/v2.1/servers/"+uuid.New().String()+"/os-instance-actions", nil).WithContext(reqCtx)
	for k, v := range map[string]string{
		"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member",
		"OpenStack-API-Version": "compute 2.21", // reaches the deleted-server fallback too
	} {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	newNovaTestRouter().ServeHTTP(w, req)
	body := w.Body.String()

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 for a lookup the database could not answer. body: %s", w.Code, body)
	}
	dec := json.NewDecoder(strings.NewReader(body))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("body is not JSON: %v; body: %s", err, body)
	}
	if _, ok := first["computeFault"]; !ok {
		t.Errorf("first document is not the 500's computeFault; body: %s", body)
	}
	var second any
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		t.Errorf("a second document followed the 500 (%v); the handler kept going. body: %s", second, body)
	}
}

// TestDeletedHistoryLookupFailureIs500 — the deleted-server fallback's own lookup failing is a
// 500 too, not an absent server. It used to log the error and answer 404, telling an owner whose
// history exists that the server never did, because the database could not answer.
func TestDeletedHistoryLookupFailureIs500(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// instances stays readable, so the live lookup finds nothing at once and the fallback runs;
	// instance_actions is held, so the fallback's lookup waits out the deadline and fails.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // only releases the lock
	if _, err := tx.Exec(ctx, "LOCK TABLE instance_actions IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock instance_actions: %v", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/v2.1/servers/"+uuid.New().String()+"/os-instance-actions", nil).WithContext(reqCtx)
	for k, v := range map[string]string{
		"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member",
		"OpenStack-API-Version": "compute 2.21",
	} {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	newNovaTestRouter().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when the deleted-server lookup cannot be answered. body: %s", w.Code, w.Body.String())
	}
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	var first, second any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("body is not JSON: %v; body: %s", err, w.Body.String())
	}
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		t.Errorf("more than one document in the body: %s", w.Body.String())
	}
}
