package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Every server's engine trusts no proxy. gin otherwise believes X-Forwarded-For and
// X-Real-IP from any caller, so ClientIP(), which the request log records as remote_addr
// and the s3tokens denial log as client_ip, was whatever address the caller chose.
func TestNewEngineTrustsNoProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		t.Run(header, func(t *testing.T) {
			r := newEngine()
			r.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = "10.0.0.5:40000"
			req.Header.Set(header, "198.51.100.7")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if got := w.Body.String(); got != "10.0.0.5" {
				t.Errorf("ClientIP() = %q with %s: 198.51.100.7; want the peer, 10.0.0.5", got, header)
			}
		})
	}
}
