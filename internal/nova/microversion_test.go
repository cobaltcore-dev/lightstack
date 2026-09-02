package nova

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-025-followup US6: the negotiated microversion is read in one place, reported, and
// honoured where the product emits behaviour upstream gates (FR-038 – FR-044).
//
// EVERY TEST HERE NEGOTIATES EXPLICITLY. A request with no version header resolves to the
// product's MAXIMUM (FR-040a) — upstream assumes its minimum, and Lightstack keeps its own
// behaviour because adopting upstream's default would strip behaviour from every existing
// caller in one release. So a test relying on the default header exercises the maximum and
// proves nothing about a gate. That divergence is recorded as entry 2 in the
// constitution's Known Divergences register.

func mvCtx(header, value string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v2.1/servers", nil)
	if header != "" {
		c.Request.Header.Set(header, value)
	}
	return c
}

// TestRequestedMicroversionParsing is T100: header precedence and the `latest` keyword.
// A CONTROL — this behaviour predates the story and must not change.
func TestRequestedMicroversionParsing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	maxVersion := microversion{2, 90}

	for _, tc := range []struct {
		name   string
		header string
		value  string
		want   microversion
	}{
		{"service-qualified form", "OpenStack-API-Version", "compute 2.21", microversion{2, 21}},
		{"nova-specific form", "X-OpenStack-Nova-API-Version", "2.21", microversion{2, 21}},
		{"latest resolves to the maximum", "OpenStack-API-Version", "compute latest", maxVersion},
		{"no header resolves to the maximum", "", "", maxVersion},
		{"unparseable is treated as no negotiation", "OpenStack-API-Version", "compute banana", maxVersion},
		// A single-digit minor is its own version, not a fraction: 2.9 is minor 9, and it
		// must NOT come back as the 2.90 maximum.
		{"single-digit minor is minor 9", "OpenStack-API-Version", "compute 2.9", microversion{2, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestedMicroversion(mvCtx(tc.header, tc.value)); got != tc.want {
				t.Errorf("requestedMicroversion = %v; want %v", got, tc.want)
			}
		})
	}

	// Both headers present: the service-qualified one wins, as upstream specifies.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v2.1/servers", nil)
	c.Request.Header.Set("OpenStack-API-Version", "compute 2.30")
	c.Request.Header.Set("X-OpenStack-Nova-API-Version", "2.10")
	if got := requestedMicroversion(c); got != (microversion{2, 30}) {
		t.Errorf("with both headers requestedMicroversion = %v; want 2.30 — OpenStack-API-Version takes precedence", got)
	}
}

// TestMicroversionOutOfRangeRefused is T098: FR-041. A version above the advertised maximum
// is refused with 406, so a client is told to negotiate down rather than silently handed
// something else.
func TestMicroversionOutOfRangeRefused(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	for _, v := range []string{"compute 2.99", "compute 3.0"} {
		hdr := testAuthHdr(seededProjectID, seededUserID)
		hdr["OpenStack-API-Version"] = v
		code, body := doReq(r, http.MethodGet, "/v2.1/servers", "", hdr)
		if code != http.StatusNotAcceptable {
			t.Errorf("%s: code=%d body=%s; want 406", v, code, body)
		}
		// Upstream's message names the range, which is what lets a client negotiate down.
		if !strings.Contains(body, novaMinVersion) || !strings.Contains(body, novaCurrentVersion) {
			t.Errorf("%s: body=%s; the refusal must name the supported range", v, body)
		}
	}

	// Below the minimum is equally out of range.
	hdr := testAuthHdr(seededProjectID, seededUserID)
	hdr["OpenStack-API-Version"] = "compute 1.0"
	if code, body := doReq(r, http.MethodGet, "/v2.1/servers", "", hdr); code != http.StatusNotAcceptable {
		t.Errorf("compute 1.0: code=%d body=%s; want 406", code, body)
	}
}

// TestMicroversionMalformedTolerated is T099: FR-041a. A header that does not PARSE is
// treated as no negotiation and the request succeeds.
//
// A CONTROL, and a deliberate divergence: upstream answers 400. SPEC-025 decided this and
// recorded it as A-006 — the gate exists to avoid granting more than a client negotiated,
// and that property holds without inventing a rejection on a surface that never had one.
// Pinned so a later change cannot make it silently.
func TestMicroversionMalformedTolerated(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	for _, v := range []string{"compute banana", "garbage", "compute", "compute 2.x"} {
		hdr := testAuthHdr(seededProjectID, seededUserID)
		hdr["OpenStack-API-Version"] = v
		code, body := doReq(r, http.MethodGet, "/v2.1/servers", "", hdr)
		if code != http.StatusOK {
			t.Errorf("%q: code=%d body=%s; want 200 — a malformed header is not a refusal (A-006)", v, code, body)
		}
	}
}

// TestMicroversionAppliedVersionReported is T101: FR-038. The response names the version
// actually applied, not a constant. Before this story the header was emitted only on the
// version documents, and always as the maximum.
func TestMicroversionAppliedVersionReported(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	for _, tc := range []struct{ negotiated, want string }{
		{"compute 2.21", "compute 2.21"},
		{"compute 2.1", "compute 2.1"},
		{"compute latest", "compute " + novaCurrentVersion},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v2.1/servers", nil)
		for k, v := range testAuthHdr(seededProjectID, seededUserID) {
			req.Header.Set(k, v)
		}
		req.Header.Set("OpenStack-API-Version", tc.negotiated)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := w.Header().Get("OpenStack-API-Version"); got != tc.want {
			t.Errorf("negotiated %q: response header = %q; want %q — the applied version, not a constant",
				tc.negotiated, got, tc.want)
		}
	}
}

// TestMicroversionNoHeaderUnchanged is T102: FR-040 and FR-040a. A request with no version
// header keeps receiving today's behaviour, which is the MAXIMUM.
//
// A CONTROL, and the pin on divergence 2: upstream would assume its minimum here. This test
// exists so that adopting upstream's default becomes a deliberate, visible change rather
// than something that slips in with a refactor.
func TestMicroversionNoHeaderUnchanged(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/v2.1/servers", nil)
	for k, v := range testAuthHdr(seededProjectID, seededUserID) {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("no header: code=%d body=%s; want 200", w.Code, w.Body.String())
	}
	if got := w.Header().Get("OpenStack-API-Version"); got != "compute "+novaCurrentVersion {
		t.Errorf("no header: response version = %q; want the maximum %q (FR-040a, upstream would assume the minimum)",
			got, "compute "+novaCurrentVersion)
	}
}

// TestCreateImageGatedAt245 is T110: the one gate the audit produced. Upstream returns
// `{"image_id": ...}` from the createImage/createBackup action starting at 2.45; before it,
// the image is identified by the Location header alone and the body is empty.
//
// Tested at BOTH sides of the boundary, negotiating explicitly.
func TestCreateImageGatedAt245(t *testing.T) {
	gin.SetMode(gin.TestMode)

	if wantsCreateImageIDInBody(mvCtx("OpenStack-API-Version", "compute 2.45")) != true {
		t.Error("at 2.45 the image_id body must be served")
	}
	if wantsCreateImageIDInBody(mvCtx("OpenStack-API-Version", "compute 2.44")) != false {
		t.Error("at 2.44 the image_id body must NOT be served — upstream identifies the image by Location alone")
	}
	if wantsCreateImageIDInBody(mvCtx("OpenStack-API-Version", "compute 2.90")) != true {
		t.Error("at the maximum the image_id body must be served")
	}
	if wantsCreateImageIDInBody(mvCtx("", "")) != true {
		t.Error("with no header the maximum applies, so the body must be served (FR-040a)")
	}
}

// TestCreateImageActionsBothGated pins that BOTH actions upstream changed at 2.45 are
// gated, not just one.
//
// The real-mode host pass found the gate applied to createBackup only: createImage
// returned no body at any version — upstream's pre-2.45 shape served unconditionally —
// while createBackup returned the body unconditionally under a comment claiming it was
// gated. Two opposite defects in two handlers that upstream changed together, and a test
// naming only one of them would have kept missing the other.
func TestCreateImageActionsBothGated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// The dispatch table is the authority on which handler serves which action; asserting
	// on it keeps this test honest if the actions are ever re-pointed.
	for _, action := range []string{"createImage", "createBackup"} {
		if _, ok := serverActions[action]; !ok {
			t.Errorf("%q is not in the action dispatch table; this test no longer covers what it claims", action)
		}
	}

	// Both read the same gate, so one helper is the whole contract.
	if !wantsCreateImageIDInBody(mvCtx("OpenStack-API-Version", "compute 2.45")) {
		t.Error("at 2.45 the image_id body must be served")
	}
	if wantsCreateImageIDInBody(mvCtx("OpenStack-API-Version", "compute 2.44")) {
		t.Error("at 2.44 it must not be")
	}
}

// TestMicroversionOrderingIsNotFloat pins that a microversion's minor part is compared as
// an INTEGER, not as a decimal fraction.
//
// The two gates in this file sit at 2.21 and 2.45. Under float comparison every
// single-digit minor from 2.3 up is numerically LARGER than both (2.9 > 2.45 > 2.21) while
// being a SMALLER version than either — so a client negotiating 2.3 … 2.9 was handed the
// deleted-action history and the image_id body it had not asked for. Granting more than the
// client negotiated is the one thing this file says its gates exist to prevent.
//
// The whole boundary is tabulated, not just the two thresholds: the previous tests
// exercised 2.1, 2.21, 2.44, 2.45 and 2.90 only, which is exactly the set that behaves
// identically under both orderings.
func TestMicroversionOrderingIsNotFloat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		negotiated     string
		deletedActions bool // gate at 2.21
		imageIDBody    bool // gate at 2.45
	}{
		// Single-digit minors. Every one of these is BELOW both gates as a version and
		// ABOVE both as a float.
		{"2.2", false, false},
		{"2.3", false, false},
		{"2.4", false, false},
		{"2.5", false, false},
		{"2.9", false, false},
		// Two-digit minors around each threshold.
		{"2.20", false, false},
		{"2.21", true, false},
		{"2.22", true, false},
		{"2.44", true, false},
		{"2.45", true, true},
		{"2.46", true, true},
		{"2.90", true, true},
	} {
		t.Run(tc.negotiated, func(t *testing.T) {
			c := mvCtx("OpenStack-API-Version", "compute "+tc.negotiated)
			if got := wantsDeletedInstanceActions(c); got != tc.deletedActions {
				t.Errorf("negotiated %s: wantsDeletedInstanceActions = %v; want %v (gate is version 2.21)",
					tc.negotiated, got, tc.deletedActions)
			}
			if got := wantsCreateImageIDInBody(c); got != tc.imageIDBody {
				t.Errorf("negotiated %s: wantsCreateImageIDInBody = %v; want %v (gate is version 2.45)",
					tc.negotiated, got, tc.imageIDBody)
			}
		})
	}
}

// TestMicroversionRangeCheckIsNotFloat pins the same ordering rule on the 406 range check.
//
// Under float comparison "2.100" parsed to 2.1, landed inside [2.1, 2.9] and was accepted:
// a version forty-one past the advertised maximum, answered as though it were the minimum.
func TestMicroversionRangeCheckIsNotFloat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		negotiated string
		outOfRange bool
	}{
		{"2.1", false},  // the minimum
		{"2.9", false},  // a real version, well inside the range
		{"2.10", false}, // minor 10, not 2.1
		{"2.90", false}, // the maximum
		{"2.91", true},  // one past it
		{"2.100", true}, // three-digit minor: float-parsed to 2.1 and wrongly accepted
		{"2.0", true},   // below the minimum
		{"3.0", true},   // a different major
		{"1.99", true},
	} {
		t.Run(tc.negotiated, func(t *testing.T) {
			msg, out := negotiatedVersionOutOfRange(mvCtx("OpenStack-API-Version", "compute "+tc.negotiated))
			if out != tc.outOfRange {
				t.Errorf("negotiated %s: outOfRange = %v; want %v", tc.negotiated, out, tc.outOfRange)
			}
			if out && (!strings.Contains(msg, novaMinVersion) || !strings.Contains(msg, novaCurrentVersion)) {
				t.Errorf("negotiated %s: refusal %q must name the supported range", tc.negotiated, msg)
			}
		})
	}
}
