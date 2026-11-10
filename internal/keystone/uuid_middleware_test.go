package keystone

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestUUIDParamMiddleware drives the middleware through a real gin router (so
// c.FullPath() is populated) with dummy handlers — no DB required.
func TestUUIDParamMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v3")
	g.Use(uuidParamMiddleware)
	ok := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	g.GET("/projects/:id", ok)
	g.GET("/users/:id/application_credentials/:cred_id", ok)
	g.PUT("/projects/:id/users/:user_id/roles/:role_id", ok)
	g.GET("/users/:id/credentials/OS-EC2/:access", ok)

	const valid = "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name, method, path string
		wantCode           int
		wantMsg            string // substring expected in the body on a 404
	}{
		{"valid project id passes", "GET", "/v3/projects/" + valid, 200, ""},
		{"malformed project id", "GET", "/v3/projects/not-a-uuid", 404, "Could not find project: not-a-uuid."},
		{"malformed cred_id -> Application Credential", "GET", "/v3/users/" + valid + "/application_credentials/bad", 404, "Could not find Application Credential: bad."},
		{"malformed user id reported first", "GET", "/v3/users/bad/application_credentials/" + valid, 404, "Could not find user: bad."},
		{"nested: first malformed segment (user)", "PUT", "/v3/projects/" + valid + "/users/bad/roles/" + valid, 404, "Could not find user: bad."},
		{"nested: project malformed first", "PUT", "/v3/projects/bad/users/bad/roles/" + valid, 404, "Could not find project: bad."},
		{"access key is not validated", "GET", "/v3/users/" + valid + "/credentials/OS-EC2/not-a-uuid-key", 200, ""},
		{"malformed id, access ignored", "GET", "/v3/users/bad/credentials/OS-EC2/anykey", 404, "Could not find user: bad."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body: %s)", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantMsg != "" && !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Fatalf("body %q missing %q", w.Body.String(), tc.wantMsg)
			}
		})
	}
}

// TestResourceNounCoversEveryRegisteredRoute walks the *real* keystone route table
// and enforces two things for every UUID path param:
//
//   - the invariant resourceNoun relies on — the param is preceded by a collection
//     segment in its own route template (gin populates c.Params only from the
//     matched template, which is what keeps resourceNoun's fail-loud branch
//     unreachable at runtime); and
//   - that collection has an explicit entry in keystoneNouns, so the 404 message
//     matches keystone/exception.py rather than a guessed singular.
//
// Adding a route whose collection is missing (or whose noun is wrong) fails here
// instead of silently shipping a message that diverges from upstream.
func TestResourceNounCoversEveryRegisteredRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := newIdentityTestRouter()

	checked := 0
	for _, ri := range r.Routes() {
		segs := strings.Split(strings.Trim(ri.Path, "/"), "/")
		for i, seg := range segs {
			// Both gin param forms, not just ":name". A wildcard "*name" also lands in
			// c.Params (keyed without the "*"), so the middleware validates it like any
			// other id — but resourceNoun looks for ":name" and would panic. Matching
			// ":" alone would let such a route pass this gate and panic in production,
			// which is exactly what the gate exists to prevent (PR #74 review, M2).
			param, ok := strings.CutPrefix(seg, ":")
			if !ok {
				param, ok = strings.CutPrefix(seg, "*")
			}
			if !ok || !isUUIDParam(param) {
				continue
			}
			if i == 0 {
				t.Errorf("%s %s: uuid param %q has no preceding collection segment", ri.Method, ri.Path, param)
				continue
			}
			collection := segs[i-1]
			want, known := keystoneNouns[collection]
			if !known {
				t.Errorf("%s %s: collection %q has no keystoneNouns entry — add the exact noun from keystone/exception.py (e.g. \"policies\": \"policy\") so the 404 message matches upstream",
					ri.Method, ri.Path, collection)
				continue
			}
			if got := resourceNoun(ri.Path, param); got != want {
				t.Errorf("%s %s: resourceNoun(%q) = %q, want %q", ri.Method, ri.Path, param, got, want)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no uuid path params found — route enumeration is broken")
	}
	t.Logf("%d uuid path params verified against keystoneNouns", checked)
}

// TestKeystoneNounsMatchUpstream pins each entry to the noun in the corresponding
// keystone/exception.py *NotFound class, and documents why the map exists: the
// naive "trim the s" rule silently mangles irregular plurals.
func TestKeystoneNounsMatchUpstream(t *testing.T) {
	// Verbatim from keystone/exception.py message_format strings.
	upstream := map[string]string{
		"projects":                "project",                // ProjectNotFound
		"users":                   "user",                   // UserNotFound
		"roles":                   "role",                   // RoleNotFound
		"groups":                  "group",                  // GroupNotFound
		"domains":                 "domain",                 // DomainNotFound
		"services":                "service",                // ServiceNotFound
		"endpoints":               "endpoint",               // EndpointNotFound
		"credentials":             "credential",             // CredentialNotFound
		"application_credentials": "Application Credential", // ApplicationCredentialNotFound
	}
	for collection, want := range upstream {
		if got := singularize(collection); got != want {
			t.Errorf("singularize(%q) = %q, want %q (keystone/exception.py)", collection, got, want)
		}
	}
	for collection := range keystoneNouns {
		if _, ok := upstream[collection]; !ok {
			t.Errorf("keystoneNouns has %q with no upstream reference — verify it against keystone/exception.py", collection)
		}
	}
	// Irregular plurals are why keystoneNouns is an explicit map rather than a
	// "trim the trailing s" rule: Keystone calls this resource "policy"
	// (PolicyNotFound), while the fallback yields "policie".
	//
	// This guard asserts the fallback CANNOT produce "policy" — i.e. that the
	// explicit map is still load-bearing. The comparison is deliberately == and
	// not !=: it must stay SILENT today, and fire only if someone later teaches
	// singularize about "-ies", making this guard's premise stale.
	if got := singularize("policies"); got == "policy" {
		t.Errorf("singularize(%q) now returns %q — the -ies fallback works, so this guard's "+
			"premise is stale: add an explicit \"policies\": \"policy\" entry (keystoneNouns is "+
			"explicit by policy) and delete this check", "policies", got)
	}
}

// TestUUIDParamMiddlewareDerivesNounForNewRoutes shows a newly added route — even
// with an oddly spelled *_id param the middleware has never seen — gets a derived
// noun and a normal 404, not a panic.
func TestUUIDParamMiddlewareDerivesNounForNewRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New() // deliberately no recovery middleware: a panic would fail the test
	g := r.Group("/v3")
	g.Use(uuidParamMiddleware)
	g.GET("/widgets/:widgt_id", func(c *gin.Context) { c.String(http.StatusOK, "handler ran") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v3/widgets/not-a-uuid", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Could not find widget: not-a-uuid.") {
		t.Errorf("malformed id: code=%d body=%s; want 404 verbatim", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v3/widgets/00000000-0000-0000-0000-000000000001", nil))
	if w2.Code != http.StatusOK {
		t.Errorf("valid uuid: code=%d, want 200 (handler reached)", w2.Code)
	}
}

// TestResourceNounPanicsOnlyWhenParamAbsentFromRoute pins the fail-loud branch to
// input gin's router cannot produce: a param that is not in the route template.
func TestResourceNounPanicsOnlyWhenParamAbsentFromRoute(t *testing.T) {
	if got := resourceNoun("/v3/widgets/:widgt_id", "widgt_id"); got != "widget" {
		t.Fatalf("resourceNoun = %q, want %q", got, "widget")
	}
	defer func() {
		if recover() == nil {
			t.Error("expected a panic when the param is absent from the route template")
		}
	}()
	resourceNoun("/v3/widgets/:widgt_id", "id")
}

// TestUUIDParamMiddlewareAbortsBeforeHandler pins the one guarantee the status code
// cannot prove: on a malformed id the chain must STOP, so the handler never runs.
// Without the abort gin still invokes the handler, which appends to the already
// written body while the status stays 404 — so a code + substring assertion passes
// either way. Deleting the abort must fail a test, not slip through (PR #74, H2).
func TestUUIDParamMiddlewareAbortsBeforeHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v3")
	g.Use(uuidParamMiddleware)

	handlerRan := false
	g.GET("/projects/:id", func(c *gin.Context) {
		handlerRan = true
		c.String(http.StatusOK, "HANDLER-RAN")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v3/projects/not-a-uuid", nil))

	if handlerRan {
		t.Error("handler ran after a malformed-id 404 — the middleware did not abort the chain")
	}
	if body := w.Body.String(); strings.Contains(body, "HANDLER-RAN") {
		t.Errorf("body %q contains handler output — the response was written twice", body)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// A well-formed but non-canonical id (dash-free, as rgw_compat_tenant_ids advertises, or
// upper-case) is rewritten to canonical form, so the handler's id comparison matches on
// SQLite, where ids are text.
func TestUUIDParamMiddlewareCanonicalisesID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v3")
	g.Use(uuidParamMiddleware)

	var seen string
	g.GET("/projects/:id", func(c *gin.Context) {
		seen = c.Param("id")
		c.Status(http.StatusOK)
	})

	const canonical = "11111111-1111-1111-1111-111111111111"
	for _, spelling := range []string{"11111111111111111111111111111111", "11111111-1111-1111-1111-111111111111"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v3/projects/"+spelling, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code = %d; want 200", spelling, w.Code)
		}
		if seen != canonical {
			t.Errorf("%s: handler saw %q; want the canonical %q", spelling, seen, canonical)
		}
	}
}

func TestCanonicalizeUUIDParamsLeavesMalformedIDsForHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Params = gin.Params{
		{Key: "id", Value: "11111111111111111111111111111111"},
		{Key: "cred_id", Value: "NOT-A-UUID"},
		{Key: "access", Value: "legacy-access-key"},
	}

	canonicalizeUUIDParams(c)
	want := []string{"11111111-1111-1111-1111-111111111111", "NOT-A-UUID", "legacy-access-key"}
	for i, expected := range want {
		if got := c.Params[i].Value; got != expected {
			t.Errorf("param %s = %q, want %q", c.Params[i].Key, got, expected)
		}
	}
}
