package nova

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestNovaQuotaWriteTargetMatchesRead: PUT resolves its target the way GET does. The
// reads treat `default` as the caller's own project, so the write must too — otherwise
// an admin scoped to any project but the one NAMED `default` rewrites that project's
// quota, while the PUT's own 200 (rendered by GetQuotaSet) shows the caller's unchanged
// one. The seeded admin cannot catch this: it sits in the project named `default`,
// where both meanings coincide. Raised on PR #79.
func TestNovaQuotaWriteTargetMatchesRead(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	var caller string
	if err := testPool.QueryRow(ctx,
		"INSERT INTO projects (name, description) VALUES ($1, 'spec020 test fixture') RETURNING id::text",
		"spec020-"+t.Name(),
	).Scan(&caller); err != nil {
		t.Fatalf("create caller project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, //nolint:errcheck // best-effort fixture cleanup; cascades to quotas
			"DELETE FROM projects WHERE id = $1", caller)
	})
	namedDefaultLimit := func() int {
		t.Helper()
		var limit int
		if err := testPool.QueryRow(ctx,
			`SELECT COALESCE((SELECT hard_limit FROM quotas WHERE project_id = $1 AND resource = 'instances'), -1)`,
			seededProjectID).Scan(&limit); err != nil {
			t.Fatalf("read the default project's instances quota: %v", err)
		}
		return limit
	}
	before := namedDefaultLimit()
	t.Cleanup(func() {
		testPool.Exec(ctx, //nolint:errcheck // best-effort restore should the write land there
			`UPDATE quotas SET hard_limit = $2 WHERE project_id = $1 AND resource = 'instances'`,
			seededProjectID, before)
	})

	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodPut, "/v2.1/os-quota-sets/default",
		`{"quota_set":{"instances":7}}`, testAuthHdr(caller, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("PUT os-quota-sets/default: code=%d body=%s; want 200", code, body)
	}
	if qs := quotaSetBody(t, body); qs["id"] != caller || qs["instances"] != float64(7) {
		t.Errorf("PUT's 200 reports id=%v instances=%v; want the caller's project %s with the 7 just written",
			qs["id"], qs["instances"], caller)
	}
	if after := namedDefaultLimit(); after != before {
		t.Errorf("the project named `default` had its instances quota changed %d → %d by a caller scoped elsewhere",
			before, after)
	}
}

// TestNovaLimitsRejectUnscopedToken: an unscoped token carries no project. GetLimits
// handed its empty id to the uuid-typed quota query — a 500 — while the quota routes
// answered the same token 404 through QuotaTargetProject. Both now answer alike.
// Raised on PR #79.
func TestNovaLimitsRejectUnscopedToken(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	unscoped := map[string]string{"X-Test-User": seededUserID} // no project, no roles

	for _, path := range []string{"/v2.1/limits", "/v2.1/os-quota-sets/" + seededProjectID} {
		if code, body := doReq(r, http.MethodGet, path, "", unscoped); code != http.StatusNotFound {
			t.Errorf("GET %s with an unscoped token: code=%d body=%s; want 404", path, code, body)
		}
	}
}

// TestNovaQuotaTargetNonAdminForeignIsForbidden: a non-admin naming another project gets
// upstream's 403 on show and detail (os-quota-sets:show/detail are
// PROJECT_READER_OR_ADMIN) — not its own quotas, and not a 404. The refusal comes
// before any lookup, so a phantom id cannot be used to probe which projects exist
// (FR-002a).
func TestNovaQuotaTargetNonAdminForeignIsForbidden(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	nonAdmin := map[string]string{
		"X-Test-Project": seededProjectID,
		"X-Test-User":    seededUserID,
	}

	for _, path := range []string{
		"/v2.1/os-quota-sets/11111111-1111-1111-1111-111111111111",
		"/v2.1/os-quota-sets/11111111-1111-1111-1111-111111111111/detail",
	} {
		if code, body := doReq(r, http.MethodGet, path, "", nonAdmin); code != http.StatusForbidden {
			t.Errorf("non-admin GET %s: code=%d body=%s; want 403", path, code, body)
		}
	}
}

// TestNovaQuotaDefaultsReadableForAnyProject: upstream's os-quota-sets:defaults is
// RULE_ANY, so a non-admin may read the defaults for any project id. The 403 above must
// not reach this endpoint.
func TestNovaQuotaDefaultsReadableForAnyProject(t *testing.T) {
	r := newNovaTestRouter()
	nonAdmin := map[string]string{
		"X-Test-Project": seededProjectID,
		"X-Test-User":    seededUserID,
	}

	code, body := doReq(r, http.MethodGet,
		"/v2.1/os-quota-sets/11111111-1111-1111-1111-111111111111/defaults", "", nonAdmin)
	if code != http.StatusOK {
		t.Fatalf("non-admin GET foreign quota defaults: code=%d body=%s; want 200 (RULE_ANY)", code, body)
	}
}

// TestNovaQuotaTargetLookupFailureIs500: a failed project lookup is an internal error, not "no
// such project". ResolveProjectRef answered not-found for any database error, so during an
// outage the quota routes told clients the project did not exist. Simulated with a request whose
// context is already cancelled, which fails the lookup query. Raised on PR #79.
func TestNovaQuotaTargetLookupFailureIs500(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v2.1/os-quota-sets/"+seededProjectID, nil).WithContext(ctx)
	for k, v := range testAuthHdr(seededProjectID, seededUserID) {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("GET os-quota-sets/{id} with a failing lookup: code=%d body=%s; want 500, not 404",
			w.Code, w.Body.String())
	}
}
