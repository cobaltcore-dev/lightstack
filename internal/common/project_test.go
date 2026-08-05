package common

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestQuotaTargetProject_ScopesWithoutTouchingTheDatabase covers every branch that
// resolves from the request alone — the bare form, the `default` alias, and any
// non-admin caller. These paths must NOT reach the database: a non-admin's foreign id
// is ignored rather than looked up, which is what keeps the endpoint from being used
// to probe which projects exist (SPEC-020 FR-002a).
//
// The remaining branch — an admin naming another project — is a DB lookup through
// ResolveProjectRef and is covered end-to-end per service by
// {Nova,Neutron}QuotaTargetMustExist / QuotaTargetResolvesRealProject and cinder's
// TestQuotaTargetMustExist. Reaching the DB here would make this a DB-gated test for
// no extra coverage, so it deliberately asserts only the no-DB branches. Any case that
// did reach the database would panic on the nil pool rather than pass quietly.
func TestQuotaTargetProject_ScopesWithoutTouchingTheDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const caller = "00000000-0000-0000-0000-000000000002"
	const foreign = "11111111-1111-1111-1111-111111111111"

	cases := []struct {
		name    string
		param   string
		roles   []string
		wantID  string
		wantOK  bool
		wantWhy string
	}{
		{
			name: "no id in the path falls back to the caller", param: "", roles: []string{"admin"},
			wantID: caller, wantOK: true,
			wantWhy: "the bare quota-set form means the caller's own project",
		},
		{
			name: "the `default` alias means the caller, even for an admin", param: "default", roles: []string{"admin"},
			wantID: caller, wantOK: true,
			wantWhy: "`default` is the quota API's alias for the caller's project, not a project named 'default'",
		},
		{
			name: "non-admin naming a foreign project is scoped to its own", param: foreign, roles: []string{"member"},
			wantID: caller, wantOK: true,
			wantWhy: "a foreign id must be ignored, not resolved — otherwise 404-vs-200 leaks which projects exist",
		},
		{
			name: "caller with no roles at all is scoped to its own", param: foreign, roles: nil,
			wantID: caller, wantOK: true,
			wantWhy: "absent roles must not be treated as admin",
		},
		{
			name: "`admin` must match exactly, not by prefix", param: foreign, roles: []string{"administrator"},
			wantID: caller, wantOK: true,
			wantWhy: "a role merely starting with 'admin' does not grant the override",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/", nil)
			c.Set("project_id", caller)
			if tc.roles != nil {
				c.Set("roles", tc.roles)
			}
			if tc.param != "" {
				c.Params = gin.Params{{Key: "id", Value: tc.param}}
			}

			gotID, gotOK := QuotaTargetProject(c)
			if gotID != tc.wantID || gotOK != tc.wantOK {
				t.Errorf("QuotaTargetProject() = (%q, %v), want (%q, %v) — %s",
					gotID, gotOK, tc.wantID, tc.wantOK, tc.wantWhy)
			}
		})
	}
}

// TestQuotaTargetProject_RejectsUnscopedToken: Keystone issues unscoped tokens (no
// project, no roles), and the middleware copies that empty claim into the context. The
// fall-back branch used to return it with ok=true, so "" reached a uuid-typed column and
// the caller got a 500 — "invalid input syntax for type uuid" — instead of a 4xx. No DB
// required: every case here resolves from the request alone.
func TestQuotaTargetProject_RejectsUnscopedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name  string
		param string
		roles []string
	}{
		{name: "bare form", param: ""},
		{name: "`default` alias", param: "default"},
		{name: "non-admin naming a project", param: "11111111-1111-1111-1111-111111111111"},
		{name: "admin with the `default` alias", param: "default", roles: []string{"admin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/", nil)
			c.Set("project_id", "") // unscoped
			if tc.roles != nil {
				c.Set("roles", tc.roles)
			}
			if tc.param != "" {
				c.Params = gin.Params{{Key: "id", Value: tc.param}}
			}

			if gotID, gotOK := QuotaTargetProject(c); gotOK {
				t.Errorf("QuotaTargetProject() = (%q, true); want ok=false for an unscoped token "+
					"(an empty project id reaches a uuid column and 500s)", gotID)
			}
		})
	}
}
