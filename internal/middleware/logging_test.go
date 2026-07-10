package middleware

import (
	"bytes"
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
