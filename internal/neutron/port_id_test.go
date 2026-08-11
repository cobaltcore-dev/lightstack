package neutron

import (
	"net/http"
	"testing"
)

// Second-review finding AGY-1 — handlers sliced the first 8 characters off an identifier before
// establishing that there were 8 to take.
//
// DeleteNetwork was guarded during the 2026-08-13 follow-up; its siblings were not swept, so
// DeletePort kept the identical defect: `tapName := "tap-" + portID[:8]` is the third statement of
// the handler, ahead of every lookup, so DELETE /v2.0/ports/p1 panicked with
// "slice bounds out of range [:8] with length 2". RecoveryMiddleware turned that into a 500 for a
// request that should simply have matched no row.
//
// The routers here deliberately wire no recovery middleware, so a regression does not merely change
// a status code — the panic escapes and fails the test outright.

func TestDeletePortWithShortIDIsNotFound(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	// "1234567" is the longest id that used to panic and "12345678" the shortest that never did, so
	// a guard written with the wrong comparison misplaces exactly one line rather than passing by luck.
	for _, id := range []string{"p1", "foo", "1234567", "12345678", absentID} {
		t.Run(id, func(t *testing.T) {
			code, body := doReq(r, http.MethodDelete, "/v2.0/ports/"+id, nil)
			if code != http.StatusNotFound {
				t.Fatalf("DELETE /v2.0/ports/%s = %d, want 404 (body %q)", id, code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "PortNotFound")
		})
	}
}

// CreatePort reached the same slice by a different route, and the slice was the symptom rather than
// the cause: the network lookup checked pgx.ErrNoRows and nothing else, so a network_id that is not
// a UUID produced "invalid input syntax for type uuid" — not ErrNoRows — which fell through
// unhandled into `bridgeName := "br-" + req.Port.NetworkID[:8]`.
//
// AllocatePortForInstance, which performs the very same lookup for the instance-boot path, checks
// both errors (ports.go:1238-1242). The two paths disagreeing is what #546 already observed about
// them: they do not fail together.
func TestCreatePortWithUnusableNetworkIDIsRefused(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	for _, tc := range []struct {
		name      string
		networkID string
	}{
		{"short non-uuid", "abc"},
		{"long non-uuid", "not-a-valid-uuid-at-all"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"port":{"network_id":"` + tc.networkID + `"}}`
			code, resp := doReqBody(r, http.MethodPost, "/v2.0/ports", body, nil)

			// 404 (no such network) or 400 (malformed) are both defensible; a 500 or a panic is not.
			if code != http.StatusNotFound && code != http.StatusBadRequest {
				t.Fatalf("POST /v2.0/ports with network_id=%q = %d, want 404 or 400 (body %q)",
					tc.networkID, code, resp)
			}
			neutronError(t, resp) // must still be Neutron-shaped, not an empty or compute body
		})
	}
}
