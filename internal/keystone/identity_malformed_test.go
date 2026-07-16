package keystone

import (
	"testing"
)

// TestIdentityMalformedNeverServerError (US3) is the invariant guard: a malformed
// id on any in-scope identity endpoint yields a client error (4xx) — never a 5xx.
// Admin credentials are used so authz-gated routes reach their in-handler id check
// (which must still be a 404, not the old 22P02 500).
func TestIdentityMalformedNeverServerError(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()

	const bad = "not-a-uuid"
	cases := []struct{ method, path string }{
		{"GET", "/v3/projects/" + bad},
		{"PATCH", "/v3/projects/" + bad},
		{"DELETE", "/v3/projects/" + bad},
		{"GET", "/v3/users/" + bad},
		{"PATCH", "/v3/users/" + bad},
		{"DELETE", "/v3/users/" + bad},
		{"POST", "/v3/users/" + bad + "/password"},
		{"GET", "/v3/users/" + bad + "/projects"},
		{"GET", "/v3/users/" + bad + "/groups"},
		{"GET", "/v3/users/" + bad + "/credentials/OS-EC2"},
		{"POST", "/v3/users/" + bad + "/credentials/OS-EC2"},
		{"GET", "/v3/roles/" + bad},
		{"PATCH", "/v3/roles/" + bad},
		{"DELETE", "/v3/roles/" + bad},
		{"GET", "/v3/groups/" + bad},
		{"PATCH", "/v3/groups/" + bad},
		{"DELETE", "/v3/groups/" + bad},
		{"GET", "/v3/domains/" + bad},
		{"PATCH", "/v3/domains/" + bad},
		{"DELETE", "/v3/domains/" + bad},
		{"GET", "/v3/domains/" + bad + "/config"},
		{"GET", "/v3/services/" + bad},
		{"DELETE", "/v3/services/" + bad},
		{"DELETE", "/v3/endpoints/" + bad},
		{"GET", "/v3/credentials/" + bad},
		{"DELETE", "/v3/credentials/" + bad},
		{"GET", "/v3/application_credentials/" + bad},
		{"GET", "/v3/projects/" + bad + "/users/" + bad + "/roles/" + bad},
		{"DELETE", "/v3/groups/" + bad + "/users/" + bad},
	}
	for _, tc := range cases {
		code, body := doReq(r, tc.method, tc.path, adminHdr)
		if code < 400 || code >= 500 {
			t.Errorf("%s %s: code=%d body=%s; want 4xx, never 5xx", tc.method, tc.path, code, body)
		}
	}
}
