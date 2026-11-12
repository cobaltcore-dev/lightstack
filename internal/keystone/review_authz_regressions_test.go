package keystone

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func identityUserHeaders(userID, roles string) map[string]string {
	return map[string]string{"X-Test-User": userID, "X-Test-Roles": roles}
}

func TestGenericCredentialOwnerAndAdminAuthorization(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	ownerID, outsiderID := "00000000-0000-0000-0000-000000000001", uuid.NewString()
	ownerCredentialID, adminCredentialID := uuid.NewString(), uuid.NewString()
	for _, id := range []string{ownerCredentialID, adminCredentialID} {
		if _, err := testPool.Exec(t.Context(), "INSERT INTO credentials (id, user_id, type, blob, created_at, updated_at) VALUES ($1, $2, 'ec2', 'review-secret', now(), now())", id, ownerID); err != nil {
			t.Fatalf("seed credential %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM credentials WHERE id IN ($1, $2)", ownerCredentialID, adminCredentialID)
	})

	owner := identityUserHeaders(ownerID, "member")
	outsider := identityUserHeaders(outsiderID, "member")
	admin := identityUserHeaders(outsiderID, "admin")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v3/credentials/" + ownerCredentialID, ""},
		{http.MethodPatch, "/v3/credentials/" + ownerCredentialID, "{\"credential\":{\"blob\":\"outsider-secret\"}}"},
		{http.MethodDelete, "/v3/credentials/" + ownerCredentialID, ""},
	} {
		if code, body := doJSONReq(r, tc.method, tc.path, tc.body, outsider); code != http.StatusForbidden {
			t.Errorf("outsider %s %s: code=%d body=%s; want 403", tc.method, tc.path, code, body)
		}
	}
	for _, tc := range []struct {
		headers map[string]string
		wantID  string
	}{{owner, ownerCredentialID}, {outsider, ""}, {admin, adminCredentialID}} {
		code, body := doJSONReq(r, http.MethodGet, "/v3/credentials", "", tc.headers)
		if code != http.StatusOK || (tc.wantID != "" && !strings.Contains(body, tc.wantID)) || (tc.wantID == "" && strings.Contains(body, ownerCredentialID)) {
			t.Errorf("credential list: code=%d body=%s", code, body)
		}
	}
	if code, body := doJSONReq(r, http.MethodGet, "/v3/credentials/"+ownerCredentialID, "", owner); code != http.StatusOK || !strings.Contains(body, "review-secret") {
		t.Errorf("owner get credential: code=%d body=%s", code, body)
	}
	if code, body := doJSONReq(r, http.MethodPatch, "/v3/credentials/"+ownerCredentialID, "{\"credential\":{\"blob\":\"owner-updated\"}}", owner); code != http.StatusOK || !strings.Contains(body, "owner-updated") {
		t.Errorf("owner update credential: code=%d body=%s", code, body)
	}
	if code, body := doJSONReq(r, http.MethodDelete, "/v3/credentials/"+ownerCredentialID, "", owner); code != http.StatusNoContent {
		t.Errorf("owner delete credential: code=%d body=%s", code, body)
	}
	if code, body := doJSONReq(r, http.MethodGet, "/v3/credentials/"+adminCredentialID, "", admin); code != http.StatusOK {
		t.Errorf("admin get credential: code=%d body=%s", code, body)
	}
	if code, body := doJSONReq(r, http.MethodPatch, "/v3/credentials/"+adminCredentialID, "{\"credential\":{\"blob\":\"admin-updated\"}}", admin); code != http.StatusOK || !strings.Contains(body, "admin-updated") {
		t.Errorf("admin update credential: code=%d body=%s", code, body)
	}
	if code, body := doJSONReq(r, http.MethodDelete, "/v3/credentials/"+adminCredentialID, "", admin); code != http.StatusNoContent {
		t.Errorf("admin delete credential: code=%d body=%s", code, body)
	}
}

func TestApplicationCredentialMalformedIDsAreValidatedAfterAuthorization(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()
	memberID := uuid.NewString()
	member := identityUserHeaders(memberID, "member")
	foreignUserID := "00000000-0000-0000-0000-000000000001"
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		path := "/v3/users/" + memberID + "/application_credentials/not-a-uuid"
		if code, body := doJSONReq(r, method, path, "", member); code != http.StatusNotFound || !strings.Contains(body, "Could not find Application Credential: not-a-uuid.") {
			t.Errorf("self %s malformed credential id: code=%d body=%s; want Keystone 404", method, code, body)
		}
		foreignPath := "/v3/users/" + foreignUserID + "/application_credentials/not-a-uuid"
		if code, body := doJSONReq(r, method, foreignPath, "", member); code != http.StatusForbidden {
			t.Errorf("foreign %s malformed credential id: code=%d body=%s; want authz-first 403", method, code, body)
		}
	}
}

func TestAssignRoleUnknownIDsReturnResourceNotFound(t *testing.T) {
	requireDB(t)
	const projectID = "00000000-0000-0000-0000-000000000002"
	const userID = "00000000-0000-0000-0000-000000000001"
	const roleID = "00000000-0000-0000-0000-000000000003"
	unknownID := uuid.NewString()
	admin := identityUserHeaders(uuid.NewString(), "admin")
	r := newIdentityTestRouter()
	for _, tc := range []struct{ kind, path string }{
		{"project", "/v3/projects/" + unknownID + "/users/" + userID + "/roles/" + roleID},
		{"user", "/v3/projects/" + projectID + "/users/" + unknownID + "/roles/" + roleID},
		{"role", "/v3/projects/" + projectID + "/users/" + userID + "/roles/" + unknownID},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			code, body := doJSONReq(r, http.MethodPut, tc.path, "", admin)
			if code != http.StatusNotFound || !strings.Contains(body, "Could not find "+tc.kind+": "+unknownID+".") {
				t.Errorf("unknown %s: code=%d body=%s; want resource-specific 404", tc.kind, code, body)
			}
		})
	}
}
