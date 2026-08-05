package nova

import (
	"net/http"
	"testing"
)

// SPEC-020 SC-007, extended to Nova: a quota target that does not exist is a 404.
//
// Cinder already enforced this via its own resolveProjectRef; Nova and Neutron used
// the admin's URL id verbatim, so this is the regression test for lifting that
// resolution into common.ResolveProjectRef. Two distinct pre-fix failures:
//   - a well-formed but unknown UUID found no quota rows and returned the code
//     defaults with a 200 — defaults for a phantom project
//   - a non-UUID reached the `uuid`-typed quotas.project_id column, which Postgres
//     rejects with "invalid input syntax for type uuid" — a 500
func TestNovaQuotaTargetMustExist(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	admin := testAuthHdr(seededProjectID, seededUserID)

	for _, tc := range []struct{ name, ref string }{
		{"unknown but well-formed uuid", "11111111-1111-1111-1111-111111111111"},
		{"nil uuid", "00000000-0000-0000-0000-000000000000"},
		{"unknown project name", "no-such-project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{
				"/v2.1/os-quota-sets/" + tc.ref,
				"/v2.1/os-quota-sets/" + tc.ref + "/detail",
				"/v2.1/os-quota-sets/" + tc.ref + "/defaults",
			} {
				code, body := doReq(r, http.MethodGet, path, "", admin)
				if code != http.StatusNotFound {
					t.Errorf("GET %s: code=%d body=%s; want 404 for a project that does not exist",
						path, code, body)
				}
			}

			code, body := doReq(r, http.MethodPut, "/v2.1/os-quota-sets/"+tc.ref,
				`{"quota_set":{"instances":5}}`, admin)
			if code != http.StatusNotFound {
				t.Errorf("PUT os-quota-sets/%s: code=%d body=%s; want 404 (never store quota rows "+
					"against a nonexistent project)", tc.ref, code, body)
			}
		})
	}
}

// TestNovaQuotaTargetResolvesRealProject: broadening the check must not break the
// paths that worked. An admin may address a real project by UUID and by name, and the
// bare/`default` forms fall back to the caller's own project.
func TestNovaQuotaTargetResolvesRealProject(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	admin := testAuthHdr(seededProjectID, seededUserID)

	for _, ref := range []string{seededProjectID, "default"} {
		path := "/v2.1/os-quota-sets/" + ref
		code, body := doReq(r, http.MethodGet, path, "", admin)
		if code != http.StatusOK {
			t.Errorf("GET %s: code=%d body=%s; want 200", path, code, body)
		}
	}
}

// TestNovaQuotaTargetNonAdminStaysScoped: a non-admin naming another project is
// silently scoped to its own, not 404'd — the id is ignored, never resolved, so a
// phantom id cannot be used to probe which projects exist (FR-002a).
func TestNovaQuotaTargetNonAdminStaysScoped(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	nonAdmin := map[string]string{
		"X-Test-Project": seededProjectID,
		"X-Test-User":    seededUserID,
	}

	code, body := doReq(r, http.MethodGet,
		"/v2.1/os-quota-sets/11111111-1111-1111-1111-111111111111", "", nonAdmin)
	if code != http.StatusOK {
		t.Fatalf("non-admin GET foreign quota-set: code=%d body=%s; want 200 scoped to own project",
			code, body)
	}
	if got, _ := quotaSetBody(t, body)["id"].(string); got != seededProjectID {
		t.Errorf("non-admin quota-set id = %q; want the caller's own project %q", got, seededProjectID)
	}
}
