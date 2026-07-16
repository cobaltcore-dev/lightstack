package keystone

import (
	"net/http"
	"strings"
	"testing"
)

// TestIdentityAuthzPrecedence (US2, FR-009) asserts that on authz-gated routes,
// authorization is decided before id validation: a non-admin gets 403 even for a
// malformed id, while an admin/owner gets the Keystone-verbatim 404 (validated
// in-handler, after the authz check — not by the pre-handler middleware).
func TestIdentityAuthzPrecedence(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	const bad = "not-a-uuid"
	nonAdmin := map[string]string{"X-Test-Roles": "member", "X-Test-User": "00000000-0000-0000-0000-0000000000aa"}

	// non-admin + malformed id -> 403 (authz precedes the not-found)
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/v3/users/" + bad},
		{"POST", "/v3/users/" + bad + "/password"},
		{"POST", "/v3/users/" + bad + "/credentials/OS-EC2"},
		{"GET", "/v3/users/" + bad + "/credentials/OS-EC2"},
	} {
		if code, body := doReq(r, tc.method, tc.path, nonAdmin); code != http.StatusForbidden {
			t.Errorf("non-admin %s %s: code=%d body=%s; want 403", tc.method, tc.path, code, body)
		}
	}

	// admin + malformed id -> verbatim 404 (in-handler check after authz)
	for _, tc := range []struct{ method, path, noun string }{
		{"DELETE", "/v3/users/" + bad, "user"},
		{"GET", "/v3/users/" + bad + "/credentials/OS-EC2", "user"},
	} {
		code, body := doReq(r, tc.method, tc.path, adminHdr)
		if code != http.StatusNotFound || !strings.Contains(body, "Could not find "+tc.noun+": "+bad+".") {
			t.Errorf("admin %s %s: code=%d body=%s; want 404 verbatim", tc.method, tc.path, code, body)
		}
	}
}
