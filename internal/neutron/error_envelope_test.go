package neutron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) US2 / B10 — Neutron answers in Neutron's error envelope.
//
// Upstream builds the body in neutron/api/api_common.py:
//
//	body = serializer.serialize({'NeutronError': get_exception_data(e)})
//	err_data = {'type': e.__class__.__name__, 'message': e, 'detail': ''}
//
// So: wrapper key `NeutronError`, inner keys exactly type/message/detail, and NO numeric `code` —
// that last one is the compute envelope's field and its presence is the defect.
//
// These build a router that mirrors createNeutronServer (cmd/lightstack/main.go:626-637) rather than the
// package's shared newNeutronTestRouter, because half the point is the paths that never enter a
// route group: an abort from engine-level middleware, NoRoute, and NoMethod. A marker registered on
// the route group would satisfy the handler cases below and silently miss those three.

const absentID = "00000000-0000-0000-0000-999999999999"

// newEnvelopeRouter mirrors the production middleware order for the Neutron engine. The envelope
// marker goes on FIRST — ahead of error handling — so panic recovery renders through it too.
func newEnvelopeRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ErrorEnvelope())
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(func(c *gin.Context) {
		c.Set("project_id", seededProjectID)
		c.Next()
	})
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())
	NewService(testPool, "stub", nil).RegisterRoutes(&r.RouterGroup)
	return r
}

// neutronError decodes a response that must be in Neutron's envelope, failing with the body when it
// is not. Returns the inner object so callers can assert on type/message/detail.
func neutronError(t *testing.T, body string) map[string]any {
	t.Helper()

	var top map[string]any
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, body)
	}
	if len(top) != 1 {
		t.Fatalf("expected exactly one top-level key, got %v (body %q)", sortedKeys(top), body)
	}
	inner, ok := top["NeutronError"].(map[string]any)
	if !ok {
		t.Fatalf("expected wrapper key `NeutronError`, got %v — this is B10 (body %q)", sortedKeys(top), body)
	}
	return inner
}

// assertNeutronShape checks the inner field set: exactly type/message/detail, and no `code`.
// The absence of `code` is the assertion that catches a wrapper-only half-fix (FR-008): renaming
// `itemNotFound` to `NeutronError` while keeping the compute body would pass a wrapper check and
// fail this one.
func assertNeutronShape(t *testing.T, inner map[string]any, wantType string) {
	t.Helper()

	if _, present := inner["code"]; present {
		t.Errorf("Neutron envelope must not carry the numeric `code` key; upstream has no such field (got %v)", sortedKeys(inner))
	}
	for _, k := range []string{"type", "message", "detail"} {
		if _, present := inner[k]; !present {
			t.Errorf("missing required inner key %q (got %v)", k, sortedKeys(inner))
		}
	}
	if len(inner) != 3 {
		t.Errorf("expected exactly 3 inner keys (type, message, detail), got %v", sortedKeys(inner))
	}
	if got := inner["type"]; got != wantType {
		t.Errorf("type = %v, want %q", got, wantType)
	}
	if msg, _ := inner["message"].(string); strings.TrimSpace(msg) == "" {
		t.Error("message must be non-empty and human-readable")
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestNeutronNotFoundUsesNeutronEnvelope — contracts E1 and E2.
func TestNeutronNotFoundUsesNeutronEnvelope(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+absentID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", code, body)
	}
	assertNeutronShape(t, neutronError(t, body), "NetworkNotFound")
}

// TestNeutronNotFoundNamesTheConditionPerResource — contract E3. Every resource names its own
// condition, the way upstream's exception classes do; one shared value would not let a client tell
// an absent network from an absent port.
func TestNeutronNotFoundNamesTheConditionPerResource(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	for _, tc := range []struct{ path, wantType string }{
		{"/v2.0/networks/" + absentID, "NetworkNotFound"},
		{"/v2.0/subnets/" + absentID, "SubnetNotFound"},
		{"/v2.0/ports/" + absentID, "PortNotFound"},
		{"/v2.0/routers/" + absentID, "RouterNotFound"},
		{"/v2.0/floatingips/" + absentID, "FloatingIPNotFound"},
		{"/v2.0/security-groups/" + absentID, "SecurityGroupNotFound"},
		{"/v2.0/security-group-rules/" + absentID, "SecurityGroupRuleNotFound"},
	} {
		t.Run(tc.wantType, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, tc.path, nil)
			if code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %s)", code, body)
			}
			assertNeutronShape(t, neutronError(t, body), tc.wantType)
		})
	}
}

// TestNeutronInUseNamesTheCondition — FR-007's in-use conditions, and what counts as in use.
//
// DeleteNetwork's refusals were built without a resource, so neutronType fell through to the generic
// Conflict and its in-use table was never consulted. Upstream raises NetworkInUse while any port
// other than its own automatic ones remains, and a floating IP or a router gateway is such a port
// there. Here each is a row of its own, so each is a case of its own. Subnets are not a case:
// upstream deletes them with the network (TestDeleteNetworkTakesItsSubnets).
func TestNeutronInUseNamesTheCondition(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	for _, tc := range []struct {
		name string
		seed func(t *testing.T, networkID string)
	}{
		{"active port", func(t *testing.T, networkID string) {
			mustExec(t, `INSERT INTO ports (name, network_id, project_id, mac_address)
				VALUES ('spec024-in-use', $1, $2, 'fa:16:3e:00:00:02')`, networkID, seededProjectID)
		}},
		{"floating IP", func(t *testing.T, networkID string) {
			mustExec(t, `INSERT INTO floating_ips (project_id, floating_network_id, floating_ip_address)
				VALUES ($1, $2, '203.0.113.77')`, seededProjectID, networkID)
		}},
		{"router gateway", func(t *testing.T, networkID string) {
			routerID := uuid.NewString()
			mustExec(t, `INSERT INTO routers (id, name, project_id, external_gateway_info)
				VALUES ($1, 'spec024-in-use', $2, jsonb_build_object('network_id', $3::text))`,
				routerID, seededProjectID, networkID)
			cleanupRows(t, "routers", routerID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedNetwork(t, "spec024-in-use-"+uuid.NewString()[:8])
			tc.seed(t, id)

			code, body := doReq(r, http.MethodDelete, "/v2.0/networks/"+id, nil)
			if code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "NetworkInUse")
		})
	}
}

// TestNeutronEnvelopeOnPathsThatNeverEnterAHandler — contracts E5, E6 and E7.
//
// 🔑 This is the test that pins the marker to engine level. Auth aborts before the route group
// exists, and NoRoute/NoMethod never run inside a group at all, so all three would keep the compute
// envelope if the marker were registered on the group.
func TestNeutronEnvelopeOnPathsThatNeverEnterAHandler(t *testing.T) {
	t.Run("E6 unrouted path", func(t *testing.T) {
		r := newEnvelopeRouter()
		code, body := doReq(r, http.MethodGet, "/v2.0/nonexistent", nil)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, body)
		}
		assertNeutronShape(t, neutronError(t, body), "NotFound")
	})

	t.Run("E7 method not allowed", func(t *testing.T) {
		r := newEnvelopeRouter()
		code, body := doReq(r, http.MethodPatch, "/v2.0/security-group-rules", nil)
		if code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405 (body %s)", code, body)
		}
		assertNeutronShape(t, neutronError(t, body), "BadRequest")
	})

	// E5: an abort from engine-level middleware, which is the shape of the real auth failure
	// (middleware/auth.go:32,39 → common.AbortWithError → common.SendError). Wiring a real
	// AuthService here would test keystone, not the envelope; the live unauthenticated call is
	// covered by the gophercloud contract test.
	t.Run("E5 abort from engine-level middleware", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(ErrorEnvelope())
		r.Use(func(c *gin.Context) {
			common.AbortWithError(c, common.NewUnauthorizedError("authentication required"))
		})
		NewService(testPool, "stub", nil).RegisterRoutes(&r.RouterGroup)

		code, body := doReq(r, http.MethodGet, "/v2.0/networks", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body %s)", code, body)
		}
		assertNeutronShape(t, neutronError(t, body), "NotAuthorized")
	})
}

// TestNeutronEnvelopeForBadRequestAndInternalFault — contracts E4 and E8.
//
// The internal case is where FR-007a lands: "failed to create bridge" has no upstream counterpart,
// so it gets the one documented generic rather than an invented per-site name. Critically it must
// NOT keep leaking `computeFault`, which is a *compute* name on a network response.
func TestNeutronEnvelopeForBadRequestAndInternalFault(t *testing.T) {
	t.Run("E4 malformed body", func(t *testing.T) {
		requireDB(t)
		r := newEnvelopeRouter()
		code, body := doReqBody(r, http.MethodPost, "/v2.0/networks", `{`, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		assertNeutronShape(t, neutronError(t, body), "BadRequest")
	})

	// The middleware set here is EXACTLY the one cmd/lightstack/main.go:createNeutronServer registers, in
	// order, and that is the whole point of the test.
	//
	// It used to register only ErrorEnvelope + ErrorHandlingMiddleware, omitting RecoveryMiddleware —
	// which is the middleware that decides the outcome. Deferred recovers run innermost-first, so
	// RecoveryMiddleware, registered last and therefore closest to the handler, recovers before
	// ErrorHandlingMiddleware ever sees the panic. It used to end in c.AbortWithStatus(500), which
	// writes NO body, so production answered a panic with an empty 500 while this test asserted a
	// NeutronError and passed. The test could not fail (branch review 2026-08-13, SRC-2).
	t.Run("E8 internal fault via recovered panic", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(ErrorEnvelope())
		r.Use(middleware.ErrorHandlingMiddleware())
		r.Use(middleware.LoggingMiddleware())
		r.Use(middleware.RecoveryMiddleware())
		r.GET("/boom", func(c *gin.Context) { panic("injected") })

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", w.Code, w.Body.String())
		}
		if w.Body.Len() == 0 {
			t.Fatal("a recovered panic answered with an EMPTY body; clients parsing the Neutron " +
				"envelope get nothing at all, and FR-008 requires every failing path to carry it")
		}
		inner := neutronError(t, w.Body.String())
		assertNeutronShape(t, inner, "NeutronException")
	})
}

// TestAutoAllocatedTopologyUsesTheSharedEnvelopePath — contract E9 / FR-009.
//
// This endpoint hand-rolled the correct NeutronError long before the rest of the service did
// (auto_allocated_topology.go:30-34). Once the shared path exists that block is a second description
// of the same envelope, free to drift — exactly the defect B9 is. It must be gone, and the response
// must be indistinguishable in shape from every other 404.
func TestAutoAllocatedTopologyUsesTheSharedEnvelopePath(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	code, body := doReq(r, http.MethodGet, "/v2.0/auto-allocated-topology/"+seededProjectID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", code, body)
	}
	inner := neutronError(t, body)
	// The hand-rolled block emitted type/message/detail with no `code`, so the shape alone cannot
	// prove it is gone. The full-key-set assertion plus the shared type vocabulary can.
	assertNeutronShape(t, inner, "AutoAllocationFailure")
}
