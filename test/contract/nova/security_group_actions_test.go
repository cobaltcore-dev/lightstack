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
)

// TestNovaAddSecurityGroup_Contract tests POST /v2.1/servers/:id/action (addSecurityGroup)
func TestNovaAddSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-action-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Add security group
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

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// TestNovaAddSecurityGroupDuplicate_Contract tests adding same SG twice returns 409
func TestNovaAddSecurityGroupDuplicate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-duplicate-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Add security group first time
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
	resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Try to add same security group again - should get 409
	req, err = http.NewRequest("POST", url, strings.NewReader(actionBody))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestNovaRemoveSecurityGroup_Contract tests POST /v2.1/servers/:id/action (removeSecurityGroup)
//
// Two security groups must be attached so the documented "cannot remove last
// security group" guard (advanced_actions.go) does not fire. The seed only
// ships "default"; "test-sg" is created here, per the explicit guidance in
// migrations/058_seed_test_security_group.up.sql:
//
//	"Security group data for contract tests should be created by the test setup code."
func TestNovaRemoveSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)
	netClient := contract.SetupNeutronClient(t)

	// Bootstrap the second SG. Declared before the server so defer LIFO runs
	// the server delete first — its cascade frees the port reference to this
	// SG, and only then do we delete the SG itself.
	sg, err := groups.Create(netClient, groups.CreateOpts{Name: "test-sg"}).Extract()
	require.NoError(t, err, "Failed to create test-sg fixture")
	defer groups.Delete(netClient, sg.ID)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "sg-remove-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	url := client.ServiceURL("servers", server.ID, "action")

	// Add "default" — verify success (previously discarded silently).
	addBody := `{"addSecurityGroup":{"name":"default"}}`
	req, _ := http.NewRequest("POST", url, strings.NewReader(addBody))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "addSecurityGroup default should return 202")

	// Add "test-sg" — verify success.
	addBody2 := `{"addSecurityGroup":{"name":"test-sg"}}`
	req, _ = http.NewRequest("POST", url, strings.NewReader(addBody2))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "addSecurityGroup test-sg should return 202")

	// Remove "default" — test-sg remains, last-SG guard should not fire.
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
		ImageRef:  "00000000-0000-0000-0000-000000000001",
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
