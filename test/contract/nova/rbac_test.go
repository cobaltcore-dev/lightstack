package nova_test

// RBAC contract tests, originally T048 of SPEC-002.
//
// This file was disabled as `rbac_test.go.broken` and explicitly excluded from
// coverage accounting. The cause was a gophercloud API drift, not a design
// problem: v1.14.1's ResetState takes a bare ServerState, not the ResetStateOpts
// struct this was written against.
//
// SPEC-023 revives it because it already tests reset-state and evacuate — the
// exact two operations FR-008a repairs. Both gated on the `admin` role and then
// scoped their lookup to the ADMINISTRATOR's own project, so an administrator
// was answered 404 for precisely the instances those operations exist to handle.
//
// Note what this file does NOT prove on its own: its administrator creates the
// server itself, so the admin path exercises the administrator's own project and
// passed even before the fix. The cross-project case is covered by
// internal/nova/authz_test.go, which can seed a second project.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/resetstate"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

const (
	authURL     = "http://localhost:35357/v3"
	adminUser   = "admin"
	adminPass   = "secret"
	projectName = "default"
	domainName  = "Default"
)

func setupAdminClient(t *testing.T) (*gophercloud.ProviderClient, *gophercloud.ServiceClient) {
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         adminUser,
		Password:         adminPass,
		TenantName:       projectName,
		DomainName:       domainName,
		AllowReauth:      true,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	require.NoError(t, err, "Failed to authenticate as admin")

	client, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Nova client")

	return provider, client
}

func setupNonAdminClient(t *testing.T, username, password string) (*gophercloud.ProviderClient, *gophercloud.ServiceClient) {
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         username,
		Password:         password,
		TenantName:       projectName,
		DomainName:       domainName,
		AllowReauth:      true,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	require.NoError(t, err, "Failed to authenticate as non-admin")

	client, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Nova client")

	return provider, client
}

// TestAdminOnlyResetState_Contract verifies os-resetState requires admin role
func TestAdminOnlyResetState_Contract(t *testing.T) {
	adminProvider, adminClient := setupAdminClient(t)

	// Create test server as admin
	server, err := servers.Create(adminClient, servers.CreateOpts{
		Name: "rbac-test-server",
		// The seeded m1.tiny, by id — the same reference every other server
		// action test uses. "1" and a name are not resolvable here.
		FlavorRef: "00000000-0000-0000-0000-000000000010",
	}).Extract()
	require.NoError(t, err, "Failed to create test server")
	defer servers.Delete(adminClient, server.ID)

	// Test 1: Admin can reset state
	err = resetstate.ResetState(adminClient, server.ID, resetstate.StateError).ExtractErr()

	assert.NoError(t, err, "Admin should be able to reset state")
	t.Log("✓ Admin successfully reset server state")

	// Test 2: Create non-admin user and verify they cannot reset state
	identityClient, err := openstack.NewIdentityV3(adminProvider, gophercloud.EndpointOpts{})
	require.NoError(t, err)

	// Create a GENUINELY non-administrative user: created AND granted the member
	// role on the project. Creating the user alone is not enough — a user with no
	// role on the project cannot obtain a project-scoped token at all, so the
	// check below would fail to authenticate rather than be forbidden, and a test
	// that skips there proves nothing.
	//
	// The name is this run's own: a fixed one collides with a user an interrupted
	// run left behind, and every later run then fails to create it. A failure here
	// fails the test rather than skipping it — a skip silently drops the
	// authorization check below.
	testUserName := "rbac-test-user-" + uuid.NewString()[:8]
	testUser, err := users.Create(identityClient, users.CreateOpts{
		Name:     testUserName,
		DomainID: "default",
		Password: "testpass123",
		Enabled:  gophercloud.Enabled,
	}).Extract()
	require.NoError(t, err, "creating the non-administrative test user")
	defer users.Delete(identityClient, testUser.ID)

	projectID := contract.GetProjectIDFromToken(t, identityClient)
	memberRoleID := resolveRoleIDByName(t, adminProvider, "member")

	grantURL := fmt.Sprintf("%s/projects/%s/users/%s/roles/%s",
		authURL, projectID, testUser.ID, memberRoleID)
	grantReq, err := http.NewRequest(http.MethodPut, grantURL, nil)
	require.NoError(t, err)
	grantReq.Header.Set("X-Auth-Token", adminProvider.TokenID)
	grantResp, err := http.DefaultClient.Do(grantReq)
	require.NoError(t, err)
	grantResp.Body.Close()
	require.Equal(t, http.StatusNoContent, grantResp.StatusCode, "granting the member role")

	// Authenticate as non-admin user
	_, nonAdminClient := setupNonAdminClient(t, testUserName, "testpass123")

	// The whole point: this token must NOT carry admin, or the check below is a
	// tautology — the exact trap #545 found in the multi-tenancy suite, where
	// both "unprivileged" users silently held the administrative role.
	requireTokenLacksAdmin(t, nonAdminClient.TokenID)

	// Test 3: Non-admin cannot reset state
	err = resetstate.ResetState(nonAdminClient, server.ID, resetstate.StateActive).ExtractErr()

	// Should get 403 Forbidden
	if err != nil {
		assert.Contains(t, err.Error(), "403", "Non-admin should get 403 Forbidden")
		t.Log("✓ Non-admin correctly denied with 403")
	} else {
		t.Error("✗ Non-admin was able to reset state (should be denied)")
	}
}

// TestAdminOnlyEvacuate_Contract verifies evacuate requires admin role
func TestAdminOnlyEvacuate_Contract(t *testing.T) {
	_, adminClient := setupAdminClient(t)

	// Create test server
	server, err := servers.Create(adminClient, servers.CreateOpts{
		Name:      "evacuate-test-server",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(adminClient, server.ID)

	// gophercloud v1 has no generic servers.Action, so the action body goes over
	// raw HTTP — the same idiom advanced_server_actions_test.go uses.
	url := adminClient.ServiceURL("servers", server.ID, "action")
	req, err := http.NewRequest("POST", url,
		strings.NewReader(`{"evacuate": {"host": "target-host"}}`))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", adminClient.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// May fail because no such host exists, but must never be 403 (the admin
	// gate) or 404 (the SPEC-023 FR-008a defect: gated to admins, then scoped to
	// the admin's own project).
	assert.NotEqual(t, http.StatusForbidden, resp.StatusCode, "admin must not be forbidden")
	assert.NotEqual(t, http.StatusNotFound, resp.StatusCode,
		"admin evacuating an instance must not 404 — that is the pre-SPEC-023 defect")
	t.Logf("admin evacuate attempt: %d", resp.StatusCode)
}

// The three remaining tests from SPEC-002's T048 are deliberately NOT revived:
//
//   - TestQuotaUpdateAdminOnly_Contract called tokens.Get with a ProviderClient
//     where a ServiceClient is required (it never compiled), hardcoded
//     http://localhost:8774, and passed a project NAME as DefaultProjectID. Its
//     assertion — quota writes require admin — is already covered by
//     internal/nova/quotas_test.go TestQuotaWritesRequireAdmin.
//   - TestProjectIsolation_Contract asserted only that an admin sees their OWN
//     server, which passes whether or not isolation works.
//   - TestReadOnlyRoleCannotCreate_Contract needs a `reader` role assignment the
//     harness cannot make.
//
// Cross-project isolation and the administrative override are covered properly
// by internal/nova/authz_test.go, which can seed a second project and a
// genuinely non-administrative caller. Reviving broken tests to raise a count
// would be worse than leaving them out.

// resolveRoleIDByName resolves a role by name SERVER-side. Before SPEC-023 the
// name filter was discarded and this would have returned whichever role sorts
// first — "admin" — which is precisely how the multi-tenancy suite came to grant
// administrative rights to both of its supposedly unprivileged test users.
func resolveRoleIDByName(t *testing.T, provider *gophercloud.ProviderClient, name string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, authURL+"/roles?name="+name, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", provider.TokenID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var body struct {
		Roles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"roles"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Roles, 1, "?name=%s must return exactly one role", name)
	require.Equal(t, name, body.Roles[0].Name)
	return body.Roles[0].ID
}

// requireTokenLacksAdmin decodes the JWT payload and fails if it carries the
// administrative role. Without this the "non-admin is forbidden" assertion below
// could pass while actually running as an administrator being denied for some
// unrelated reason — or, worse, fail to be denied at all and be read as a bug in
// the code rather than in the fixture.
func requireTokenLacksAdmin(t *testing.T, token string) {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "expected a JWT")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err, "decoding the token payload")

	var claims struct {
		Roles []string `json:"roles"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	for _, r := range claims.Roles {
		require.NotEqual(t, "admin", r,
			"the supposedly non-administrative fixture holds the admin role — every "+
				"authorization assertion using it would be meaningless")
	}
	require.NotEmpty(t, claims.Roles, "the fixture must hold at least one role, or it "+
		"cannot obtain a project-scoped token and the test refuses for the wrong reason")
}
