package health

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

func init() { gin.SetMode(gin.TestMode) }

func okCheck(name string) Check {
	return Check{Name: name, Probe: func(context.Context) error { return nil }}
}

func failCheck(name string, err error) Check {
	return Check{Name: name, Probe: func(context.Context) error { return err }}
}

// startedChecker returns a Checker with started=true (listeners bound).
func startedChecker(checks ...Check) *Checker {
	c := New(zerolog.Nop(), checks...)
	c.MarkStarted()
	return c
}

func doReq(c *Checker, method, target string) *httptest.ResponseRecorder {
	r := gin.New()
	c.Register(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// TestReadyz_DecoupledFromRequestContext: a client disconnect (a cancelled
// request context) must not cancel the DB round-trip and be misreported as 503
// (PR #70 review). The check is rooted at context.Background(), not the request.
func TestReadyz_DecoupledFromRequestContext(t *testing.T) {
	// A ctx-aware check like the real DB probe — it fails if its context is done.
	ctxAware := Check{Name: "database", Probe: func(ctx context.Context) error { return ctx.Err() }}
	c := startedChecker(ctxAware)

	r := gin.New()
	c.Register(r)
	cctx, cancel := context.WithCancel(context.Background())
	cancel() // the client already went away
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(cctx)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz with a cancelled request context = %d, want 200 — health must not depend on the request lifecycle", w.Code)
	}
}

func TestReady_NotStartedUntilListenersBound(t *testing.T) {
	c := New(zerolog.Nop(), okCheck("database")) // not started
	if r := c.Ready(context.Background()); r.Ready || r.Reason != "starting" {
		t.Fatalf("got %+v, want not-ready/starting before MarkStarted", r)
	}
	c.MarkStarted()
	if r := c.Ready(context.Background()); !r.Ready {
		t.Fatalf("got %+v, want ready after MarkStarted", r)
	}
}

func TestReady_Draining(t *testing.T) {
	c := startedChecker(okCheck("database"))
	c.MarkDraining()
	if r := c.Ready(context.Background()); r.Ready || r.Reason != "draining" {
		t.Fatalf("got %+v, want not-ready/draining", r)
	}
}

func TestReady_DBUp(t *testing.T) {
	if r := startedChecker(okCheck("database")).Ready(context.Background()); !r.Ready {
		t.Fatalf("got %+v, want ready when DB ok", r)
	}
}

func TestReady_DBDown_NamesComponent(t *testing.T) {
	r := startedChecker(failCheck("database", errors.New("down"))).Ready(context.Background())
	if r.Ready {
		t.Fatal("want not-ready when DB down")
	}
	if !strings.Contains(r.Reason, "database") {
		t.Fatalf("reason %q does not name the failing component", r.Reason)
	}
}

func TestReady_BoundedTimeout_NeverHangs(t *testing.T) {
	slow := Check{Name: "database", Probe: func(ctx context.Context) error {
		select {
		case <-time.After(10 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	c := startedChecker(slow)
	c.timeout = 100 * time.Millisecond
	start := time.Now()
	r := c.Ready(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Ready took %v — must be bounded sub-second", elapsed)
	}
	if r.Ready {
		t.Fatal("want not-ready on check timeout")
	}
}

func TestReady_ReasonHasNoSensitiveDetail(t *testing.T) {
	leaky := errors.New("dial tcp 10.1.2.3:5432: connection refused (password=hunter2)")
	r := startedChecker(failCheck("database", leaky)).Ready(context.Background())
	if strings.Contains(r.Reason, "hunter2") || strings.Contains(r.Reason, "10.1.2.3") {
		t.Fatalf("reason leaked internal detail: %q", r.Reason)
	}
}

func TestReadyzHandler(t *testing.T) {
	if w := doReq(startedChecker(okCheck("database")), http.MethodGet, "/readyz"); w.Code != http.StatusOK {
		t.Fatalf("readyz (ok) = %d, want 200", w.Code)
	}
	w := doReq(startedChecker(failCheck("database", errors.New("down"))), http.MethodGet, "/readyz")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz (down) = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "database") {
		t.Fatalf("readyz 503 body %q lacks component name", w.Body.String())
	}
}

func TestReadyzHandler_VerboseListsChecks(t *testing.T) {
	w := doReq(startedChecker(okCheck("database")), http.MethodGet, "/readyz?verbose")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "database") {
		t.Fatalf("verbose readyz = %d %q", w.Code, w.Body.String())
	}
}

func TestLivezHandler_IgnoresDB(t *testing.T) {
	w := doReq(startedChecker(failCheck("database", errors.New("down"))), http.MethodGet, "/livez")
	if w.Code != http.StatusOK {
		t.Fatalf("livez = %d, want 200 regardless of DB", w.Code)
	}
}

func TestTransitionIsLogged(t *testing.T) {
	var buf bytes.Buffer
	c := New(zerolog.New(&buf), okCheck("database"))
	c.MarkStarted()
	c.Ready(context.Background())
	if !strings.Contains(buf.String(), "readiness transition") {
		t.Fatalf("expected a transition log line, got %q", buf.String())
	}
}

func TestReadyzHandler_VerboseNotReady(t *testing.T) {
	// DB down → the verbose view names the failing check.
	w := doReq(startedChecker(failCheck("database", errors.New("down"))), http.MethodGet, "/readyz?verbose")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "[failed] database") {
		t.Fatalf("verbose (DB down) = %d %q, want 503 + [failed] database", w.Code, w.Body.String())
	}

	// Not started → the verbose view shows the lifecycle state, not a [failed] check.
	w = doReq(New(zerolog.Nop(), okCheck("database")), http.MethodGet, "/readyz?verbose")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "[not-ready] starting") {
		t.Fatalf("verbose (not started) = %d %q, want 503 + [not-ready] starting", w.Code, w.Body.String())
	}
}

func TestDatabaseCheck_Name(t *testing.T) {
	// The Probe closure needs a live DB (covered by integration), but the
	// constructor and its name are free to assert here.
	if got := DatabaseCheck(nil).Name; got != "database" {
		t.Fatalf("DatabaseCheck(nil).Name = %q, want database", got)
	}
}
