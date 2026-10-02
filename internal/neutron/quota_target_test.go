package neutron

import (
	"context"
	"net/http"
	"testing"
)

// SPEC-020 SC-007, extended to Neutron: a quota target that does not exist is a 404.
//
// Neutron's quota views are computed — code-default limits plus live usage counts — so
// before common.ResolveProjectRef they echoed the admin's URL id straight back and
// reported a complete, plausible quota set for a project that had never existed. There
// is no write path here (lightstack has no Neutron quota table), so reads are the whole
// surface.
func TestNeutronQuotaTargetMustExist(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := neutronAuthHdr(seededProjectID, true)

	for _, tc := range []struct{ name, ref string }{
		{"unknown but well-formed uuid", "11111111-1111-1111-1111-111111111111"},
		{"nil uuid", "00000000-0000-0000-0000-000000000000"},
		{"unknown project name", "no-such-project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{
				"/v2.0/quotas/" + tc.ref,
				"/v2.0/quotas/" + tc.ref + "/default",
				"/v2.0/quotas/" + tc.ref + "/details",
				"/v2.0/quotas/" + tc.ref + "/details.json",
			} {
				code, body := doReq(r, http.MethodGet, path, admin)
				if code != http.StatusNotFound {
					t.Errorf("GET %s: code=%d body=%s; want 404 for a project that does not exist",
						path, code, body)
				}
			}
		})
	}
}

// TestNeutronQuotaTargetResolvesRealProject guards against over-correcting: the paths
// that worked must keep working, by UUID and by the `default` alias.
func TestNeutronQuotaTargetResolvesRealProject(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := neutronAuthHdr(seededProjectID, true)

	for _, ref := range []string{seededProjectID, "default"} {
		for _, suffix := range []string{"", "/default", "/details"} {
			path := "/v2.0/quotas/" + ref + suffix
			code, body := doReq(r, http.MethodGet, path, admin)
			if code != http.StatusOK {
				t.Errorf("GET %s: code=%d body=%s; want 200", path, code, body)
			}
		}
	}
}

// TestNeutronQuotaTargetNonAdminForeignIsForbidden: a non-admin naming another project
// gets upstream's 403 (show checks get_quota whenever id != the caller's project) rather
// than a 404 — the refusal comes before any lookup, so a phantom id cannot be used to
// probe which projects exist (FR-002a).
func TestNeutronQuotaTargetNonAdminForeignIsForbidden(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	nonAdmin := neutronAuthHdr(seededProjectID, false)

	code, body := doReq(r, http.MethodGet,
		"/v2.0/quotas/11111111-1111-1111-1111-111111111111", nonAdmin)
	if code != http.StatusForbidden {
		t.Fatalf("non-admin GET foreign quota: code=%d body=%s; want 403", code, body)
	}
}

// createTestProject inserts a throwaway project and returns its id, removing it when
// the test finishes. Tests that need "some other project" must use a real one: a
// nonexistent id is now a 404 (SC-007a), so a phantom stand-in would assert the
// opposite of the intended behaviour.
func createTestProject(t *testing.T, name string) string {
	t.Helper()
	var id string
	unique := name + "-" + t.Name()
	if err := testPool.QueryRow(context.Background(),
		"INSERT INTO projects (name, description) VALUES ($1, $2) RETURNING id::text",
		unique, "spec020 test fixture",
	).Scan(&id); err != nil {
		t.Fatalf("create test project %q: %v", unique, err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", id); err != nil {
			t.Logf("cleanup project %s: %v", id, err)
		}
	})
	return id
}
