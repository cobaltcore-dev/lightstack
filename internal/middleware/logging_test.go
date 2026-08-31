package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

// TestLoggingMiddleware_SkipsHealthProbes: the k8s health endpoints are polled
// every few seconds and must not be access-logged (SPEC-013); ordinary requests
// still are.
func TestLoggingMiddleware_SkipsHealthProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := logger
	logger = zerolog.New(&buf)
	t.Cleanup(func() { logger = orig })

	r := gin.New()
	r.Use(LoggingMiddleware())
	r.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/livez", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/healthcheck", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/v3", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, p := range []string{"/readyz", "/livez", "/healthcheck"} {
		buf.Reset()
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
		if buf.Len() != 0 {
			t.Fatalf("%s was logged but should be skipped: %q", p, buf.String())
		}
	}

	// A normal request IS logged.
	buf.Reset()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v3", nil))
	if buf.Len() == 0 {
		t.Fatal("/v3 produced no log line; LoggingMiddleware should log non-probe paths")
	}
}

// SPEC-025 US8 (cloud-storage#547 B15): the access log must name the operator who
// acted, not the tenant acted upon.
//
// "project_id" is the EFFECTIVE project a handler scopes its queries by, and the compute
// action dispatcher deliberately rebinds it to the owning project so an administrator's
// action lands in the right tenant (SPEC-023 FR-007). The log read that field AFTER the
// handler ran, so a cross-tenant action was attributed to the target — the one field an
// operator greps when asking who touched a tenant's server.
//
// Removing the rebind would "fix" the log and break the override, so the fix is here.

// completedLine runs a request through the middleware and returns the decoded
// "request completed" line.
func completedLine(t *testing.T, handler gin.HandlerFunc) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := logger
	logger = zerolog.New(&buf)
	t.Cleanup(func() { logger = orig })

	r := gin.New()
	r.Use(LoggingMiddleware())
	r.GET("/v2.1/servers/x/action", handler)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2.1/servers/x/action", nil))

	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var line map[string]any
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		if line["message"] == "request completed" {
			return line
		}
	}
	t.Fatalf("no \"request completed\" line in %q", buf.String())
	return nil
}

const (
	callerProj = "00000000-0000-0000-0000-00000000caaa"
	targetProj = "00000000-0000-0000-0000-00000000tbbb"
	actingUser = "00000000-0000-0000-0000-00000000u111"
)

// T108/T110 (FR-045, FR-046, FR-047): a cross-project action names the caller, carries
// the target alongside it, and leaves user_id alone.
func TestLoggingNamesTheCallerNotTheTarget(t *testing.T) {
	line := completedLine(t, func(c *gin.Context) {
		// What the auth middleware does.
		c.Set("user_id", actingUser)
		c.Set("project_id", callerProj)
		c.Set(CallerProjectKey, callerProj)
		// What the compute action dispatcher does: rebind the effective project to the
		// server's owner so every sub-handler scopes to it.
		c.Set("project_id", targetProj)
		c.Status(http.StatusAccepted)
	})

	if got := line["project_id"]; got != callerProj {
		t.Errorf("project_id = %v; want the CALLER %q — the log attributed the action to the tenant acted upon (FR-045)", got, callerProj)
	}
	if got := line["target_project_id"]; got != targetProj {
		t.Errorf("target_project_id = %v; want %q — the project acted upon must be identifiable from this line alone (FR-046, SC-012)", got, targetProj)
	}
	if got := line["user_id"]; got != actingUser {
		t.Errorf("user_id = %v; want %q unchanged (FR-047)", got, actingUser)
	}
}

// T109 (FR-047, FR-046a): an ordinary same-project request logs exactly as before — no
// extra field appears. The target field is present only when it differs, so every line
// that was already correct is byte-identical.
func TestLoggingSameProjectRequestIsUnchanged(t *testing.T) {
	line := completedLine(t, func(c *gin.Context) {
		c.Set("user_id", actingUser)
		c.Set("project_id", callerProj)
		c.Set(CallerProjectKey, callerProj)
		c.Status(http.StatusOK)
	})

	if got := line["project_id"]; got != callerProj {
		t.Errorf("project_id = %v; want %q", got, callerProj)
	}
	if _, present := line["target_project_id"]; present {
		t.Error("target_project_id appeared on a same-project request; it must be present only when the two differ (FR-046a)")
	}
}

// The existing caller field must never be renamed or dropped — it is what operators
// already grep, so a request with no rebinding at all still has to carry it.
func TestLoggingKeepsProjectFieldWithoutACallerRecord(t *testing.T) {
	line := completedLine(t, func(c *gin.Context) {
		// Only project_id, as an unauthenticated path or an older wiring would leave it.
		c.Set("project_id", callerProj)
		c.Status(http.StatusOK)
	})

	if got := line["project_id"]; got != callerProj {
		t.Errorf("project_id = %v; the field must survive even with no caller recorded", got)
	}
	if _, present := line["target_project_id"]; present {
		t.Error("target_project_id appeared with no caller recorded; there is nothing to compare against")
	}
}

// A request with no identity at all must still log, without inventing fields.
func TestLoggingUnauthenticatedRequestHasNoProjectFields(t *testing.T) {
	line := completedLine(t, func(c *gin.Context) { c.Status(http.StatusUnauthorized) })

	if _, present := line["project_id"]; present {
		t.Error("project_id appeared for a request with no identity")
	}
	if _, present := line["target_project_id"]; present {
		t.Error("target_project_id appeared for a request with no identity")
	}
}
