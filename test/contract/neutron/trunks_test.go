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
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTrunkNetwork creates the network the test's ports live on. The database seeds none, and
// relying on one left behind by an earlier test fails as soon as that test cleans up after itself.
func createTrunkNetwork(t *testing.T, client *gophercloud.ServiceClient) string {
	t.Helper()
	network, err := networks.Create(client, networks.CreateOpts{
		Name:         "trunk-test-network",
		AdminStateUp: gophercloud.Enabled,
	}).Extract()
	require.NoError(t, err, "Failed to create network")
	t.Cleanup(func() { networks.Delete(client, network.ID) })
	return network.ID
}

// Helper to create test port in database (bypasses bridge issues). A failed insert fails the test
// here rather than surfacing later as a trunk that cannot reference its port.
func createDBPort(t *testing.T, networkID, name string) string {
	t.Helper()
	portID := uuid.New().String()
	contract.Seed(t,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		 VALUES ($1, $2, $3, (SELECT id FROM projects WHERE name = 'default' LIMIT 1), '00:00:00:00:00:00', true, 'ACTIVE', '[]', NOW(), NOW())`,
		portID, name, networkID)
	return portID
}

// TestTrunkBasicCRUD tests trunk CRUD operations
func TestTrunkBasicCRUD(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNeutronClient(t)

	portID := createDBPort(t, createTrunkNetwork(t, client), "test-trunk-port")

	// CREATE
	createReq := map[string]interface{}{
		"trunk": map[string]interface{}{
			"name":    "test-trunk",
			"port_id": portID,
		},
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", client.ServiceURL("trunks"), bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var createResult struct {
		Trunk map[string]interface{} `json:"trunk"`
	}
	json.Unmarshal(respBody, &createResult)
	trunkID := createResult.Trunk["id"].(string)
	assert.NotEmpty(t, trunkID)

	// LIST
	req, _ = http.NewRequest("GET", client.ServiceURL("trunks"), nil)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// GET
	req, _ = http.NewRequest("GET", client.ServiceURL("trunks", trunkID), nil)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// UPDATE
	updateReq := map[string]interface{}{
		"trunk": map[string]interface{}{
			"name": "updated-trunk",
		},
	}
	body, _ = json.Marshal(updateReq)
	req, _ = http.NewRequest("PUT", client.ServiceURL("trunks", trunkID), bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// DELETE
	req, _ = http.NewRequest("DELETE", client.ServiceURL("trunks", trunkID), nil)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Cleanup
	contract.Seed(t, "DELETE FROM ports WHERE id = $1", portID)
}

// TestTrunkSubports tests adding subports
func TestTrunkSubports(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNeutronClient(t)

	networkID := createTrunkNetwork(t, client)
	parentPort := createDBPort(t, networkID, "trunk-parent")
	subPort := createDBPort(t, networkID, "trunk-sub")

	// Create trunk
	createReq := map[string]interface{}{
		"trunk": map[string]interface{}{
			"name":    "test-trunk-subport",
			"port_id": parentPort,
		},
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", client.ServiceURL("trunks"), bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var createResult struct {
		Trunk map[string]interface{} `json:"trunk"`
	}
	json.Unmarshal(respBody, &createResult)
	trunkID := createResult.Trunk["id"].(string)

	// Add subport
	addReq := map[string]interface{}{
		"sub_ports": []map[string]interface{}{
			{
				"port_id":           subPort,
				"segmentation_type": "vlan",
				"segmentation_id":   100,
			},
		},
	}
	body, _ = json.Marshal(addReq)
	req, _ = http.NewRequest("PUT", client.ServiceURL("trunks", trunkID, "add_subports"), bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Cleanup
	http.NewRequest("DELETE", client.ServiceURL("trunks", trunkID), nil)
	contract.Seed(t, "DELETE FROM ports WHERE id IN ($1, $2)", parentPort, subPort)
}
