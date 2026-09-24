package cinder

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-020 (cloud-storage#533) — B1/N3: the two cinder paths Horizon reaches
// through python-cinderclient that Lightstack did not serve.

func cinderGroups(t *testing.T, body string) []map[string]any {
	t.Helper()
	var env struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse groups: %v; body=%s", err, body)
	}
	if env.Groups == nil {
		t.Fatalf("groups key missing or null; body=%s", body)
	}
	return env.Groups
}

// TestGroupsDetailIsNotShadowedByID is the regression test for B1, the defect that
// made Horizon's Create Volume dialog open nothing.
//
// python-cinderclient's groups.list(detailed=True) requests `groups/detail`. Lightstack
// registered only `groups/:id`, so gin matched "detail" as a group id and GetGroup
// answered 500 "failed to get group". Horizon's CreateForm._populate_group_choices
// turns any group_list error into a redirect, so the modal returned an empty 200
// with X-Horizon-Location and the button appeared inert — no traceback anywhere.
//
// The catalog decides which of the two spellings a client uses (flat
// http://host:8776/v3 vs project-scoped .../v3/{project_id}), so both must work.
func TestGroupsDetailIsNotShadowedByID(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	for _, path := range []string{
		"/v3/groups/detail",
		"/v3/" + seededProjectID + "/groups",
		"/v3/" + seededProjectID + "/groups/detail",
	} {
		t.Run(path, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, path, "", hdr)
			if code != http.StatusOK {
				t.Fatalf("GET %s: code=%d body=%s; want 200", path, code, body)
			}
			// A non-nil array, not the 500 the shadowed route produced.
			cinderGroups(t, body)
		})
	}
}

// TestGroupsIDStillResolves guards the ordering fix from over-correcting: adding the
// static `detail` route must not stop a real group id from matching `:id`. A missing
// group is a 404 — anything else (notably 500) means the route table is wrong again.
func TestGroupsIDStillResolves(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	hdr := cinderAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodGet, "/v3/groups/11111111-1111-1111-1111-111111111111", "", hdr)
	if code != http.StatusNotFound {
		t.Errorf("GET unknown group id: code=%d body=%s; want 404", code, body)
	}
}

// TestOsQuotaSetsAliasMatchesQuotaSets is the regression test for N3. Upstream Cinder
// exposes quotas at os-quota-sets, and that is what python-cinderclient requests —
// Horizon's /api/cinder/quota-sets/defaults/ returned 500 "Resource could not be
// found. (HTTP 404)" because Lightstack only registered the un-prefixed spelling. Both must
// resolve, to byte-identical bodies.
func TestOsQuotaSetsAliasMatchesQuotaSets(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	// A project of its own. The usage view counts the project's volumes, and packages
	// run in parallel against one test database: on the shared seeded project another
	// package's test could create a volume between the two reads below and make two
	// identical handlers look divergent.
	projectID := uuid.NewString()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled) VALUES ($1, $2, '', true)`,
		projectID, "quota-alias-"+projectID); err != nil {
		t.Fatalf("seeding project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", projectID)
	})
	hdr := cinderAuthHdr(projectID, seededUserID)

	// The real Block Storage v3 surface: show, show-with-usage (a query parameter, not
	// Nova's `/detail` sub-resource), and defaults.
	for _, suffix := range []string{"", "?usage=true", "/defaults"} {
		t.Run("suffix"+suffix, func(t *testing.T) {
			plain := "/v3/quota-sets/" + projectID + suffix
			prefixed := "/v3/os-quota-sets/" + projectID + suffix

			plainCode, plainBody := doReq(r, http.MethodGet, plain, "", hdr)
			prefixedCode, prefixedBody := doReq(r, http.MethodGet, prefixed, "", hdr)

			if prefixedCode != http.StatusOK {
				t.Fatalf("GET %s: code=%d body=%s; want 200", prefixed, prefixedCode, prefixedBody)
			}
			if prefixedCode != plainCode || prefixedBody != plainBody {
				t.Errorf("alias diverged:\n  %s -> %d %s\n  %s -> %d %s",
					plain, plainCode, plainBody, prefixed, prefixedCode, prefixedBody)
			}
		})
	}
}

// TestOsQuotaSetsAliasKeepsAuthz: the alias must not become a way around FR-020's
// admin-only quota writes. Omitting X-Test-Roles makes the caller a non-admin.
func TestOsQuotaSetsAliasKeepsAuthz(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()
	nonAdmin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		body := ""
		if method == http.MethodPut {
			body = `{"quota_set":{"volumes":7}}`
		}
		code, resp := doReq(r, method, "/v3/os-quota-sets/"+seededProjectID, body, nonAdmin)
		if code != http.StatusForbidden {
			t.Errorf("%s os-quota-sets as non-admin: code=%d body=%s; want 403", method, code, resp)
		}
	}
}
