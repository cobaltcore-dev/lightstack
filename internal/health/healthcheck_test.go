package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHealthcheck_GETandHEAD: oslo/Octavia-compatible alias answers GET + HEAD.
func TestHealthcheck_GETandHEAD(t *testing.T) {
	c := startedChecker(okCheck("database"))

	g := doReq(c, http.MethodGet, "/healthcheck")
	if g.Code != http.StatusOK || strings.TrimSpace(g.Body.String()) != "OK" {
		t.Fatalf("GET /healthcheck = %d %q, want 200 OK", g.Code, g.Body.String())
	}

	h := doReq(c, http.MethodHead, "/healthcheck")
	if h.Code != http.StatusOK || h.Body.Len() != 0 {
		t.Fatalf("HEAD /healthcheck = %d body=%q, want 200 with no body", h.Code, h.Body.String())
	}
}

// TestHealthcheck_JSONNegotiation: Accept: application/json → JSON body.
func TestHealthcheck_JSONNegotiation(t *testing.T) {
	c := startedChecker(okCheck("database"))
	r := gin.New()
	c.Register(r)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthcheck", nil)
	req.Header.Set("Accept", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("json /healthcheck = %d, want 200", w.Code)
	}
	var body struct {
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, w.Body.String())
	}
	if len(body.Reasons) == 0 || body.Reasons[0] != "OK" {
		t.Fatalf("reasons = %+v, want [OK]", body.Reasons)
	}
}

// TestHealthcheck_MirrorsReadiness: DB down → 503, same as /readyz.
func TestHealthcheck_MirrorsReadiness(t *testing.T) {
	w := doReq(startedChecker(failCheck("database", errors.New("down"))), http.MethodGet, "/healthcheck")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthcheck (DB down) = %d, want 503", w.Code)
	}
}
