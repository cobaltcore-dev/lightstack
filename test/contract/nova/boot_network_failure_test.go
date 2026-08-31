package nova_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// SPEC-025 US3 (cloud-storage#547 B13): a server naming a network it cannot use must be
// refused before anything is created.
//
// The client-visible half of the story. The asynchronous half — an allocation that fails
// after the create was already answered — is covered by unit tests, because inducing it
// through the API requires control over the network service's internals.

// createServerRaw posts a server-create body and returns the status and response.
func createServerRaw(t *testing.T, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, novaRootURL(t)+"/v2.1/servers", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// firstImageID returns an image the create path will accept, listed from the image endpoint
// the service catalog advertises rather than an assumed localhost.
func firstImageID(t *testing.T, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, contract.SetupImageV2Client(t).ServiceURL("images"), nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Images []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"images"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	for _, img := range out.Images {
		if img.Status == "active" {
			return img.ID
		}
	}
	t.Skip("no active image available to boot from")
	return ""
}

// T053 (FR-017, FR-016a): a nonexistent network is refused up front, and the refusal
// names it. Before this the create was accepted and the server reached ACTIVE with no
// interface at all.
func TestNovaCreateRefusesUnknownNetwork_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	image := firstImageID(t, token)

	const ghost = "99999999-9999-9999-9999-999999999999"
	body := `{"server":{"name":"spec025-ghost-net","flavorRef":"m1.small","imageRef":"` + image +
		`","networks":[{"uuid":"` + ghost + `"}]}}`

	code, resp := createServerRaw(t, token, body)

	assert.Equal(t, http.StatusBadRequest, code,
		"a create naming an unknown network must be refused before anything is created; body: %s", resp)
	assert.Contains(t, resp, ghost, "the refusal should name the offending network")

	// Nothing may have been created. A refused request that leaves a row behind is
	// the same fail-open in a different place.
	if code != http.StatusBadRequest {
		var created struct {
			Server struct {
				ID string `json:"id"`
			} `json:"server"`
		}
		if json.Unmarshal([]byte(resp), &created) == nil && created.Server.ID != "" {
			req, _ := http.NewRequest(http.MethodDelete, novaRootURL(t)+"/v2.1/servers/"+created.Server.ID, nil)
			req.Header.Set("X-Auth-Token", token)
			if r, err := http.DefaultClient.Do(req); err == nil {
				_ = r.Body.Close()
			}
		}
	}
}

// T054 (FR-017): the same refusal for a network that exists but belongs to another
// project. Deliberately indistinguishable from "absent" — saying which it is would
// confirm another project's network exists.
//
// The network must really exist and really belong to someone else. An id that names
// nothing is refused whether or not the ownership check is there, so it cannot tell a
// regression that lets any existing network through from the correct answer. And the
// caller must not be an administrator, who meets a different rule for some networks.
func TestNovaCreateRefusesForeignNetwork_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	adminProvider, _ := setupAdminClient(t)

	// A private network of the administrator's own project...
	foreign := createPrivateNetwork(t, adminProvider, "spec025-foreign-net-"+uuid.NewString()[:8])
	// ...and a caller from another project, holding nothing but the member role there.
	memberToken := memberOfAFreshProject(t, adminProvider)
	requireTokenLacksAdmin(t, memberToken)
	image := firstImageID(t, memberToken)

	body := `{"server":{"name":"spec025-foreign-net","flavorRef":"m1.small","imageRef":"` + image +
		`","networks":[{"uuid":"` + foreign + `"}]}}`

	code, resp := createServerRaw(t, memberToken, body)
	if code != http.StatusBadRequest {
		deleteCreatedServer(t, memberToken, resp)
	}
	assert.Equal(t, http.StatusBadRequest, code,
		"a network this project may not use must be refused; body: %s", resp)
	assert.Contains(t, resp, foreign, "the refusal should name the offending network")

	// The refusal must not reveal whether the network exists elsewhere.
	lowered := strings.ToLower(resp)
	assert.NotContains(t, lowered, "another project",
		"the refusal must not disclose that the network belongs to someone else")
	assert.NotContains(t, lowered, "forbidden",
		"a foreign network is reported as unavailable, not as forbidden")
}

// deleteCreatedServer removes a server a create that should have been refused made anyway, so
// the regression a test reports does not also leave a server behind.
func deleteCreatedServer(t *testing.T, token, createResponse string) {
	t.Helper()
	var created struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	if json.Unmarshal([]byte(createResponse), &created) != nil || created.Server.ID == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, novaRootURL(t)+"/v2.1/servers/"+created.Server.ID, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	if r, err := http.DefaultClient.Do(req); err == nil {
		_ = r.Body.Close()
	}
}

// createPrivateNetwork creates a network in the provider's project, deleted at the end.
func createPrivateNetwork(t *testing.T, provider *gophercloud.ProviderClient, name string) string {
	t.Helper()
	client, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	network, err := networks.Create(client, networks.CreateOpts{Name: name}).Extract()
	require.NoError(t, err, "creating the network")
	t.Cleanup(func() { _ = networks.Delete(client, network.ID).ExtractErr() })
	return network.ID
}

// memberOfAFreshProject creates a project and a user holding only the member role on it,
// both deleted at the end, and returns a token scoped to that project. The names are
// this run's own, so an interrupted run's leftovers cannot collide with them.
func memberOfAFreshProject(t *testing.T, adminProvider *gophercloud.ProviderClient) string {
	t.Helper()
	identity, err := openstack.NewIdentityV3(adminProvider, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	suffix := uuid.NewString()[:8]

	project, err := projects.Create(identity, projects.CreateOpts{
		Name: "spec025-foreign-" + suffix, DomainID: "default",
	}).Extract()
	require.NoError(t, err, "creating the second project")
	t.Cleanup(func() { _ = projects.Delete(identity, project.ID).ExtractErr() })

	const password = "testpass123"
	user, err := users.Create(identity, users.CreateOpts{
		Name: "spec025-member-" + suffix, DomainID: "default", Password: password, Enabled: gophercloud.Enabled,
	}).Extract()
	require.NoError(t, err, "creating the member user")
	t.Cleanup(func() { _ = users.Delete(identity, user.ID).ExtractErr() })

	grantURL := fmt.Sprintf("%s/projects/%s/users/%s/roles/%s",
		authURL, project.ID, user.ID, resolveRoleIDByName(t, adminProvider, "member"))
	grantReq, err := http.NewRequest(http.MethodPut, grantURL, nil)
	require.NoError(t, err)
	grantReq.Header.Set("X-Auth-Token", adminProvider.TokenID)
	grantResp, err := http.DefaultClient.Do(grantReq)
	require.NoError(t, err)
	_ = grantResp.Body.Close()
	require.Equal(t, http.StatusNoContent, grantResp.StatusCode, "granting the member role")

	member, err := openstack.AuthenticatedClient(gophercloud.AuthOptions{
		IdentityEndpoint: authURL, Username: user.Name, Password: password,
		TenantID: project.ID, DomainName: domainName,
	})
	require.NoError(t, err, "authenticating as the member")
	return member.TokenID
}
