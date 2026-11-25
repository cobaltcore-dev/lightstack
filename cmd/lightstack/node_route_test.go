package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/keystone"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
)

// SPEC-033 T210: the Neutron server routes the node call ahead of the user-token
// middleware, through the real wiring, so the call reaches its own node-token check — its own 401 —
// while every other route still asks for a user's token.
func TestNeutronServerRoutesTheNodeCallAheadOfUserAuth(t *testing.T) {
	svc := neutron.NewService(nil, "stub", nil)
	svc.SetNodes(neutron.NodeOptions{Name: "node-a", Secret: []byte("node-secret")})
	srv := createNeutronServer(&common.Config{}, svc, keystone.NewAuthService(nil, "user-secret", time.Hour))
	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		return w
	}
	if w := do(http.MethodPost, "/internal/v1/converge"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "node token refused") {
		t.Errorf("a node call without a node token: %d %s; want the call's own 401, node token refused", w.Code, w.Body.String())
	}
	if w := do(http.MethodGet, "/v2.0/networks"); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "node token") {
		t.Errorf("a user route without a token: %d %s; want the middleware's 401", w.Code, w.Body.String())
	}
}
