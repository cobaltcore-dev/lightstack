package common

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// queryCtx builds a gin context whose request carries the given raw query string.
// The raw form matters: a valueless parameter ("?all_tenants") and an
// empty-valued one ("?all_tenants=") are distinct at the URL level but both mean
// "enabled" upstream, and neither survives a map[string]string fixture.
func queryCtx(rawQuery string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v2.1/servers?"+rawQuery, nil)
	return c
}

// TestQueryFlag: the shared boolean-query parser follows upstream Nova's own
// conventions rather than Go's. Two of these cases are why strconv.ParseBool is
// not enough on its own — it rejects "on"/"yes"/"y"/"no"/"off", and it cannot
// express the valueless form at all. Horizon sends the valueless form; the
// OpenStack CLI sends "True". No DB required.
func TestQueryFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		// Absent means false — the default view must never widen by accident.
		{"absent", "", false},
		{"other params only", "limit=10&marker=abc", false},

		// Present with no value means true (upstream: "when used without a
		// value, it defaults to True"). Both spellings reach the server.
		{"valueless", "all_tenants", true},
		{"empty value", "all_tenants=", true},

		// Upstream's true vocabulary, case-insensitive.
		{"one", "all_tenants=1", true},
		{"t", "all_tenants=t", true},
		{"true", "all_tenants=true", true},
		{"True (python-openstackclient)", "all_tenants=True", true},
		{"TRUE", "all_tenants=TRUE", true},
		{"on", "all_tenants=on", true},
		{"y", "all_tenants=y", true},
		{"yes", "all_tenants=yes", true},
		{"YES", "all_tenants=YES", true},

		// Upstream's false vocabulary.
		{"zero", "all_tenants=0", false},
		{"f", "all_tenants=f", false},
		{"false", "all_tenants=false", false},
		{"False", "all_tenants=False", false},
		{"off", "all_tenants=off", false},
		{"n", "all_tenants=n", false},
		{"no", "all_tenants=no", false},

		// Unrecognised parses as false rather than erroring, matching
		// upstream's non-strict parsing. Fail closed: an unreadable value must
		// never widen a listing.
		{"garbage", "all_tenants=banana", false},
		{"numeric other than 0/1", "all_tenants=2", false},
		{"whitespace", "all_tenants=%20", false},
	}
	for _, tc := range cases {
		if got := QueryFlag(queryCtx(tc.query), "all_tenants"); got != tc.want {
			t.Errorf("%s: QueryFlag(%q) = %v, want %v", tc.name, tc.query, got, tc.want)
		}
	}
}

// TestQueryFlag_OtherNames: the helper is not specific to all_tenants — it is
// placed in common so the two existing ad-hoc bool parses (neutron's "shared",
// cinder's "usage") can adopt it later without this feature touching them.
func TestQueryFlag_OtherNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := queryCtx("shared=yes&usage=0&deleted")
	if !QueryFlag(c, "shared") {
		t.Error(`QueryFlag(c, "shared") with "yes" = false, want true`)
	}
	if QueryFlag(c, "usage") {
		t.Error(`QueryFlag(c, "usage") with "0" = true, want false`)
	}
	if !QueryFlag(c, "deleted") {
		t.Error(`QueryFlag(c, "deleted") valueless = false, want true`)
	}
	if QueryFlag(c, "absent") {
		t.Error(`QueryFlag(c, "absent") = true, want false`)
	}
}

// TestQueryFlag_NoRequest: a context with no request must not panic. Handlers
// always have one, so this is a defensive guarantee rather than a live path —
// the same shape as HasRole's malformed-claim case.
func TestQueryFlag_NoRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if QueryFlag(c, "all_tenants") {
		t.Error("QueryFlag with no request = true, want false")
	}
}

// TestParseQueryBool: SRC-4. The vocabulary is shared with QueryFlag, but the
// two-value form lets a *filter* distinguish "said false" from "not a boolean" —
// a filter that narrowed on unparseable input would return a wrong subset, where
// ignoring the parameter returns an unfiltered one.
func TestParseQueryBool(t *testing.T) {
	for _, tc := range []struct {
		in            string
		want, want_ok bool
	}{
		{"1", true, true}, {"t", true, true}, {"true", true, true},
		{"True", true, true}, {"on", true, true}, {"y", true, true}, {"yes", true, true},
		{"0", false, true}, {"f", false, true}, {"false", false, true},
		{"False", false, true}, {"off", false, true}, {"n", false, true}, {"no", false, true},
		// Unrecognised: ok=false, so a filter ignores it rather than narrowing.
		{"banana", false, false}, {"2", false, false}, {"", false, false},
	} {
		got, ok := ParseQueryBool(tc.in)
		if got != tc.want || ok != tc.want_ok {
			t.Errorf("ParseQueryBool(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.want_ok)
		}
	}
}

// TestQueryFlagUsesTheSharedVocabulary: QueryFlag still fails closed on garbage
// even though ParseQueryBool now reports recognisability — an unreadable value
// must never widen a listing.
func TestQueryFlagFailsClosedOnGarbage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, q := range []string{"all_tenants=banana", "all_tenants=2", "all_tenants=maybe"} {
		if QueryFlag(queryCtx(q), "all_tenants") {
			t.Errorf("QueryFlag(%q) = true, want false — an unreadable value must not widen", q)
		}
	}
}
