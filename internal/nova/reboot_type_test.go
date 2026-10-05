package nova

import (
	"net/http"
	"testing"
)

// Upstream's schema requires reboot.type, one of HARD, Hard, hard, SOFT, Soft or soft,
// and allows no other property (nova/api/openstack/compute/schemas/servers.py). Anything
// else is a 400 before the instance is looked up, like an unknown action, so a malformed
// reboot cannot revoke the instance's console links on its way to being refused.
func TestRebootRequiresAValidType(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct{ name, body string }{
		{"missing type", `{"reboot":{}}`},
		{"unknown type", `{"reboot":{"type":"FAST"}}`},
		{"a case upstream does not accept", `{"reboot":{"type":"hArD"}}`},
		{"not a string", `{"reboot":{"type":1}}`},
		{"an extra property", `{"reboot":{"type":"SOFT","force":true}}`},
		{"not an object", `{"reboot":"HARD"}`},
		{"null", `{"reboot":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)
			tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket, testDomainID)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400. body: %s", code, body)
			}
			if _, err := store.Resolve(t.Context(), tok); err != nil {
				t.Errorf("a refused reboot revoked the console token: %v", err)
			}
		})
	}
}

// Each accepted type shows upstream's status for it while the reboot runs: HARD_REBOOT
// for a hard one, REBOOT for a soft one.
func TestRebootTypeSetsItsStatus(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct{ body, want string }{
		{`{"reboot":{"type":"HARD"}}`, "HARD_REBOOT"},
		{`{"reboot":{"type":"hard"}}`, "HARD_REBOOT"},
		{`{"reboot":{"type":"Soft"}}`, "REBOOT"},
		{`{"reboot":{"type":"SOFT"}}`, "REBOOT"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r, _ := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstanceNamed(t, seededProjectID, "reboot-status-"+tc.want)

			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", tc.body,
				testAuthHdr(seededProjectID, seededUserID))
			if code != http.StatusAccepted {
				t.Fatalf("status = %d; want 202. body: %s", code, body)
			}

			var status string
			if err := testPool.QueryRow(t.Context(),
				`SELECT status FROM instances WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatalf("reading status: %v", err)
			}
			if status != tc.want {
				t.Errorf("status = %q; want %q", status, tc.want)
			}
		})
	}
}
