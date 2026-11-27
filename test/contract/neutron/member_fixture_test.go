package neutron_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2: a member of a project of their own, for the tests that need a caller without the
// administrator role (T080, T103).

// keystoneJSON sends a request to Keystone with the token and returns the status and the decoded body.
func keystoneJSON(t *testing.T, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, authURL+path, reader)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// memberNetworkClient creates a project and a user holding the member role in it, and returns a
// Networking client scoped to that project, with the project's id. The user and the project are
// deleted when the test ends.
func memberNetworkClient(t *testing.T) (*gophercloud.ServiceClient, string) {
	t.Helper()
	admin := contract.SetupOpenStackClient(t)
	token := admin.TokenID
	tag := uuid.NewString()[:8]

	code, out := keystoneJSON(t, token, http.MethodPost, "/projects",
		map[string]any{"project": map[string]any{"name": "contract-member-" + tag, "domain_id": "default"}})
	require.Equal(t, http.StatusCreated, code, "create the member's project: %v", out)
	projectID := out["project"].(map[string]any)["id"].(string)
	t.Cleanup(func() { keystoneJSON(t, token, http.MethodDelete, "/projects/"+projectID, nil) })

	name, password := "contract-member-"+tag, "contract-member-"+tag
	code, out = keystoneJSON(t, token, http.MethodPost, "/users",
		map[string]any{"user": map[string]any{"name": name, "password": password, "domain_id": "default"}})
	require.Equal(t, http.StatusCreated, code, "create the member: %v", out)
	userID := out["user"].(map[string]any)["id"].(string)
	t.Cleanup(func() { keystoneJSON(t, token, http.MethodDelete, "/users/"+userID, nil) })

	code, out = keystoneJSON(t, token, http.MethodGet, "/roles", nil)
	require.Equal(t, http.StatusOK, code)
	var memberRole string
	for _, r := range out["roles"].([]any) {
		role := r.(map[string]any)
		if role["name"] == "member" {
			memberRole = role["id"].(string)
		}
	}
	require.NotEmpty(t, memberRole, "the member role")
	code, _ = keystoneJSON(t, token, http.MethodPut, "/projects/"+projectID+"/users/"+userID+"/roles/"+memberRole, nil)
	require.Equal(t, http.StatusNoContent, code, "grant the member role")

	provider, err := openstack.AuthenticatedClient(gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         name,
		Password:         password,
		DomainName:       domainName,
		Scope:            &gophercloud.AuthScope{ProjectID: projectID},
	})
	require.NoError(t, err, "authenticate as the member")
	client, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	return client, projectID
}

// neutronRaw sends a Networking API request with the client's token and returns the status and the
// NeutronError type of an error body ("" for a success).
func neutronRaw(t *testing.T, client *gophercloud.ServiceClient, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, client.ServiceURL(path), reader)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		NeutronError struct {
			Type string `json:"type"`
		} `json:"NeutronError"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.NeutronError.Type
}
