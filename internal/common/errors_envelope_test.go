package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-024 (cloud-storage#546) US2 / B10, FR-010.
//
// The Neutron envelope work adds a second rendering to this package. These are the guard that it
// stays a *second* one: every other service keeps the body it emits today, byte for byte. Unlike the
// rest of the feature's tests these are NOT expected to go red first — they pass before the change
// and must keep passing after it. A failure here means the envelope selection leaked out of Neutron.

// renderError runs SendError through a bare gin context, as a handler would, and returns the status
// and decoded body. Nothing marks the request, so this is the default (compute) rendering.
func renderError(t *testing.T, osErr *OpenStackError) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	SendError(c, osErr)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, w.Body.String())
	}
	return w.Code, body
}

// TestDefaultEnvelopeIsUnchanged pins the compute/identity/block-storage/image shape: the error is
// keyed by Code, and the inner object carries `message` plus the numeric `code`. Neutron is the only
// service that departs from this, and only when its own marker is present.
func TestDefaultEnvelopeIsUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		err      *OpenStackError
		wantKey  string
		wantCode int
	}{
		{"not found", NewNotFoundError("network"), "itemNotFound", http.StatusNotFound},
		{"resource not found", NewResourceNotFoundError("server", "abc"), "itemNotFound", http.StatusNotFound},
		{"bad request", NewBadRequestError("nope"), "badRequest", http.StatusBadRequest},
		{"unauthorized", NewUnauthorizedError("nope"), "unauthorized", http.StatusUnauthorized},
		{"forbidden", NewForbiddenError("nope"), "forbidden", http.StatusForbidden},
		{"conflict", NewConflictError("nope"), "conflict", http.StatusConflict},
		{"internal", NewInternalServerError("nope"), "computeFault", http.StatusInternalServerError},
		{"method not allowed", NewMethodNotAllowedError("DELETE"), "badMethod", http.StatusMethodNotAllowed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := renderError(t, tc.err)

			if status != tc.wantCode {
				t.Errorf("status = %d, want %d", status, tc.wantCode)
			}
			if len(body) != 1 {
				t.Fatalf("expected exactly one top-level key, got %v", keysOf(body))
			}
			inner, ok := body[tc.wantKey].(map[string]any)
			if !ok {
				t.Fatalf("expected envelope key %q, got %v", tc.wantKey, keysOf(body))
			}
			if _, ok := inner["message"]; !ok {
				t.Errorf("inner object lost its `message` key: %v", keysOf(inner))
			}
			// The numeric `code` key is what distinguishes this envelope from Neutron's. Its
			// presence here is required, and its absence on a Neutron response is required too.
			code, ok := inner["code"]
			if !ok {
				t.Fatalf("inner object lost its `code` key: %v", keysOf(inner))
			}
			if int(code.(float64)) != tc.wantCode {
				t.Errorf("inner code = %v, want %d", code, tc.wantCode)
			}
			if _, unexpected := inner["type"]; unexpected {
				t.Errorf("default envelope must not carry Neutron's `type` key: %v", keysOf(inner))
			}
		})
	}
}

// TestDefaultEnvelopeCarriesDetailsAsDetails guards the other half of the shape difference: this
// envelope spells the optional field `details`, Neutron's spells it `detail` and always emits it.
func TestDefaultEnvelopeCarriesDetailsAsDetails(t *testing.T) {
	_, body := renderError(t, NewBadRequestErrorWithDetails("bad", "because reasons"))

	inner, ok := body["badRequest"].(map[string]any)
	if !ok {
		t.Fatalf("expected badRequest envelope, got %v", keysOf(body))
	}
	if got := inner["details"]; got != "because reasons" {
		t.Errorf("details = %v, want %q", got, "because reasons")
	}
	if _, unexpected := inner["detail"]; unexpected {
		t.Error("default envelope must not use Neutron's singular `detail` spelling")
	}
}

// TestResourceCarriedForTypeDerivation covers the additive field the Neutron type table reads
// (FR-007). It is populated by the resource-taking constructors and must not change any rendering:
// the assertions above already pin the body, so this only checks the value is available.
func TestResourceCarriedForTypeDerivation(t *testing.T) {
	cases := map[string]*OpenStackError{
		"network": NewNotFoundError("network"),
		"subnet":  NewResourceNotFoundError("subnet", "abc"),
	}
	for want, err := range cases {
		if err.Resource != want {
			t.Errorf("Resource = %q, want %q — the type table cannot name the condition without it", err.Resource, want)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
