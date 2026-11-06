package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/cobaltcore-dev/lightstack/internal/health"
	"github.com/cobaltcore-dev/lightstack/internal/nova"
)

// SPEC-031 FR-017, FR-018a: in real mode readiness also waits for the hypervisor — the
// console socket directory prepared once — and stub mode's readiness is unchanged.
func TestReadinessChecks(t *testing.T) {
	names := func(checks []health.Check) []string {
		var out []string
		for _, c := range checks {
			out = append(out, c.Name)
		}
		return out
	}
	svc := nova.NewService(nil, "", "real", nil)
	if got := names(readinessChecks(nil, svc, "real")); !slices.Equal(got, []string{"database", "hypervisor"}) {
		t.Errorf("real mode checks %v; want [database hypervisor]", got)
	}
	if got := names(readinessChecks(nil, svc, "stub")); !slices.Equal(got, []string{"database"}) {
		t.Errorf("stub mode checks %v; want [database]", got)
	}

	readyz := func(checks ...health.Check) (int, string) {
		gin.SetMode(gin.TestMode)
		c := health.New(zerolog.Nop(), checks...)
		c.MarkStarted()
		r := gin.New()
		c.Register(r)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	hypervisorCheck := health.Check{Name: "hypervisor", Probe: svc.HypervisorReady}
	if code, body := readyz(hypervisorCheck); code != http.StatusServiceUnavailable || body != "hypervisor unavailable" {
		t.Errorf("/readyz = %d %q; want 503 \"hypervisor unavailable\"", code, body)
	}
	dbDown := health.Check{Name: "database", Probe: func(context.Context) error { return errors.New("down") }}
	if code, body := readyz(dbDown, hypervisorCheck); code != http.StatusServiceUnavailable || body != "database,hypervisor unavailable" {
		t.Errorf("/readyz = %d %q; want 503 \"database,hypervisor unavailable\"", code, body)
	}
}
