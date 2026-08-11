package neutron

import (
	"net/http"
	"testing"
)

// SPEC-024 host-pass follow-up, finding 1 (adjacent) — DeleteNetwork derived its bridge name by
// slicing the first 8 characters of the raw path parameter before checking anything, so an id
// shorter than that sliced out of range.
//
// The panic was recovered by RecoveryMiddleware, which at the time ended in c.AbortWithStatus(500)
// and wrote no body at all — so a request that should simply match no row answered with a bodiless
// 500 that also escaped the Neutron envelope B10 exists to guarantee. (That middleware now renders
// through common.SendError, so a panic at least carries an envelope; the guard below is still what
// keeps this request a 404 rather than any kind of 500.)
//
// newEnvelopeRouter deliberately wires no recovery middleware, so a regression here does not merely
// change a status code: the panic propagates and fails the test outright.
func TestDeleteNetworkWithShortIDIsNotFound(t *testing.T) {
	requireDB(t)
	r := newEnvelopeRouter()

	// The 8-character boundary is the point: "1234567" is the longest id that used to panic and
	// "12345678" the shortest that never did, so a guard written with the wrong comparison fails
	// exactly one of these rather than passing by luck.
	for _, id := range []string{"foo", "ab", "1234567", "12345678", absentID} {
		t.Run(id, func(t *testing.T) {
			code, body := doReq(r, http.MethodDelete, "/v2.0/networks/"+id, nil)
			if code != http.StatusNotFound {
				t.Fatalf("DELETE /v2.0/networks/%s = %d, want 404 (body %q)", id, code, body)
			}
			assertNeutronShape(t, neutronError(t, body), "NetworkNotFound")
		})
	}
}
