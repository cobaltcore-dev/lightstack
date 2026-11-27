package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-033 round 2, T062 (research R39): an error can carry the exact upstream exception class
// Neutron's envelope names. Upstream raises several 409s about one resource — a security group in
// use, the default group that cannot be removed, renamed or repeated — so a 409 is named at the call
// site, and one without a type is a Conflict whatever its resource (SPEC-033 T216). An explicit
// type wins over the tables; errors without one render as before.

// renderNeutron runs SendError through a request marked for Neutron's envelope and returns the
// status and the NeutronError object.
func renderNeutron(t *testing.T, osErr *OpenStackError) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	MarkNeutronErrors(c)
	SendError(c, osErr)

	var body map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, w.Body.String())
	}
	inner, ok := body["NeutronError"]
	if !ok {
		t.Fatalf("no NeutronError envelope in %q", w.Body.String())
	}
	return w.Code, inner
}

func TestNeutronExplicitTypeWins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      *OpenStackError
		status   int
		wantType string
	}{
		{
			"a 409 about a security group, typed",
			NewNeutronError(http.StatusConflict, "SecurityGroupCannotRemoveDefault", "Insufficient rights for removing default security group."),
			http.StatusConflict, "SecurityGroupCannotRemoveDefault",
		},
		{
			"a 409 typed with a resource the in-use table knows",
			func() *OpenStackError {
				e := NewNeutronError(http.StatusConflict, "SecurityGroupDefaultAlreadyExists", "Default security group already exists.")
				e.Resource = "security group"
				return e
			}(),
			http.StatusConflict, "SecurityGroupDefaultAlreadyExists",
		},
		{
			"a 400",
			NewNeutronError(http.StatusBadRequest, "InvalidAllocationPool", "The allocation pool 10.0.0.9-10.0.0.1 is not valid."),
			http.StatusBadRequest, "InvalidAllocationPool",
		},
		{
			"a 404",
			NewNeutronError(http.StatusNotFound, "ExternalGatewayForFloatingIPNotFound", "External network is not reachable."),
			http.StatusNotFound, "ExternalGatewayForFloatingIPNotFound",
		},
		{
			"a 412",
			NewNeutronError(http.StatusPreconditionFailed, "RevisionNumberConstraintFailed", "Constrained to 1, but current revision is 2."),
			http.StatusPreconditionFailed, "RevisionNumberConstraintFailed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, inner := renderNeutron(t, tc.err)
			if status != tc.status {
				t.Errorf("status = %d; want %d", status, tc.status)
			}
			if inner["type"] != tc.wantType {
				t.Errorf("type = %v; want %s", inner["type"], tc.wantType)
			}
			if inner["message"] != tc.err.Message {
				t.Errorf("message = %v; want %q", inner["message"], tc.err.Message)
			}
		})
	}
}

// Errors without an explicit type keep the tables' names (lock).
func TestNeutronTypeWithoutExplicitTypeUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      *OpenStackError
		wantType string
	}{
		{"a resource's conflict", &OpenStackError{StatusCode: http.StatusConflict, Code: "conflictingRequest", Message: "in use", Resource: "security group"}, "Conflict"},
		{"not found, by resource", NewNotFoundError("security group"), "SecurityGroupNotFound"},
		{"plain conflict", NewConflictError("nope"), "Conflict"},
		{"bad request", NewBadRequestError("nope"), "BadRequest"},
		{"internal", NewInternalServerError("nope"), "NeutronException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, inner := renderNeutron(t, tc.err)
			if inner["type"] != tc.wantType {
				t.Errorf("type = %v; want %s", inner["type"], tc.wantType)
			}
		})
	}
}

// The other envelope renders a typed error by its status, as any other error of that status.
func TestNeutronTypedErrorKeepsTheDefaultEnvelope(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantKey string
	}{
		{http.StatusBadRequest, "badRequest"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "itemNotFound"},
		{http.StatusConflict, "conflict"},
	} {
		status, body := renderError(t, NewNeutronError(tc.status, "SomeType", "message"))
		if status != tc.status {
			t.Errorf("status = %d; want %d", status, tc.status)
		}
		if _, ok := body[tc.wantKey]; !ok {
			t.Errorf("default envelope for %d keyed %v; want %q", tc.status, keysOf(body), tc.wantKey)
		}
	}
}
