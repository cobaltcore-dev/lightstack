package keystone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
)

// SPEC-023 US2: the scope vocabulary. Encodes contracts/identity-scope.md.
//
// Before this feature a domain-scoped authentication request was answered 201
// with a token carrying no project, no domain and no roles — the caller could
// not tell it had been ignored, and every later call failed for reasons that
// pointed nowhere.
//
// O3K issues no domain-scoped token: role_assignments binds a user to a role
// within a PROJECT, so there is no domain-level grant to read. The honest answer
// is "no", stated once, at the door.

// postAuth posts an auth body to the real /v3/auth/tokens route.
//
// Only usable for bodies that must be REFUSED: a refusal returns before the
// password/token fork, so it never reaches the database this service was built
// without. A supported scope falls through to AuthenticatePassword and its live
// pool — assert those with scopeRefusal instead.
func postAuth(t *testing.T, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService(nil, nil).RegisterRoutes(&r.RouterGroup, func(c *gin.Context) { c.Next() })
	req := httptest.NewRequest(http.MethodPost, "/v3/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// scopeRefusal parses an auth body and returns the refusal its scope warrants,
// or nil for a supported one.
//
// It runs the same two steps the route does — ScopeField.UnmarshalJSON, where
// the project/domain distinction is actually made, then unsupportedScope — and
// stops there. That is the whole decision under test, and unlike postAuth it
// says something about the bodies that are meant to SUCCEED.
func scopeRefusal(t *testing.T, body string) *common.OpenStackError {
	t.Helper()
	var req AuthRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("parsing the auth body: %v", err)
	}
	return unsupportedScope(req.Auth.Scope)
}

// credentials is the identity half every case below shares. Note its OWN domain
// qualifier (user.domain) — distinct again from a scope.
const credentials = `"identity":{"methods":["password"],"password":{"user":` +
	`{"name":"admin","password":"secret","domain":{"name":"Default"}}}}`

// TestDomainScopeIsRefused: S1. The defect this story exists for.
func TestDomainScopeIsRefused(t *testing.T) {
	code, body := postAuth(t, `{"auth":{`+credentials+`,"scope":{"domain":{"name":"Default"}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("domain-scoped auth: code=%d, want 400. body: %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "domain") {
		t.Errorf("refusal must name the scope kind it is refusing; body: %s", body)
	}
	// Never a success carrying neither the scope asked for nor any roles (FR-010).
	if code == http.StatusCreated {
		t.Error("answered 201 — the pre-SPEC-023 defect")
	}
}

// TestDomainScopeRefusalIsUniform: S2 / FR-014. Every domain reference produces
// the same answer, so none of them reveals which domains exist. Uniformity falls
// out of refusing BEFORE resolving the reference — if the implementation ever
// starts looking the domain up, this test fails.
func TestDomainScopeRefusalIsUniform(t *testing.T) {
	refs := []struct{ name, scope string }{
		{"exists", `{"name":"Default"}`},
		{"does not exist", `{"name":"no-such-domain"}`},
		{"well-known alias", `{"id":"default"}`},
		{"unknown uuid", `{"id":"00000000-0000-0000-0000-000000000009"}`},
		{"malformed identifier", `{"id":"not-a-uuid"}`},
		{"empty object", `{}`},
	}
	var wantCode int
	var wantBody string
	for i, tc := range refs {
		code, body := postAuth(t, `{"auth":{`+credentials+`,"scope":{"domain":`+tc.scope+`}}}`)
		if i == 0 {
			wantCode, wantBody = code, body
			continue
		}
		if code != wantCode || body != wantBody {
			t.Errorf("%s: (%d, %s) differs from the reference answer (%d, %s) — a differing "+
				"answer discloses whether the domain exists", tc.name, code, body, wantCode, wantBody)
		}
	}
	// A malformed identifier must never reach a uuid column and surface as a 500.
	if wantCode >= 500 {
		t.Errorf("refusal is a server error (%d); want a client error", wantCode)
	}
}

// TestNestedDomainQualifierStillWorks: S4 — THE REGRESSION GUARD.
//
// `scope.project.domain` qualifies WHICH project is meant. It is what Horizon,
// Terraform and python-openstackclient send on every authentication. If the new
// check catches it, every client in existence stops being able to log in.
func TestNestedDomainQualifierStillWorks(t *testing.T) {
	for _, tc := range []struct{ name, scope string }{
		{"project by name with domain qualifier",
			`{"project":{"name":"default","domain":{"name":"Default"}}}`},
		{"project by id with domain qualifier",
			`{"project":{"id":"00000000-0000-0000-0000-000000000002","domain":{"name":"Default"}}}`},
		{"project by name, no qualifier", `{"project":{"name":"default"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if osErr := scopeRefusal(t, `{"auth":{`+credentials+`,"scope":`+tc.scope+`}}`); osErr != nil {
				t.Fatalf("a PROJECT scope carrying a domain qualifier was refused as a domain "+
					"scope — this breaks every existing client. refusal: %s", osErr.Message)
			}
		})
	}
}

// TestUnscopedAndAbsentScopeUnaffected: S5. The other two supported forms.
func TestUnscopedAndAbsentScopeUnaffected(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"explicit unscoped", `{"auth":{` + credentials + `,"scope":"unscoped"}}`},
		{"no scope key", `{"auth":{` + credentials + `}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if osErr := scopeRefusal(t, tc.body); osErr != nil {
				t.Errorf("%s was refused as a domain scope. refusal: %s", tc.name, osErr.Message)
			}
		})
	}
}

// TestContradictoryScopeIsRefused: S7 / FR-012. Naming both kinds is refused as
// contradictory rather than resolved by precedence — silently picking one is how
// a caller ends up with authority it did not ask for.
func TestContradictoryScopeIsRefused(t *testing.T) {
	code, body := postAuth(t, `{"auth":{`+credentials+
		`,"scope":{"project":{"name":"default"},"domain":{"name":"Default"}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("project+domain scope: code=%d, want 400. body: %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "not both") {
		t.Errorf("refusal should say the two are mutually exclusive; body: %s", body)
	}
}

// TestDomainScopeRefusedOnTokenPath: S6 / FR-016. The token re-scoping path must
// answer identically to the password path. They carry duplicated scope logic and
// have drifted once already, which is why the check sits before the fork.
func TestDomainScopeRefusedOnTokenPath(t *testing.T) {
	code, body := postAuth(t,
		`{"auth":{"identity":{"methods":["token"],"token":{"id":"whatever"}},`+
			`"scope":{"domain":{"name":"Default"}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("token-path domain scope: code=%d, want 400 (same as the password path). body: %s",
			code, body)
	}
}
