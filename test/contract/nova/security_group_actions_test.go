package nova_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
)

// newTestSecurityGroup creates a security group of the test's own and returns its name.
//
// Not "default": a server created without security groups is given "default", as
// upstream Nova does, so adding it again is a duplicate rather than an add — the tests
// below used to assume a new server had none. A unique name keeps a group left over by
// an earlier run from making the name ambiguous. The group is deleted after the test,
// once the server's own deferred delete has released it.
func newTestSecurityGroup(t *testing.T) string {
	t.Helper()
	netClient := contract.SetupNeutronClient(t)
	name := "contract-sg-" + uuid.NewString()[:8]
	sg, err := groups.Create(netClient, groups.CreateOpts{Name: name}).Extract()
	require.NoError(t, err, "creating the test security group")
	t.Cleanup(func() { _ = groups.Delete(netClient, sg.ID).ExtractErr() })
	return name
}

// TestNovaAddSecurityGroup_Contract tests POST /v2.1/servers/:id/action (addSecurityGroup)
//
// A new server already carries "default" (assigned at create when none is requested,
// as upstream does), so the group added here is one the test creates.
func TestNovaAddSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)
	sgName := newTestSecurityGroup(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-action-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Add security group
	actionBody := `{"addSecurityGroup":{"name":"` + sgName + `"}}`

	url := client.ServiceURL("servers", server.ID, "action")
	req, err := http.NewRequest("POST", url, strings.NewReader(actionBody))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// TestNovaAddSecurityGroupDuplicate_Contract tests adding a SG the server already has returns 400
//
// Upstream answers 400, not 409: Nova appends the group's id to the port's list, Neutron
// rejects the duplicate ("Duplicate items in the list") and Nova maps that to
// SecurityGroupCannotBeApplied. A new server already carries "default", so adding it
// once is the duplicate.
func TestNovaAddSecurityGroupDuplicate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-duplicate-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Add "default", which the server already has - should get 400
	actionBody := `{
		"addSecurityGroup": {
			"name": "default"
		}
	}`

	url := client.ServiceURL("servers", server.ID, "action")
	req, err := http.NewRequest("POST", url, strings.NewReader(actionBody))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestNovaRemoveSecurityGroup_Contract tests POST /v2.1/servers/:id/action (removeSecurityGroup)
//
// Two security groups must be attached so the documented "cannot remove last
// security group" guard (advanced_actions.go) does not fire. The server gets
// "default" at create; the second is created here, per the explicit guidance in
// migrations/058_seed_test_security_group.up.sql:
//
//	"Security group data for contract tests should be created by the test setup code."
func TestNovaRemoveSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)
	sgName := newTestSecurityGroup(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-remove-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	url := client.ServiceURL("servers", server.ID, "action")

	// Add the test's own group — "default" is already there from create.
	addBody := `{"addSecurityGroup":{"name":"` + sgName + `"}}`
	req, _ := http.NewRequest("POST", url, strings.NewReader(addBody))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "addSecurityGroup %s should return 202", sgName)

	// Remove "default" — the test's group remains, last-SG guard should not fire.
	removeBody := `{"removeSecurityGroup":{"name":"default"}}`
	req, err = http.NewRequest("POST", url, strings.NewReader(removeBody))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// TestNovaChangePassword_Contract tests POST /v2.1/servers/:id/action (changePassword)
func TestNovaChangePassword_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "password-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Wait for server to be ACTIVE (needed for password change)
	for i := 0; i < 30; i++ {
		s, _ := servers.Get(client, server.ID).Extract()
		if s.Status == "ACTIVE" {
			break
		}
		if i == 29 {
			t.Skip("Server did not become ACTIVE in time")
		}
		time.Sleep(time.Second)
	}

	// Change admin password
	actionBody := `{
		"changePassword": {
			"adminPass": "newSecurePassword123"
		}
	}`

	url := client.ServiceURL("servers", server.ID, "action")
	req, err := http.NewRequest("POST", url, strings.NewReader(actionBody))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Verify response contains no error
	respBody, _ := io.ReadAll(resp.Body)
	if len(respBody) > 0 {
		var result map[string]interface{}
		json.Unmarshal(respBody, &result)
		assert.Nil(t, result["error"])
	}
}
