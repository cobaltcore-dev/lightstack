package keystone_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-014 FR-005/FR-007 (authorization): the EC2-credentials API is scoped to a
// user path (/v3/users/:id/credentials/OS-EC2) and returns the *secret*. A caller
// must therefore only manage its OWN credentials unless it is admin — otherwise any
// authenticated tenant could read or mint another user's S3 secret and reach that
// tenant's RGW namespace (cross-tenant breach).
//
// Per Constitution Art III this test is RED before the self-or-admin guard is added
// to the handlers (o3k currently returns 200/201 to the cross-user calls) and GREEN
// after. It mirrors the guard o3k already uses in ChangePassword (handlers.go).
func TestKeystoneEC2Credential_CrossUserForbidden_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupKeystoneClient(t)

	const adminUID = "00000000-0000-0000-0000-000000000001"  // seeded admin
	const projectID = "00000000-0000-0000-0000-000000000002" // seeded default project

	// A non-admin (member) user + their scoped token.
	bobID, bobToken := createMemberUser(t, client, projectID, "authz-bob", "Bob-Test-1")
	defer deleteUser(t, client, bobID)

	adminEC2 := client.ServiceURL("users", adminUID, "credentials", "OS-EC2")
	mintBody, _ := json.Marshal(map[string]any{"tenant_id": projectID})

	// (1) A non-admin must NOT mint an EC2 credential for another user.
	code, body := doJSON(t, "POST", adminEC2, bobToken, mintBody)
	assert.Equal(t, http.StatusForbidden, code,
		"non-admin minting EC2 creds for another user must be 403; got %d: %s", code, body)

	// (2) A non-admin must NOT list/read another user's EC2 credentials (secret leaks).
	code, body = doJSON(t, "GET", adminEC2, bobToken, nil)
	assert.Equal(t, http.StatusForbidden, code,
		"non-admin reading another user's EC2 creds must be 403; got %d: %s", code, body)

	// (3) Self-service must still work after the fix: bob mints for himself.
	bobEC2 := client.ServiceURL("users", bobID, "credentials", "OS-EC2")
	code, body = doJSON(t, "POST", bobEC2, bobToken, mintBody)
	require.Equalf(t, http.StatusCreated, code, "self-service create must work; got %d: %s", code, body)
	var created struct {
		Credential struct {
			Access string `json:"access"`
		} `json:"credential"`
	}
	_ = json.Unmarshal(body, &created)
	if created.Credential.Access != "" {
		doJSON(t, "DELETE", client.ServiceURL("users", bobID, "credentials", "OS-EC2", created.Credential.Access), bobToken, nil)
	}
}

// doJSON sends an authenticated request (JSON body optional) and returns the
// status code and response body.
func doJSON(t *testing.T, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// createMemberUser creates a user with the `member` role in projectID and returns
// its id plus a project-scoped token. Uses IDs throughout (o3k's GET-by-name 500s).
func createMemberUser(t *testing.T, client *gophercloud.ServiceClient, projectID, name, password string) (string, string) {
	t.Helper()

	// Domain to create the user in — read it off the seeded admin user.
	_, ab := doJSON(t, "GET", client.ServiceURL("users", "00000000-0000-0000-0000-000000000001"), client.TokenID, nil)
	var admin struct {
		User struct {
			DomainID string `json:"domain_id"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(ab, &admin))
	require.NotEmpty(t, admin.User.DomainID, "admin domain_id")

	// Create the user.
	ub, _ := json.Marshal(map[string]any{"user": map[string]any{
		"name": name, "password": password, "domain_id": admin.User.DomainID, "default_project_id": projectID,
	}})
	code, cb := doJSON(t, "POST", client.ServiceURL("users"), client.TokenID, ub)
	require.Equalf(t, http.StatusCreated, code, "create user: %s", cb)
	var cu struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(cb, &cu))
	require.NotEmpty(t, cu.User.ID)

	// Resolve the member role id and assign it in the project.
	_, rb := doJSON(t, "GET", client.ServiceURL("roles"), client.TokenID, nil)
	var rl struct {
		Roles []struct{ ID, Name string } `json:"roles"`
	}
	require.NoError(t, json.Unmarshal(rb, &rl))
	var memberID string
	for _, r := range rl.Roles {
		if r.Name == "member" {
			memberID = r.ID
		}
	}
	require.NotEmpty(t, memberID, "member role must exist")
	acode, ares := doJSON(t, "PUT", client.ServiceURL("projects", projectID, "users", cu.User.ID, "roles", memberID), client.TokenID, nil)
	require.Truef(t, acode == http.StatusNoContent || acode == http.StatusOK, "assign role: %d %s", acode, ares)

	// Authenticate as the new user, scoped to the project, and return the token.
	ath, _ := json.Marshal(map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{
			"user": map[string]any{"name": name, "domain": map[string]any{"id": admin.User.DomainID}, "password": password}}},
		"scope": map[string]any{"project": map[string]any{"id": projectID}},
	}})
	req, err := http.NewRequest("POST", client.ServiceURL("auth", "tokens"), bytes.NewReader(ath))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "authenticate as new user")
	token := resp.Header.Get("X-Subject-Token")
	require.NotEmpty(t, token, "new user's token")
	return cu.User.ID, token
}

// deleteUser removes a user (best-effort cleanup).
func deleteUser(t *testing.T, client *gophercloud.ServiceClient, userID string) {
	t.Helper()
	doJSON(t, "DELETE", client.ServiceURL("users", userID), client.TokenID, nil)
}
